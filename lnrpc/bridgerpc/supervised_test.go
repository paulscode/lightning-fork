//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSha256Node stands in for the stock lnd: its wallet state, what it was
// asked to do, and who it says it is.
type fakeSha256Node struct {
	mu sync.Mutex

	state       lnrpc.WalletState
	stateErr    error
	initReq     *lnrpc.InitWalletRequest
	initErr     error
	unlockWith  []byte
	password    []byte // what UnlockWallet accepts
	identity    string
	bakedPerms  []string
	bakes       int
	rejectBaked bool // the baked macaroon no longer works
	adminMac    []byte

	// writeAdmin is where InitWallet writes the admin macaroon, as lnd
	// does.
	writeAdmin string
}

type fakeUnlocker struct{ n *fakeSha256Node }

func (f fakeUnlocker) GetState(context.Context) (lnrpc.WalletState, error) {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()

	return f.n.state, f.n.stateErr
}

func (f fakeUnlocker) InitWallet(_ context.Context,
	req *lnrpc.InitWalletRequest) error {

	f.n.mu.Lock()
	defer f.n.mu.Unlock()

	if f.n.initErr != nil {
		return f.n.initErr
	}
	f.n.initReq = req
	f.n.password = req.WalletPassword
	f.n.state = lnrpc.WalletState_SERVER_ACTIVE
	if f.n.writeAdmin != "" {
		_ = os.MkdirAll(filepath.Dir(f.n.writeAdmin), 0700)
		_ = os.WriteFile(f.n.writeAdmin, f.n.adminMac, 0600)
	}

	return nil
}

func (f fakeUnlocker) UnlockWallet(_ context.Context, pw []byte) error {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()

	f.n.unlockWith = pw
	if !bytes.Equal(pw, f.n.password) {
		return status.Error(codes.Unknown, "invalid passphrase")
	}
	f.n.state = lnrpc.WalletState_SERVER_ACTIVE

	return nil
}

func (f fakeUnlocker) Close() error { return nil }

type fakeAdmin struct {
	n   *fakeSha256Node
	mac []byte
}

func (f fakeAdmin) BakeMacaroon(_ context.Context,
	perms []string) ([]byte, error) {

	f.n.mu.Lock()
	defer f.n.mu.Unlock()

	if !bytes.Equal(f.mac, f.n.adminMac) {
		return nil, status.Error(codes.PermissionDenied, "not admin")
	}
	f.n.bakes++
	f.n.bakedPerms = perms
	f.n.rejectBaked = false

	return []byte("baked-" + string(rune('0'+f.n.bakes))), nil
}

func (f fakeAdmin) IdentityPubkey(context.Context) (string, error) {
	f.n.mu.Lock()
	defer f.n.mu.Unlock()

	if f.n.rejectBaked && !bytes.Equal(f.mac, f.n.adminMac) {
		return "", status.Error(codes.Unknown,
			"verification failed: signature mismatch")
	}

	return f.n.identity, nil
}

func (f fakeAdmin) Close() error { return nil }

// testEntropy is the seed the fake node "was created from".
var testEntropy = func() [aezeed.EntropySize]byte {
	var e [aezeed.EntropySize]byte
	copy(e[:], bytes.Repeat([]byte{0x42}, aezeed.EntropySize))

	return e
}()

func testIdentity(t *testing.T) string {
	key, err := Sha256NodeKey(testEntropy, 1)
	require.NoError(t, err)

	return hex.EncodeToString(key.SerializeCompressed())
}

// newTestSupervisor is a supervisor over a temporary layout like the one
// validateBridgeConfig derives, talking to fake.
func newTestSupervisor(t *testing.T, fake *fakeSha256Node) (*supervisor,
	*Config) {

	dir := t.TempDir()
	cfg := &Config{
		Enabled:                 true,
		Supervised:              true,
		SHA256Dir:               filepath.Join(dir, "sha256-node"),
		SHA256RPCHost:           "127.0.0.1:10019",
		SHA256TLSCertPath:       filepath.Join(dir, "sha256-node", "tls.cert"),
		SHA256PasswordFile:      filepath.Join(dir, "net", "bridge", "sha256", "wallet.password"),
		SHA256MacaroonPath:      filepath.Join(dir, "net", "bridge", "sha256", "bridge.macaroon"),
		SHA256AdminMacaroonPath: filepath.Join(dir, "sha256-node", "data", "chain", "bitcoin", "regtest", "admin.macaroon"),
		Deps: &Deps{
			Network: "regtest",
			DeriveSha256Seed: func() ([aezeed.EntropySize]byte, error) {
				return testEntropy, nil
			},
		},
	}
	fake.writeAdmin = cfg.SHA256AdminMacaroonPath
	if fake.adminMac == nil {
		fake.adminMac = []byte("admin")
	}

	s := newSupervisor(cfg)
	s.dialUnlocker = func(*Config) (unlockerClients, error) {
		return fakeUnlocker{fake}, nil
	}
	s.dialAdmin = func(_ *Config, mac []byte) (adminClients, error) {
		return fakeAdmin{fake, mac}, nil
	}

	return s, cfg
}

// startNode writes the certificate, which is what the node does first.
func startNode(t *testing.T, cfg *Config) {
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256TLSCertPath), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256TLSCertPath, []byte("cert"), 0600))
}

func TestSupervisorFirstRun(t *testing.T) {
	fake := &fakeSha256Node{
		state: lnrpc.WalletState_NON_EXISTING, identity: "",
	}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	ctx := context.Background()

	// Before the node has started: the password is written (the platform
	// waits for it), and the bridge waits for the certificate.
	err := s.prepare(ctx)
	require.ErrorIs(t, err, errSha256Pending)
	state, _ := s.report()
	require.Equal(t, sha256Starting, state)

	pw, err := os.ReadFile(cfg.SHA256PasswordFile)
	require.NoError(t, err)
	require.Len(t, pw, 64, "32 random bytes, hex")
	info, err := os.Stat(cfg.SHA256PasswordFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	// Started: the wallet is created from the derived seed, the macaroon
	// baked, and the identity matches.
	startNode(t, cfg)
	require.NoError(t, s.prepare(ctx))
	state, _ = s.report()
	require.Equal(t, sha256Ready, state)

	require.NotNil(t, fake.initReq)
	require.Equal(t, pw, fake.initReq.WalletPassword)
	require.EqualValues(t, sha256RecoveryWindow,
		fake.initReq.RecoveryWindow)
	var words aezeed.Mnemonic
	require.Len(t, fake.initReq.CipherSeedMnemonic, len(words))
	copy(words[:], fake.initReq.CipherSeedMnemonic)
	seed, err := words.ToCipherSeed(nil)
	require.NoError(t, err)
	require.Equal(t, testEntropy, seed.Entropy,
		"the wallet must be created from the derived seed")

	require.Equal(t, bridgeMacaroonPermissions, fake.bakedPerms)
	mac, err := os.ReadFile(cfg.SHA256MacaroonPath)
	require.NoError(t, err)
	require.Equal(t, "baked-1", string(mac))
	info, err = os.Stat(cfg.SHA256MacaroonPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	// Again, as after every restart: nothing is created twice.
	require.NoError(t, s.prepare(ctx))
	require.Equal(t, 1, fake.bakes)
	pw2, err := os.ReadFile(cfg.SHA256PasswordFile)
	require.NoError(t, err)
	require.Equal(t, pw, pw2, "the password must not change")
}

// The bridge's macaroon carries only what it calls: nothing that opens or
// closes channels or moves on-chain funds.
func TestBridgeMacaroonPermissionsAreNarrow(t *testing.T) {
	for _, uri := range bridgeMacaroonPermissions {
		for _, forbidden := range []string{
			"SendCoins", "SendMany", "OpenChannel", "CloseChannel",
			"AbandonChannel", "UpdateChannelPolicy", "BakeMacaroon",
			"ExportAllChannelBackups", "NewAddress", "SignMessage",
			"Connect", "Disconnect", "SendPaymentSync", "SendToRoute",
		} {
			require.NotContains(t, uri, "/"+forbidden,
				"%s must not be in the bridge's macaroon", uri)
		}
	}
	require.Len(t, bridgeMacaroonPermissions, 14)
}

func TestSupervisorWaitsWhileStarting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state lnrpc.WalletState
		err   error
	}{
		{"waiting to start", lnrpc.WalletState_WAITING_TO_START, nil},
		{"not answering yet", 0,
			status.Error(codes.Unavailable, "connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSha256Node{state: tc.state, stateErr: tc.err}
			s, cfg := newTestSupervisor(t, fake)
			startNode(t, cfg)

			err := s.prepare(context.Background())
			require.ErrorIs(t, err, errSha256Pending)
			state, _ := s.report()
			require.Equal(t, sha256Starting, state)
			require.Nil(t, fake.initReq)
		})
	}
}

// Locked means the node was started without its password file; the bridge
// unlocks it with the password it keeps, and says so plainly when that fails
// rather than retrying a wrong password forever.
func TestSupervisorUnlocks(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_LOCKED}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256PasswordFile), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256PasswordFile,
		[]byte("0123456789abcdef\n"), 0600))
	fake.password = []byte("0123456789abcdef")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256AdminMacaroonPath), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256AdminMacaroonPath, fake.adminMac, 0600))

	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, []byte("0123456789abcdef"), fake.unlockWith,
		"the trailing newline is not part of the password, as for lnd")
	require.Nil(t, fake.initReq, "a locked wallet exists; never create")

	// A wallet this node did not create.
	fake.state = lnrpc.WalletState_LOCKED
	fake.password = []byte("someone else's")
	err := s.prepare(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errSha256Pending)
	state, detail := s.report()
	require.Equal(t, sha256Locked, state)
	require.Contains(t, detail, "did not create")
}

// A node whose identity is not the one the derived seed makes is refused: it
// may be a restore from the wrong phrase, or not ours at all.
func TestSupervisorRefusesAnotherNode(t *testing.T) {
	fake := &fakeSha256Node{
		state:    lnrpc.WalletState_SERVER_ACTIVE,
		identity: "02" + hex.EncodeToString(bytes.Repeat([]byte{1}, 32)),
	}
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256AdminMacaroonPath), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256AdminMacaroonPath, fake.adminMac, 0600))

	err := s.prepare(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errSha256Pending)
	state, detail := s.report()
	require.Equal(t, sha256NotOurs, state)
	require.Contains(t, detail, fake.identity)
	require.Contains(t, detail, testIdentity(t))
	require.Contains(t, detail, cfg.SHA256Dir)
}

// A baked macaroon the node no longer accepts (its macaroon store was reset)
// is replaced rather than left to fail every call.
func TestSupervisorRebakesARejectedMacaroon(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_SERVER_ACTIVE}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256AdminMacaroonPath), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256AdminMacaroonPath, fake.adminMac, 0600))

	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, 1, fake.bakes)

	fake.rejectBaked = true
	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, 2, fake.bakes)
	mac, err := os.ReadFile(cfg.SHA256MacaroonPath)
	require.NoError(t, err)
	require.Equal(t, "baked-2", string(mac))
}

// The admin macaroon appears a moment after the wallet; until then the bridge
// waits rather than failing.
func TestSupervisorWaitsForTheAdminMacaroon(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_SERVER_ACTIVE}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)

	err := s.prepare(context.Background())
	require.ErrorIs(t, err, errSha256Pending)
	state, _ := s.report()
	require.Equal(t, sha256CreatingWallet, state)
	require.Zero(t, fake.bakes)
}

func TestSupervisorPasswordFile(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_NON_EXISTING}
	s, cfg := newTestSupervisor(t, fake)

	// One that cannot be a password this node wrote is not overwritten:
	// it may be guarding a wallet.
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256PasswordFile), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256PasswordFile, []byte("\n"), 0600))
	err := s.prepare(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errSha256Pending)
	raw, _ := os.ReadFile(cfg.SHA256PasswordFile)
	require.Equal(t, "\n", string(raw))
}

func TestSupervisorWithoutAWallet(t *testing.T) {
	fake := &fakeSha256Node{}
	s, _ := newTestSupervisor(t, fake)
	s.derive = nil

	err := s.prepare(context.Background())
	require.Error(t, err)
	state, _ := s.report()
	require.Equal(t, sha256Error, state)
}

func TestSupervisorReportsACreationFailure(t *testing.T) {
	fake := &fakeSha256Node{
		state:   lnrpc.WalletState_NON_EXISTING,
		initErr: errors.New("wallet already exists"),
	}
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)

	err := s.prepare(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errSha256Pending)
	require.Contains(t, err.Error(), "wallet already exists")
}

// A wallet that has not come up within the wait hands back to the retry loop
// as pending, rather than holding the attempt open indefinitely.
func TestSupervisorGivesUpWaitingPolitely(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_LOCKED}
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256PasswordFile), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256PasswordFile,
		[]byte("0123456789abcdef"), 0600))
	fake.password = []byte("0123456789abcdef")

	// Unlocking succeeds but the node stays in RPC_ACTIVE.
	s.dialUnlocker = func(*Config) (unlockerClients, error) {
		return stuckUnlocker{fakeUnlocker{fake}}, nil
	}
	clock := time.Now()
	s.now = func() time.Time {
		clock = clock.Add(time.Minute)

		return clock
	}

	err := s.prepare(context.Background())
	require.ErrorIs(t, err, errSha256Pending)
}

type stuckUnlocker struct{ fakeUnlocker }

func (stuckUnlocker) GetState(context.Context) (lnrpc.WalletState, error) {
	return lnrpc.WalletState_RPC_ACTIVE, nil
}

func TestCoinTypeFor(t *testing.T) {
	require.EqualValues(t, 0, coinTypeFor("mainnet"))
	for _, n := range []string{"regtest", "testnet", "testnet4", "signet",
		"simnet", ""} {

		require.EqualValues(t, 1, coinTypeFor(n), n)
	}
}
