//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"crypto/rand"
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
// that case find its coins, at the cost of a rescan from the fixed birthday,
// which is days of blocks rather than years.
const sha256RecoveryWindow = 2500

// sha256ReadyWait bounds how long one attempt waits for a wallet it has just
// created to come up, before handing back to the retry loop.
const sha256ReadyWait = 2 * time.Minute

// bridgeMacaroonPermissions is everything the bridge calls on the SHA256 node,
// method by method, and nothing else.
//
// Per method rather than per entity, because entities are coarse: offchain
// write alone would also allow closing channels and changing their policy.
// The list is what remote.go and the status summary call; a method added
// there that is missing here fails with a permission error naming it, which
// is the loud way to find out.
var bridgeMacaroonPermissions = []string{
	"/lnrpc.Lightning/GetInfo",
	"/lnrpc.Lightning/ListChannels",
	"/lnrpc.Lightning/DecodePayReq",
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

	mu     sync.Mutex
	state  string
	detail string
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

// set records where the node is and returns an error saying the same, so a
// step can report and return in one line.
func (s *supervisor) set(state, detail string, cause error) error {
	s.mu.Lock()
	s.state, s.detail = state, detail
	s.mu.Unlock()

	switch {
	case state == sha256Ready || state == sha256Syncing:
		return nil
	case cause != nil:
		return fmt.Errorf("%s: %w", detail, cause)
	case state == sha256Starting || state == sha256CreatingWallet:
		return fmt.Errorf("%w: %s", errSha256Pending, detail)
	default:
		return errors.New(detail)
	}
}

// prepare brings the supervised node to the point where the bridge can use it:
// a wallet created from the derived seed, unlocked, a bridge macaroon baked,
// and its identity checked. It returns nil once all of that holds, and an
// error saying where it stopped otherwise.
func (s *supervisor) prepare(ctx context.Context) error {
	if s.derive == nil {
		return s.set(sha256Error, "this node cannot derive a seed for "+
			"the SHA256 node (no wallet keys were handed to the "+
			"bridge)", nil)
	}

	password, err := s.ensurePassword()
	if err != nil {
		return s.set(sha256Error, "the SHA256 node's wallet password "+
			"could not be prepared", err)
	}

	// The certificate is the first thing lnd writes, so its absence
	// means the process has not started: the platform starts it once the
	// password file exists, which the step above has just made sure of.
	if _, err := os.Stat(s.cfg.SHA256TLSCertPath); err != nil {
		return s.set(sha256Starting, fmt.Sprintf("waiting for the "+
			"SHA256 Lightning node to start (it writes %s when it "+
			"does); the platform starts it once the bridge is on, "+
			"so if this lasts, look at that node's log",
			s.cfg.SHA256TLSCertPath), nil)
	}

	if err := s.ensureWallet(ctx, password); err != nil {
		return err
	}

	entropy, err := s.derive()
	if err != nil {
		return s.set(sha256Error, "the SHA256 node's seed could not "+
			"be derived", err)
	}
	if err := s.ensureMacaroon(ctx); err != nil {
		return err
	}

	return s.checkIdentity(ctx, entropy)
}

// ensurePassword returns the wallet password, creating it the first time.
//
// Random rather than derived: it protects the wallet file at rest, which is
// only worth anything if it is not computable from something else on the same
// disk. It is never the way back in (the seed is), so losing it costs a wallet
// re-creation from the same seed, not funds.
func (s *supervisor) ensurePassword() ([]byte, error) {
	path := s.cfg.SHA256PasswordFile
	if raw, err := os.ReadFile(path); err == nil {
		pw := []byte(strings.TrimRight(string(raw), "\r\n"))
		if len(pw) >= 8 {
			return pw, nil
		}

		return nil, fmt.Errorf("%s holds no usable password; remove it "+
			"only if the SHA256 node has no wallet yet", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	pw := []byte(hex.EncodeToString(buf[:]))

	if err := writeFileAtomic(path, pw); err != nil {
		return nil, err
	}

	return pw, nil
}

// ensureWallet creates the node's wallet from the derived seed if it has none,
// unlocks it if it is locked, and waits for it to come up.
func (s *supervisor) ensureWallet(ctx context.Context, password []byte) error {
	conn, err := s.dialUnlocker(s.cfg)
	if err != nil {
		return s.set(sha256Unreachable, fmt.Sprintf("the SHA256 "+
			"Lightning node at %s cannot be reached",
			s.cfg.SHA256RPCHost), err)
	}
	defer conn.Close()

	state, err := conn.GetState(ctx)
	if err != nil {
		// A node that is starting answers nothing for a moment, and
		// that is the common case right after the certificate appears.
		return s.set(sha256Starting, "the SHA256 Lightning node is "+
			"starting", nil)
	}

	switch state {
	case lnrpc.WalletState_WAITING_TO_START:
		return s.set(sha256Starting, "the SHA256 Lightning node is "+
			"starting", nil)

	case lnrpc.WalletState_NON_EXISTING:
		if err := s.createWallet(ctx, conn, password); err != nil {
			return err
		}

	case lnrpc.WalletState_LOCKED:
		// The platform starts the node with the password file, so it
		// unlocks itself. Being found locked means it was started
		// without it; unlocking here gets the bridge going, and the
		// wrong password is reported rather than retried forever.
		if err := conn.UnlockWallet(ctx, password); err != nil {
			return s.set(sha256Locked, "the SHA256 node's wallet "+
				"is locked and the password this node keeps "+
				"for it does not open it; this node did not "+
				"create that wallet", err)
		}
	}

	return s.waitActive(ctx, conn)
}

// createWallet creates the node's wallet from the derived seed.
func (s *supervisor) createWallet(ctx context.Context, conn unlockerClients,
	password []byte) error {

	entropy, err := s.derive()
	if err != nil {
		return s.set(sha256Error, "the SHA256 node's seed could not "+
			"be derived", err)
	}
	mnemonic, err := Sha256SeedMnemonic(entropy)
	if err != nil {
		return s.set(sha256Error, "the SHA256 node's seed could not "+
			"be encoded", err)
	}

	_ = s.set(sha256CreatingWallet, "creating the SHA256 node's wallet "+
		"from a seed derived from this node's own; there is nothing "+
		"new to write down", nil)

	err = conn.InitWallet(ctx, &lnrpc.InitWalletRequest{
		WalletPassword:     password,
		CipherSeedMnemonic: mnemonic[:],
		RecoveryWindow:     sha256RecoveryWindow,
	})
	if err != nil {
		return s.set(sha256Error, "the SHA256 node would not create "+
			"its wallet", err)
	}

	log.Infof("Bridge created the supervised SHA256 node's wallet from " +
		"the derived seed")

	return nil
}

// waitActive waits for the node's RPC to come up after creation or unlock.
func (s *supervisor) waitActive(ctx context.Context,
	conn unlockerClients) error {

	deadline := s.now().Add(sha256ReadyWait)
	for {
		// SERVER_ACTIVE, not RPC_ACTIVE: in between, lnd answers some
		// of the calls the next steps make with "the server is still
		// starting", which would read as a node that is down.
		state, err := conn.GetState(ctx)
		if err == nil && state == lnrpc.WalletState_SERVER_ACTIVE {
			return nil
		}
		if s.now().After(deadline) {
			return s.set(sha256CreatingWallet, "the SHA256 node's "+
				"wallet is opening; this can take a few minutes "+
				"the first time", nil)
		}

		select {
		case <-ctx.Done():
			return s.set(sha256Starting, "the SHA256 Lightning node "+
				"is starting", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// ensureMacaroon bakes the bridge's macaroon from the node's admin one, if it
// has not been baked yet.
//
// Kept apart from the admin macaroon on purpose: the admin one stays in the
// SHA256 node's own directory for the operator's tools, and the bridge holds
// only what it calls. A baked macaroon that no longer works (the node's
// macaroon store was reset) is replaced rather than left to fail every call.
func (s *supervisor) ensureMacaroon(ctx context.Context) error {
	if mac, err := os.ReadFile(s.cfg.SHA256MacaroonPath); err == nil &&
		len(mac) > 0 {

		ok, err := s.macaroonWorks(ctx, mac)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		log.Infof("Bridge macaroon for the SHA256 node no longer " +
			"works; baking a new one")
	}

	admin, err := os.ReadFile(s.cfg.SHA256AdminMacaroonPath)
	if err != nil || len(admin) == 0 {
		return s.set(sha256CreatingWallet, fmt.Sprintf("waiting for "+
			"the SHA256 node to write its admin macaroon (%s)",
			s.cfg.SHA256AdminMacaroonPath), nil)
	}

	conn, err := s.dialAdmin(s.cfg, admin)
	if err != nil {
		return s.set(sha256Unreachable, "the SHA256 Lightning node "+
			"cannot be reached", err)
	}
	defer conn.Close()

	baked, err := conn.BakeMacaroon(ctx, bridgeMacaroonPermissions)
	if err != nil {
		return s.set(sha256Error, "the SHA256 node would not make the "+
			"bridge's macaroon", err)
	}
	if err := writeFileAtomic(s.cfg.SHA256MacaroonPath, baked); err != nil {
		return s.set(sha256Error, "the bridge's macaroon for the "+
			"SHA256 node could not be saved", err)
	}

	return nil
}

// macaroonWorks reports whether the bridge's macaroon is accepted. A refusal
// on credentials is "no"; any other failure is an error, because it says
// nothing about the macaroon.
func (s *supervisor) macaroonWorks(ctx context.Context, mac []byte) (bool,
	error) {

	conn, err := s.dialAdmin(s.cfg, mac)
	if err != nil {
		return false, s.set(sha256Unreachable, "the SHA256 Lightning "+
			"node cannot be reached", err)
	}
	defer conn.Close()

	_, err = conn.IdentityPubkey(ctx)
	switch status.Code(err) {
	case codes.OK:
		return true, nil
	case codes.PermissionDenied, codes.Unauthenticated:
		return false, nil
	}
	if strings.Contains(err.Error(), "verification failed") ||
		strings.Contains(err.Error(), "cannot get macaroon") {

		return false, nil
	}

	return false, s.set(sha256Unreachable, "the SHA256 Lightning node "+
		"does not answer", err)
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
		return s.set(sha256Error, "the bridge's macaroon for the "+
			"SHA256 node cannot be read", err)
	}
	conn, err := s.dialAdmin(s.cfg, mac)
	if err != nil {
		return s.set(sha256Unreachable, "the SHA256 Lightning node "+
			"cannot be reached", err)
	}
	defer conn.Close()

	got, err := conn.IdentityPubkey(ctx)
	if err != nil {
		return s.set(sha256Unreachable, "the SHA256 Lightning node "+
			"does not answer", err)
	}

	want, err := Sha256NodeKey(entropy, s.coinType)
	if err != nil {
		return s.set(sha256Error, "the SHA256 node's expected identity "+
			"could not be derived", err)
	}
	wantHex := hex.EncodeToString(want.SerializeCompressed())
	if !strings.EqualFold(got, wantHex) {
		return s.set(sha256NotOurs, fmt.Sprintf("the SHA256 Lightning "+
			"node at %s is not the one this node created (its "+
			"identity is %s, the derived seed gives %s); the bridge "+
			"will not use it. If its directory was replaced or "+
			"restored from another phrase, move %s aside so a "+
			"node can be created from the right seed", s.cfg.SHA256RPCHost,
			got, wantHex, s.cfg.SHA256Dir), nil)
	}

	return s.set(sha256Ready, "ready", nil)
}

// writeFileAtomic writes a secret whole and renames it into place, so a crash
// leaves the old file or the new one and never half of either.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
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
