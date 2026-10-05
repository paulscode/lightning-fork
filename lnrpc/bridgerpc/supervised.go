//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/lnrpc"
	"gopkg.in/macaroon.v2"
)

// The supervised SHA256 node is a stock lnd the platform starts and this node
// gives an identity to. The division of labour is deliberate:
//
//   - The platform owns the process: it starts it once this node has written
//     the wallet password file, with --wallet-unlock-password-file and
//     --wallet-unlock-allow-create, restarts it if it dies and stops it with
//     the package. Without the file lnd refuses to start, which is what keeps
//     the process from doing anything on a node that never bridges.
//   - This node owns everything that makes it ours: the seed (derived, never
//     stored), the wallet's creation, its password, the macaroon the bridge
//     uses, and the check that the node answering is the one it created.
//
// Every step below is idempotent and runs before each connection attempt, so a
// restart at any point (this node's, the platform's, or the SHA256 node's)
// resumes where it stopped rather than needing to be undone.

// Where the supervised node is, as a stable code for Status and a person.
const (
	sha256Starting       = "starting"
	sha256CreatingWallet = "creating_wallet"
	sha256Locked         = "locked"
	sha256Syncing        = "syncing"
	sha256Ready          = "ready"
	sha256Unreachable    = "unreachable"
	sha256NotOurs        = "not_ours"
	sha256Error          = "error"
)

// errSha256Pending is why the bridge is not up while the supervised node is
// on its way: started, creating its wallet, or catching up. Not a fault, so
// the bridge retries it sooner than a real failure.
var errSha256Pending = errors.New("the SHA256 Lightning node is not ready yet")

// sha256RecoveryWindow is how many addresses a newly created supervised wallet
// looks ahead for.
//
// A new wallet created from a derived seed is not necessarily a new wallet: if
// this node's data was restored and the SHA256 node's was not, the same seed
// is being created again over an existing history. Asking lnd to recover makes
// that case find its on-chain coins, at the cost of a rescan from the fixed
// birthday, which is days of blocks rather than years. Channels are another
// matter: those come back only from that node's channel backup, which
// is restored from once the node runs (see stageRestore).
const sha256RecoveryWindow = 2500

// sha256RestoredBackupName is the copy of the SHA256 node's channel backup
// kept beside its wallet password when its wallet is recreated from it.
const sha256RestoredBackupName = "channel.backup.restored"

// sha256RestoreRetry is how often a restore that has not worked is tried
// again.
const sha256RestoreRetry = time.Minute

// sha256RestoreMaxRetry is the longest wait between restore attempts.
const sha256RestoreMaxRetry = 30 * time.Minute

// sha256ReadyWait bounds how long one attempt waits for a wallet it has just
// created to come up, before handing back to the retry loop.
const sha256ReadyWait = 2 * time.Minute

// sha256StartTimeout is how long a node that has started before (its
// certificate is there) may go without answering before it is reported as
// failing rather than starting.
//
// lnd exits at once on a wrong unlock password or an unreachable chain node,
// and the platform restarts it, so a node in that loop never answers. Without
// a limit that would read as "starting" for ever.
const sha256StartTimeout = 5 * time.Minute

// bridgeMacaroonPermissions is everything the bridge calls on the SHA256 node,
// method by method, and nothing else.
//
// Per method rather than per entity, because entities are coarse: offchain
// write alone would also allow closing channels and changing their policy.
// The list is what remote.go and the status summary call. A macaroon baked
// from an earlier list is baked again (see macaroonCurrent), so a release that
// adds a method here does not leave upgraded nodes failing that call.
var bridgeMacaroonPermissions = []string{
	"/lnrpc.Lightning/GetInfo",
	"/lnrpc.Lightning/ListChannels",
	"/lnrpc.Lightning/DecodePayReq",
	"/lnrpc.Lightning/QueryRoutes",
	"/lnrpc.Lightning/DeleteCanceledInvoice",
	"/lnrpc.Lightning/WalletBalance",
	"/lnrpc.Lightning/ChannelBalance",
	"/invoicesrpc.Invoices/AddHoldInvoice",
	"/invoicesrpc.Invoices/CancelInvoice",
	"/invoicesrpc.Invoices/SettleInvoice",
	"/invoicesrpc.Invoices/LookupInvoiceV2",
	"/routerrpc.Router/SendPaymentV2",
	"/routerrpc.Router/TrackPaymentV2",
	"/chainrpc.ChainKit/GetBlockHash",
	"/chainrpc.ChainKit/GetBlockHeader",
}

// unlockerClients is what preparing the node needs from it before any
// macaroon exists. An interface so tests can stand in for a node.
type unlockerClients interface {
	GetState(ctx context.Context) (lnrpc.WalletState, error)
	InitWallet(ctx context.Context, req *lnrpc.InitWalletRequest) error
	UnlockWallet(ctx context.Context, password []byte) error
	Close() error
}

// adminClients is what preparing the node needs once its admin macaroon
// exists: baking the bridge's narrower one, and reading who it is.
type adminClients interface {
	BakeMacaroon(ctx context.Context, perms []string) ([]byte, error)
	IdentityPubkey(ctx context.Context) (string, error)
	RestoreChannelBackups(ctx context.Context, multi []byte) error
	Close() error
}

// supervisor prepares the supervised SHA256 node and reports where it is.
type supervisor struct {
	cfg *Config

	// derive is the node's seed entropy; see Deps.DeriveSha256Seed.
	derive func() ([aezeed.EntropySize]byte, error)

	// coinType is what lnd derives the SHA256 node's keys under: 0 on
	// mainnet and 1 on the test networks.
	coinType uint32

	// dialUnlocker and dialAdmin reach the node. Replaced in tests.
	dialUnlocker func(cfg *Config) (unlockerClients, error)
	dialAdmin    func(cfg *Config, macaroon []byte) (adminClients, error)

	// now is replaced in tests.
	now func() time.Time

	// unansweredSince is when a node that has started before was first
	// found not answering, or zero. Only prepare touches it.
	unansweredSince time.Time

	mu     sync.Mutex
	state  string
	detail string

	// restoreNote says why channels staged for restoring are not restored
	// yet, or is empty.
	restoreNote string
	restoring   bool
}

// newSupervisor is a supervisor for cfg, which must be supervised.
func newSupervisor(cfg *Config) *supervisor {
	s := &supervisor{
		cfg:          cfg,
		dialUnlocker: dialUnlocker,
		dialAdmin:    dialAdmin,
		now:          time.Now,
		state:        sha256Starting,
		detail: "waiting for the SHA256 Lightning node to start; " +
			"the platform starts it once the bridge is on",
	}
	if cfg.Deps != nil {
		s.derive = cfg.Deps.DeriveSha256Seed
		s.coinType = coinTypeFor(cfg.Deps.Network)
	}

	return s
}

// coinTypeFor is the BIP44 coin type lnd uses on a network, as GetInfo names
// it: 0 on mainnet, 1 everywhere else.
func coinTypeFor(network string) uint32 {
	if network == "mainnet" {
		return 0
	}

	return 1
}

// report is where the node is, for Status.
func (s *supervisor) report() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state, s.detail
}

// record notes where the node is, for Status.
func (s *supervisor) record(state, detail string) {
	s.mu.Lock()
	s.state, s.detail = state, detail
	s.mu.Unlock()
}

// pending records a state the node is on its way through and returns the
// error that has the bridge try again soon: not a fault.
func (s *supervisor) pending(state, detail string) error {
	s.record(state, detail)

	return fmt.Errorf("%w: %s", errSha256Pending, detail)
}

// fail records a state that needs something done and returns an error saying
// what, which the bridge reports and retries at its ordinary pace.
func (s *supervisor) fail(state, detail string, cause error) error {
	s.record(state, detail)
	if cause != nil {
		return fmt.Errorf("%s: %w", detail, cause)
	}

	return errors.New(detail)
}

// prepare brings the supervised node to the point where the bridge can use it:
// a wallet created from the derived seed, unlocked, a bridge macaroon baked,
// and its identity checked. It returns nil once all of that holds, and an
// error saying where it stopped otherwise: errSha256Pending while the node is
// on its way, anything else when it needs something done.
func (s *supervisor) prepare(ctx context.Context) error {
	if s.derive == nil {
		return s.fail(sha256Error, "this node cannot derive a seed for "+
			"the SHA256 node (no wallet keys were handed to the "+
			"bridge)", nil)
	}

	password, err := s.ensurePassword()
	if err != nil {
		return err
	}

	// The certificate is the first thing lnd writes, so its absence
	// means the process has never started: the platform starts it once the
	// password file exists, which the step above has just made sure of.
	if _, err := os.Stat(s.cfg.SHA256TLSCertPath); err != nil {
		return s.pending(sha256Starting, fmt.Sprintf("waiting for the "+
			"SHA256 Lightning node to start (it writes %s when it "+
			"does); the platform starts it once the bridge is on, "+
			"so if this lasts, look at that node's log",
			s.cfg.SHA256TLSCertPath))
	}

	if err := s.ensureWallet(ctx, password); err != nil {
		return err
	}

	entropy, err := s.derive()
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node's seed could not "+
			"be derived", err)
	}
	if err := s.ensureMacaroon(ctx); err != nil {
		return err
	}
	if err := s.checkIdentity(ctx, entropy); err != nil {
		return err
	}
	s.ensureOperatorMacaroon(ctx)
	s.record(sha256Ready, "ready")

	return nil
}

// walletDB is where the supervised node keeps its wallet, beside its admin
// macaroon.
func (s *supervisor) walletDB() string {
	return filepath.Join(
		filepath.Dir(s.cfg.SHA256AdminMacaroonPath), "wallet.db",
	)
}

// ensurePassword returns the wallet password, creating it the first time.
//
// Random rather than derived: it protects the wallet file at rest, which is
// only worth anything if it is not computable from something else on the same
// disk. It is never the way back in (the seed is).
//
// It is only ever created for a node with no wallet. A node that has a wallet
// and no password here lost it with this node's data, and a new one would not
// open that wallet: lnd would exit on it at every start. That is said instead,
// with the way out.
func (s *supervisor) ensurePassword() ([]byte, error) {
	path := s.cfg.SHA256PasswordFile
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		pw := []byte(strings.TrimRight(string(raw), "\r\n"))
		if len(pw) >= 8 {
			return pw, nil
		}

		return nil, s.fail(sha256Error, fmt.Sprintf("%s holds no "+
			"usable password; remove it only if the SHA256 node "+
			"has no wallet yet", path), nil)

	case !errors.Is(err, os.ErrNotExist):
		return nil, s.fail(sha256Error, "the SHA256 node's wallet "+
			"password cannot be read", err)
	}

	if _, err := os.Stat(s.walletDB()); err == nil {
		return nil, s.fail(sha256Locked, fmt.Sprintf("the SHA256 node "+
			"has a wallet but the password this node kept for it "+
			"(%s) is gone. Nothing is lost: its seed comes from this "+
			"node's own. Move %s aside, keeping its channel.backup, "+
			"and the bridge creates the node again from the same "+
			"seed; then restore its channels from that backup "+
			"(docs/bridge-sha256-node.md)", path, s.cfg.SHA256Dir),
			nil)
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, s.fail(sha256Error, "no randomness for the SHA256 "+
			"node's wallet password", err)
	}
	pw := []byte(hex.EncodeToString(buf[:]))

	if err := writeFileAtomic(path, pw); err != nil {
		return nil, s.fail(sha256Error, "the SHA256 node's wallet "+
			"password could not be saved", err)
	}

	return pw, nil
}

// ensureWallet creates the node's wallet from the derived seed if it has none,
// unlocks it if it is locked, and waits for it to come up.
func (s *supervisor) ensureWallet(ctx context.Context, password []byte) error {
	conn, err := s.dialUnlocker(s.cfg)
	if err != nil {
		return s.notAnswering(err)
	}
	defer conn.Close()

	state, err := conn.GetState(ctx)
	if err != nil {
		return s.notAnswering(err)
	}
	s.unansweredSince = time.Time{}

	created := false
	switch state {
	case lnrpc.WalletState_WAITING_TO_START:
		return s.pending(sha256Starting, "the SHA256 Lightning node "+
			"is starting")

	case lnrpc.WalletState_NON_EXISTING:
		if err := s.createWallet(ctx, conn, password); err != nil {
			return err
		}
		created = true

	case lnrpc.WalletState_LOCKED:
		// The platform starts the node with the password file, so it
		// unlocks itself. Being found locked means it was started
		// without it; unlocking here gets the bridge going, and the
		// wrong password is reported rather than retried for ever.
		if err := conn.UnlockWallet(ctx, password); err != nil {
			return s.fail(sha256Locked, "the SHA256 node's wallet "+
				"is locked and the password this node keeps "+
				"for it does not open it; this node did not "+
				"create that wallet", err)
		}
	}

	return s.waitActive(ctx, conn, created)
}

// notAnswering reports a node that has started before and does not answer:
// starting, for a while, and then failing.
func (s *supervisor) notAnswering(cause error) error {
	now := s.now()
	if s.unansweredSince.IsZero() {
		s.unansweredSince = now
	}
	if now.Sub(s.unansweredSince) < sha256StartTimeout {
		return s.pending(sha256Starting, "the SHA256 Lightning node "+
			"is starting")
	}

	return s.fail(sha256Unreachable, fmt.Sprintf("the SHA256 Lightning "+
		"node has started before (its certificate is at %s) but has "+
		"not answered at %s for %v. It may be failing to start: look "+
		"at its log. The usual causes are its chain node being "+
		"unreachable and a wallet password that does not open its "+
		"wallet", s.cfg.SHA256TLSCertPath, s.cfg.SHA256RPCHost,
		now.Sub(s.unansweredSince).Round(time.Minute)), cause)
}

// createWallet creates the node's wallet from the derived seed.
func (s *supervisor) createWallet(ctx context.Context, conn unlockerClients,
	password []byte) error {

	entropy, err := s.derive()
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node's seed could not "+
			"be derived", err)
	}
	mnemonic, err := Sha256SeedMnemonic(entropy)
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node's seed could not "+
			"be encoded", err)
	}

	staged, err := s.stageRestore()
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node's channel backup "+
			"could not be kept for restoring", err)
	}

	req := &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: mnemonic[:],
		RecoveryWindow:     sha256RecoveryWindow,
	}
	if staged {
		s.record(sha256CreatingWallet, "recreating the SHA256 node's "+
			"wallet from its derived seed; its channels are "+
			"restored from the channel backup it left once it runs")
	} else {
		s.record(sha256CreatingWallet, "creating the SHA256 node's "+
			"wallet from a seed derived from this node's own; "+
			"there is nothing new to write down")
	}

	err = conn.InitWallet(ctx, req)
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node would not create "+
			"its wallet", err)
	}

	if staged {
		log.Infof("Bridge recreated the supervised SHA256 node's " +
			"wallet from the derived seed; its channels are " +
			"restored once it runs")
	} else {
		log.Infof("Bridge created the supervised SHA256 node's " +
			"wallet from the derived seed")
	}

	return nil
}

// stageRestore keeps the channel backup a SHA256 node left behind when its
// wallet is being created again, and reports whether there was one.
//
// That is a restore: a platform backup carries this file and leaves out the
// wallet and channel database (a channel database from the past can broadcast
// an old state and lose the channel), so the node is recreated from the
// derived seed and its channels come back the way any lnd's come back from a
// channel backup. A first run has no such file.
//
// The copy is what restoreChannels restores from, after the bridge has
// checked which chain the node follows. The node rewrites its own
// channel.backup from the channels it knows as soon as it starts, so the copy
// is all there is until the restore is done. A copy whose restore has not
// finished is never replaced.
func (s *supervisor) stageRestore() (bool, error) {
	data, err := os.ReadFile(s.nodeChannelBackup())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case len(data) == 0:
		return false, nil
	}

	kept, done := s.restoredBackup()
	if _, err := os.Stat(kept); err == nil {
		if _, err := os.Stat(done); err != nil {
			// An earlier restore that has not finished: its copy
			// stands.
			return true, nil
		}
	}
	// The new copy first, then the old restore's marker: a crash between
	// the two leaves the new copy marked done, never the old one unmarked
	// in place of the new.
	if err := writeFileAtomic(kept, data); err != nil {
		return false, fmt.Errorf("keeping a copy at %s: %w", kept, err)
	}
	if err := os.Remove(done); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	return true, nil
}

// nodeChannelBackup is the stock node's own channel.backup.
func (s *supervisor) nodeChannelBackup() string {
	return filepath.Join(
		filepath.Dir(s.cfg.SHA256AdminMacaroonPath), "channel.backup",
	)
}

// restoredBackup is the kept copy, and the marker written once its restore
// has finished.
func (s *supervisor) restoredBackup() (string, string) {
	kept := filepath.Join(
		filepath.Dir(s.cfg.SHA256PasswordFile),
		sha256RestoredBackupName,
	)

	return kept, kept + ".done"
}

// restoreChannels restores the channels staged by stageRestore, if any are
// waiting, and marks them done. lnd skips a channel it already has, so this
// is safe to repeat until it has worked; a peer that cannot be reached fails
// it, and it is tried again.
//
// Called only after the bridge's chain check has passed: restoring a channel
// asks its peer to close it, which on the wrong chain would be answered from
// the wrong chain. (On a test network that check cannot always tell, below the
// activation height or without chainrpc, and passes.)
func (s *supervisor) restoreChannels(ctx context.Context) error {
	kept, done := s.restoredBackup()
	if _, err := os.Stat(done); err == nil {
		return nil
	}
	data, err := os.ReadFile(kept)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return s.noteRestore(fmt.Errorf("reading %s: %w", kept, err))
	}

	admin, err := os.ReadFile(s.cfg.SHA256AdminMacaroonPath)
	if err != nil {
		return s.noteRestore(err)
	}
	conn, err := s.dialAdmin(s.cfg, admin)
	if err != nil {
		return s.noteRestore(err)
	}
	defer conn.Close()

	if err := conn.RestoreChannelBackups(ctx, data); err != nil {
		if ctx.Err() != nil {
			return err
		}

		return s.noteRestore(err)
	}
	if err := writeFileAtomic(done, []byte(s.now().UTC().Format(
		time.RFC3339)+"\n")); err != nil {

		return s.noteRestore(err)
	}

	s.mu.Lock()
	s.restoreNote = ""
	s.mu.Unlock()
	log.Infof("Bridge restored the supervised SHA256 node's channels " +
		"from its channel backup; their peers close them and the " +
		"funds return to its wallet")

	return nil
}

func (s *supervisor) noteRestore(err error) error {
	note := "restoring its channels from its channel backup has not " +
		"worked yet (" + err.Error() + "); trying again"
	if restoreHopeless(err) {
		kept, _ := s.restoredBackup()
		note = "its channel backup cannot be read (" + err.Error() +
			"): it is damaged, or another node's. Restore its " +
			"channels by hand from a good copy (lncli " +
			"restorechanbackup), then delete " + kept
	}
	s.mu.Lock()
	s.restoreNote = note
	s.mu.Unlock()
	log.Warnf("Bridge could not restore the SHA256 node's channels: %s",
		note)

	return err
}

// restoreHopeless is a restore that cannot work however often it is tried:
// a backup lnd cannot decrypt or read (damaged, cut short, or made by another
// node). Anything else, a peer that cannot be reached above all, may work
// later.
func restoreHopeless(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"message authentication failed",
		"payload size too small",
		"unable to unpack unknown multi-version",
		"unexpected EOF",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}

	return false
}

// restoreStatus is restoreNote, for the status.
func (s *supervisor) restoreStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.restoreNote
}

// startRestore runs restoreUntilDone once, in the background, until quit.
func (s *supervisor) startRestore(quit <-chan struct{}) {
	s.mu.Lock()
	if s.restoring {
		s.mu.Unlock()
		return
	}
	s.restoring = true
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-quit:
		case <-ctx.Done():
		}
		cancel()
	}()
	go func() {
		defer cancel()
		s.restoreUntilDone(ctx, sha256RestoreRetry)
		s.mu.Lock()
		s.restoring = false
		s.mu.Unlock()
	}()
}

// restoreUntilDone keeps restoring until it has worked or ctx ends.
//
// Tried again after every, doubling up to sha256RestoreMaxRetry; never again
// once lnd says the backup cannot be read, which the status then says how to
// get past. Shutting down is no failure and is not reported as one.
func (s *supervisor) restoreUntilDone(ctx context.Context,
	every time.Duration) {

	wait := every
	for {
		err := s.restoreChannels(ctx)
		if err == nil || restoreHopeless(err) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > sha256RestoreMaxRetry {
			wait = sha256RestoreMaxRetry
		}
	}
}

// waitActive waits for the node to finish starting after creation or unlock.
//
// SERVER_ACTIVE, not RPC_ACTIVE: in between, lnd is catching up with its chain
// and answers some of the calls the next steps make with "the server is still
// starting", which would read as a node that is down. A node that is not there
// within the wait is handed back to the retry loop as on its way, named for
// what it is doing.
func (s *supervisor) waitActive(ctx context.Context, conn unlockerClients,
	created bool) error {

	state, detail := sha256Syncing, "the SHA256 node is catching up with "+
		"its chain before it serves calls"
	if created {
		state, detail = sha256CreatingWallet, "the SHA256 node's new "+
			"wallet is opening and scanning its chain; this takes a "+
			"few minutes the first time"
	}
	s.record(state, detail)

	deadline := s.now().Add(sha256ReadyWait)
	for {
		st, err := conn.GetState(ctx)
		if err == nil && st == lnrpc.WalletState_SERVER_ACTIVE {
			return nil
		}
		if s.now().After(deadline) {
			return s.pending(state, detail)
		}

		select {
		case <-ctx.Done():
			return s.pending(state, detail)
		case <-time.After(time.Second):
		}
	}
}

// permissionsFingerprint identifies the permission list a macaroon was baked
// with, so a release that changes the list re-bakes it.
func permissionsFingerprint() string {
	return fingerprintOf(bridgeMacaroonPermissions)
}

func fingerprintOf(perms []string) string {
	h := sha256.Sum256([]byte(strings.Join(perms, "\n")))

	return hex.EncodeToString(h[:])
}

// operatorMacaroonPermissions is what the operator's console (the dashboard)
// does with the SHA256 node, and nothing more: read its balances and
// channels, give a deposit address, connect a peer and open a channel, close
// one, send its coins on chain, take its channel backup, and pay a SHA256
// invoice from it (the operator paying from their own bridge, which needs no
// swap). The console used to hold the node's admin macaroon for this, which
// also signs, changes fees and policy, and bakes macaroons.
var operatorMacaroonPermissions = []string{
	"/lnrpc.Lightning/GetInfo",
	"/lnrpc.Lightning/WalletBalance",
	"/lnrpc.Lightning/ChannelBalance",
	"/lnrpc.Lightning/ListChannels",
	"/lnrpc.Lightning/PendingChannels",
	"/lnrpc.Lightning/ListPeers",
	"/lnrpc.Lightning/NewAddress",
	"/lnrpc.Lightning/ConnectPeer",
	"/lnrpc.Lightning/OpenChannelSync",
	"/lnrpc.Lightning/CloseChannel",
	"/lnrpc.Lightning/EstimateFee",
	"/lnrpc.Lightning/SendCoins",
	"/lnrpc.Lightning/ExportAllChannelBackups",
	"/lnrpc.Lightning/DecodePayReq",
	"/routerrpc.Router/SendPaymentV2",
	"/routerrpc.Router/TrackPaymentV2",
}

// ensureOperatorMacaroon bakes the console's macaroon (see
// operatorMacaroonPermissions) beside the bridge's, unless a current one is
// there. The bridge does not need it, so failing here only says so.
func (s *supervisor) ensureOperatorMacaroon(ctx context.Context) bool {
	path := s.cfg.SHA256OperatorMacaroonPath
	if path == "" {
		return true
	}
	if cur, _ := os.ReadFile(path + ".perms"); string(cur) ==
		fingerprintOf(operatorMacaroonPermissions) {

		if mac, err := os.ReadFile(path); err == nil &&
			(&macaroon.Macaroon{}).UnmarshalBinary(mac) == nil {

			conn, err := s.dialAdmin(s.cfg, mac)
			if err == nil {
				_, err = conn.IdentityPubkey(ctx)
				conn.Close()
				if err == nil || !macaroonRefused(err) {
					return true
				}
			}
		}
	}

	admin, err := os.ReadFile(s.cfg.SHA256AdminMacaroonPath)
	if err != nil {
		return false
	}
	conn, err := s.dialAdmin(s.cfg, admin)
	if err != nil {
		return false
	}
	defer conn.Close()

	baked, err := conn.BakeMacaroon(ctx, operatorMacaroonPermissions)
	if err == nil {
		err = writeFileAtomic(path, baked)
	}
	if err == nil {
		err = writeFileAtomic(path+".perms",
			[]byte(fingerprintOf(operatorMacaroonPermissions)))
	}
	if err != nil {
		log.Warnf("Bridge could not bake the console's macaroon for the "+
			"SHA256 node: %v", err)

		return false
	}
	log.Infof("Bridge baked the console's macaroon for the SHA256 node")

	return true
}

// ensureMacaroon bakes the bridge's macaroon from the node's admin one, unless
// a current one is already there.
//
// Kept apart from the admin macaroon on purpose: the admin one stays in the
// SHA256 node's own directory for the operator's tools, and the bridge holds
// only what it calls. One baked from an earlier permission list, one that does
// not parse, and one the node no longer accepts (its macaroon store was reset)
// are all replaced rather than left to fail.
func (s *supervisor) ensureMacaroon(ctx context.Context) error {
	current, err := s.macaroonCurrent(ctx)
	if err != nil || current {
		return err
	}

	admin, err := os.ReadFile(s.cfg.SHA256AdminMacaroonPath)
	if err != nil || len(admin) == 0 {
		return s.pending(sha256CreatingWallet, fmt.Sprintf("waiting for "+
			"the SHA256 node to write its admin macaroon (%s)",
			s.cfg.SHA256AdminMacaroonPath))
	}

	conn, err := s.dialAdmin(s.cfg, admin)
	if err != nil {
		return s.fail(sha256Unreachable, "the SHA256 Lightning node "+
			"cannot be reached", err)
	}
	defer conn.Close()

	baked, err := conn.BakeMacaroon(ctx, bridgeMacaroonPermissions)
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node would not make the "+
			"bridge's macaroon", err)
	}

	// The macaroon first and its fingerprint after: a crash between the two
	// leaves a fingerprint that does not match, which bakes again, rather
	// than one that vouches for a macaroon that is not there.
	if err := writeFileAtomic(s.cfg.SHA256MacaroonPath, baked); err != nil {
		return s.fail(sha256Error, "the bridge's macaroon for the "+
			"SHA256 node could not be saved", err)
	}
	if err := writeFileAtomic(s.cfg.SHA256MacaroonPath+".perms",
		[]byte(permissionsFingerprint())); err != nil {

		return s.fail(sha256Error, "the bridge's macaroon for the "+
			"SHA256 node could not be saved", err)
	}
	log.Infof("Bridge baked its macaroon for the SHA256 node")

	return nil
}

// macaroonCurrent reports whether the bridge's macaroon is there, baked from
// today's permission list, and accepted by the node. A node that does not
// answer is an error, because that says nothing about the macaroon.
func (s *supervisor) macaroonCurrent(ctx context.Context) (bool, error) {
	mac, err := os.ReadFile(s.cfg.SHA256MacaroonPath)
	if err != nil || len(mac) == 0 {
		return false, nil
	}
	if err := (&macaroon.Macaroon{}).UnmarshalBinary(mac); err != nil {
		log.Infof("Bridge macaroon for the SHA256 node does not parse; " +
			"baking a new one")

		return false, nil
	}
	perms, err := os.ReadFile(s.cfg.SHA256MacaroonPath + ".perms")
	if err != nil || string(perms) != permissionsFingerprint() {
		log.Infof("Bridge macaroon for the SHA256 node predates its " +
			"current permissions; baking a new one")

		return false, nil
	}

	conn, err := s.dialAdmin(s.cfg, mac)
	if err != nil {
		return false, s.fail(sha256Unreachable, "the SHA256 Lightning "+
			"node cannot be reached", err)
	}
	defer conn.Close()

	_, err = conn.IdentityPubkey(ctx)
	if err == nil {
		return true, nil
	}
	if macaroonRefused(err) {
		log.Infof("Bridge macaroon for the SHA256 node is no longer " +
			"accepted; baking a new one")

		return false, nil
	}

	return false, s.fail(sha256Unreachable, "the SHA256 Lightning node "+
		"does not answer", err)
}

// macaroonRefused reports whether an error is the node refusing the macaroon
// itself, as opposed to not answering. lnd says so in several ways, most of
// them with the Unknown code, so the message is read too.
func macaroonRefused(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated:
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"verification failed", "permission denied",
		"cannot get macaroon", "invalid macaroon",
		"unable to unmarshal",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}

	return false
}

// checkIdentity confirms the node answering is the one the derived seed makes.
//
// Without this, anything listening at the address with the files in the
// directory would be trusted with swaps: a node someone restored from the
// wrong phrase, or a directory replaced by hand. The expected key comes from
// the seed, so a match means this is the node we created and nobody else's.
func (s *supervisor) checkIdentity(ctx context.Context,
	entropy [aezeed.EntropySize]byte) error {

	mac, err := os.ReadFile(s.cfg.SHA256MacaroonPath)
	if err != nil {
		return s.fail(sha256Error, "the bridge's macaroon for the "+
			"SHA256 node cannot be read", err)
	}
	conn, err := s.dialAdmin(s.cfg, mac)
	if err != nil {
		return s.fail(sha256Unreachable, "the SHA256 Lightning node "+
			"cannot be reached", err)
	}
	defer conn.Close()

	got, err := conn.IdentityPubkey(ctx)
	if err != nil {
		return s.fail(sha256Unreachable, "the SHA256 Lightning node "+
			"does not answer", err)
	}

	want, err := Sha256NodeKey(entropy, s.coinType)
	if err != nil {
		return s.fail(sha256Error, "the SHA256 node's expected identity "+
			"could not be derived", err)
	}
	wantHex := hex.EncodeToString(want.SerializeCompressed())
	if !strings.EqualFold(got, wantHex) {
		return s.fail(sha256NotOurs, fmt.Sprintf("the SHA256 Lightning "+
			"node at %s is not the one this node created (its "+
			"identity is %s, the derived seed gives %s); the bridge "+
			"will not use it. If its directory was replaced or "+
			"restored from another phrase, move %s aside so a "+
			"node can be created from the right seed",
			s.cfg.SHA256RPCHost, got, wantHex, s.cfg.SHA256Dir), nil)
	}

	return nil
}

// writeFileAtomic writes a secret whole, flushed to disk, and renames it into
// place, so neither a crash nor a power cut leaves half of one or an empty
// file where a password was.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()

		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()

		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	// The rename is only durable once the directory is.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()

	return d.Sync()
}

// The real clients.

type grpcUnlocker struct {
	conn     *grpc.ClientConn
	state    lnrpc.StateClient
	unlocker lnrpc.WalletUnlockerClient
}

func dialUnlocker(cfg *Config) (unlockerClients, error) {
	creds, err := credentials.NewClientTLSFromFile(
		cfg.SHA256TLSCertPath, "",
	)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(
		cfg.SHA256RPCHost, grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, err
	}

	return &grpcUnlocker{
		conn:     conn,
		state:    lnrpc.NewStateClient(conn),
		unlocker: lnrpc.NewWalletUnlockerClient(conn),
	}, nil
}

func (g *grpcUnlocker) GetState(ctx context.Context) (lnrpc.WalletState,
	error) {

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := g.state.GetState(ctx, &lnrpc.GetStateRequest{})
	if err != nil {
		return 0, err
	}

	return resp.GetState(), nil
}

func (g *grpcUnlocker) InitWallet(ctx context.Context,
	req *lnrpc.InitWalletRequest) error {

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	_, err := g.unlocker.InitWallet(ctx, req)

	return err
}

func (g *grpcUnlocker) UnlockWallet(ctx context.Context,
	password []byte) error {

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	_, err := g.unlocker.UnlockWallet(ctx, &lnrpc.UnlockWalletRequest{
		WalletPassword: password,
	})

	return err
}

func (g *grpcUnlocker) Close() error { return g.conn.Close() }

type grpcAdmin struct {
	conn *grpc.ClientConn
	main lnrpc.LightningClient
}

func dialAdmin(cfg *Config, macaroon []byte) (adminClients, error) {
	creds, err := credentials.NewClientTLSFromFile(
		cfg.SHA256TLSCertPath, "",
	)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.SHA256RPCHost,
		grpc.WithTransportCredentials(creds),
		grpc.WithPerRPCCredentials(macaroonCredential{
			hex: hex.EncodeToString(macaroon),
		}),
	)
	if err != nil {
		return nil, err
	}

	return &grpcAdmin{conn: conn, main: lnrpc.NewLightningClient(conn)},
		nil
}

func (g *grpcAdmin) BakeMacaroon(ctx context.Context,
	perms []string) ([]byte, error) {

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req := &lnrpc.BakeMacaroonRequest{}
	for _, uri := range perms {
		req.Permissions = append(req.Permissions,
			&lnrpc.MacaroonPermission{Entity: "uri", Action: uri})
	}
	resp, err := g.main.BakeMacaroon(ctx, req)
	if err != nil {
		return nil, err
	}

	return hex.DecodeString(resp.GetMacaroon())
}

func (g *grpcAdmin) RestoreChannelBackups(ctx context.Context,
	multi []byte) error {

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	_, err := g.main.RestoreChannelBackups(ctx,
		&lnrpc.RestoreChanBackupRequest{
			Backup: &lnrpc.RestoreChanBackupRequest_MultiChanBackup{
				MultiChanBackup: multi,
			},
		})

	return err
}

func (g *grpcAdmin) IdentityPubkey(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := g.main.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return "", err
	}

	return info.GetIdentityPubkey(), nil
}

func (g *grpcAdmin) Close() error { return g.conn.Close() }
