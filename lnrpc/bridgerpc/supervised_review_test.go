//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// toBLAKE2b and a bridge that starts with no rate.
// ---------------------------------------------------------------------------

// Its bounds are converted at the rate in force when it is built, so without a
// rate it is held: built, so swaps already in the journal are driven, but
// quoting nothing until SetRate rebuilds it with its bounds converted.
func TestToBLAKE2bWaitsForARate(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0
	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	require.True(t, svc.heldForRate)
	var held *side
	for _, sd := range svc.sides {
		if sd.name == "toBLAKE2b" {
			held = sd
		}
	}
	require.NotNil(t, held, "built, to drive swaps already in the journal")
	require.Len(t, svc.sides, 2)

	_, err := svc.pricer(held)(context.Background())
	require.ErrorIs(t, err, ErrNoRate, "and quoting nothing without a rate")
	require.ErrorIs(t, err, quote.ErrRefused)
}

// With a rate set at runtime and none configured, both directions come up and
// the BLAKE2b-paying bounds are converted at that rate.
func TestToBLAKE2bComesUpWithARuntimeRate(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0
	cfg.MaxSwapMsat = 1_000_000_000
	cfg.Journal = filepath.Join(t.TempDir(), "swaps.journal")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.Journal), 0700))
	b, err := openRatebook(filepath.Dir(cfg.Journal), 0, time.Hour, nil)
	require.NoError(t, err)
	_, _, err = b.set(0.005, 0)
	require.NoError(t, err)

	svc, err := newService(&cfg, NewLocal((&fakeNode{synced: true}).deps()),
		remote(nil, nil, nil))
	require.NoError(t, err)
	t.Cleanup(svc.close)

	require.False(t, svc.heldForRate)
	var found bool
	for _, sd := range svc.sides {
		if sd.name == "toBLAKE2b" {
			found = true
			require.EqualValues(t, 1_000_000_000/0.005,
				sd.quoter.Policy.MaxSwapMsat,
				"the cap converted at the rate in force")
		}
	}
	require.True(t, found)
}

// Setting the first rate rebuilds the bridge, so the held direction comes up
// without a restart; the replaced bridge is stopped and nothing is left
// running when the node stops.
func TestSettingTheFirstRateRebuildsTheBridge(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0
	cfg.Deps = (&fakeNode{synced: true}).deps()
	srv, _, err := New(&cfg)
	require.NoError(t, err)
	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))
	cfg.Journal = svc.cfg.Journal
	require.True(t, svc.heldForRate)
	srv.quit = make(chan struct{})
	srv.mu.Lock()
	srv.svc = svc
	srv.mu.Unlock()

	_, err = srv.SetRate(context.Background(), &SetRateRequest{Rate: 0.005})
	require.NoError(t, err)

	// The old service is retired at once and a new attempt starts; with
	// no SHA256 node to dial here, it reports why rather than hanging.
	require.Eventually(t, func() bool {
		return srv.service() == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()

		return srv.startErr != nil && srv.startErr != errStarting
	}, 10*time.Second, 20*time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		_ = srv.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after a rebuild")
	}
}

// ---------------------------------------------------------------------------
// Where a rate came from, once the configuration stops naming one.
// ---------------------------------------------------------------------------

func writeRateFile(t *testing.T, dir string, f rateFile) {
	t.Helper()

	raw, err := json.Marshal(f)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, rateFileName), raw,
		0600))
}

// Removing the configured rate removes it: trading on at it would be the
// bridge choosing a price nobody set at runtime.
func TestRemovingTheConfiguredRateDropsTheConfiguredOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	b, err := openRatebook(dir, 0.004, time.Hour, nil)
	require.NoError(t, err)
	r, _, err := b.usable()
	require.NoError(t, err)
	require.Equal(t, 0.004, r)

	again, err := openRatebook(dir, 0, time.Hour, nil)
	require.NoError(t, err)
	_, _, err = again.usable()
	require.ErrorIs(t, err, ErrNoRate)
}

// Files written before the source was recorded are read by comparing the rate
// with the configured one.
func TestOlderRateFilesAreReadByWhatTheyHold(t *testing.T) {
	t.Parallel()

	now := time.Now()

	fromConfig := t.TempDir()
	writeRateFile(t, fromConfig, rateFile{
		Rate: 0.004, SetAt: now, Configured: 0.004,
	})
	b, err := openRatebook(fromConfig, 0, time.Hour, nil)
	require.NoError(t, err)
	_, _, err = b.usable()
	require.ErrorIs(t, err, ErrNoRate)

	setLater := t.TempDir()
	writeRateFile(t, setLater, rateFile{
		Rate: 0.0048, SetAt: now, Configured: 0.004,
	})
	b, err = openRatebook(setLater, 0, time.Hour, nil)
	require.NoError(t, err)
	r, _, err := b.usable()
	require.NoError(t, err)
	require.Equal(t, 0.0048, r)

	// A runtime rate equal to the configured one is still a runtime
	// rate when the file says so.
	tagged := t.TempDir()
	writeRateFile(t, tagged, rateFile{
		Rate: 0.004, SetAt: now, Configured: 0.004,
		Source: rateFromRuntime,
	})
	b, err = openRatebook(tagged, 0, time.Hour, nil)
	require.NoError(t, err)
	r, _, err = b.usable()
	require.NoError(t, err)
	require.Equal(t, 0.004, r)
}

// ---------------------------------------------------------------------------
// The supervisor's harder states.
// ---------------------------------------------------------------------------

// A node with a wallet and no password here lost it with this node's data. A
// new password would not open that wallet and lnd would exit on it at every
// start, so none is made; the operator is told the way out.
func TestSupervisorWillNotReplaceALostPassword(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_LOCKED}
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)
	db := filepath.Join(filepath.Dir(cfg.SHA256AdminMacaroonPath),
		"wallet.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(db), 0700))
	require.NoError(t, os.WriteFile(db, []byte("wallet"), 0600))

	err := s.prepare(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errSha256Pending)
	state, detail := s.report()
	require.Equal(t, sha256Locked, state)
	require.Contains(t, detail, "is gone")
	require.Contains(t, detail, cfg.SHA256Dir)
	_, err = os.Stat(cfg.SHA256PasswordFile)
	require.True(t, os.IsNotExist(err), "no password may be made for it")
}

// A node that has started before and never answers is starting for a while
// and then failing: lnd exits on a wrong password or an unreachable chain
// node, and the platform restarting it must not read as "starting" for ever.
func TestSupervisorReportsANodeThatNeverAnswers(t *testing.T) {
	fake := &fakeSha256Node{
		stateErr: status.Error(codes.Unavailable, "connection refused"),
	}
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)

	clock := time.Now()
	s.now = func() time.Time { return clock }

	require.ErrorIs(t, s.prepare(context.Background()), errSha256Pending)
	clock = clock.Add(sha256StartTimeout - time.Second)
	require.ErrorIs(t, s.prepare(context.Background()), errSha256Pending)

	clock = clock.Add(2 * time.Second)
	err := s.prepare(context.Background())
	require.Error(t, err)
	require.NotErrorIs(t, err, errSha256Pending)
	state, detail := s.report()
	require.Equal(t, sha256Unreachable, state)
	require.Contains(t, detail, "look at its log")

	// Answering again resets the clock.
	fake.mu.Lock()
	fake.stateErr = nil
	fake.state = lnrpc.WalletState_WAITING_TO_START
	fake.mu.Unlock()
	require.ErrorIs(t, s.prepare(context.Background()), errSha256Pending)
	require.True(t, s.unansweredSince.IsZero())
}

// A node that already has a wallet and is catching up is syncing, not
// creating a wallet: every restart goes through this.
func TestSupervisorCallsAnExistingWalletSyncing(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_RPC_ACTIVE}
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)

	clock := time.Now()
	s.now = func() time.Time {
		clock = clock.Add(time.Minute)

		return clock
	}

	require.ErrorIs(t, s.prepare(context.Background()), errSha256Pending)
	state, detail := s.report()
	require.Equal(t, sha256Syncing, state)
	require.Contains(t, detail, "catching up")
}

// A release that adds a method to the permission list re-bakes the macaroon
// every upgraded node already has; one that does not parse is re-baked too.
func TestSupervisorRebakesAnOutdatedMacaroon(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_SERVER_ACTIVE}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	startNode(t, cfg)
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.SHA256AdminMacaroonPath), 0700))
	require.NoError(t, os.WriteFile(cfg.SHA256AdminMacaroonPath, fake.adminMac, 0600))

	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, 1, fake.bakes)

	// Baked under another list.
	require.NoError(t, os.WriteFile(cfg.SHA256MacaroonPath+".perms",
		[]byte("an older list"), 0600))
	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, 2, fake.bakes)

	// Corrupted.
	require.NoError(t, os.WriteFile(cfg.SHA256MacaroonPath,
		[]byte("not a macaroon"), 0600))
	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, 3, fake.bakes)

	// Current: left alone.
	require.NoError(t, s.prepare(context.Background()))
	require.Equal(t, 3, fake.bakes)
}

func TestMacaroonRefused(t *testing.T) {
	for _, err := range []error{
		status.Error(codes.PermissionDenied, "x"),
		status.Error(codes.Unauthenticated, "x"),
		status.Error(codes.Unknown, "verification failed: signature "+
			"mismatch after caveat verification"),
		status.Error(codes.Unknown, "permission denied"),
		status.Error(codes.Unknown, "unable to unmarshal macaroon"),
	} {
		require.True(t, macaroonRefused(err), "%v", err)
	}
	for _, err := range []error{
		status.Error(codes.Unavailable, "connection refused"),
		status.Error(codes.Unknown, "server is still in the process "+
			"of starting"),
		context.DeadlineExceeded,
	} {
		require.False(t, macaroonRefused(err), "%v", err)
	}
}

// Secrets are written whole and private, and a second write replaces the
// first rather than appending to it.
func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "secret")
	require.NoError(t, writeFileAtomic(path, []byte("one")))
	require.NoError(t, writeFileAtomic(path, []byte("two")))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "two", string(raw))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	_, err = os.Stat(path + ".tmp")
	require.True(t, os.IsNotExist(err))
}

// ---------------------------------------------------------------------------
// The status summary does not turn an unreadable balance into a zero.
// ---------------------------------------------------------------------------

type summaryMain struct {
	*fakeMain
	walletErr  error
	channelErr error
}

func (f *summaryMain) WalletBalance(context.Context,
	*lnrpc.WalletBalanceRequest, ...grpc.CallOption) (
	*lnrpc.WalletBalanceResponse, error) {

	if f.walletErr != nil {
		return nil, f.walletErr
	}

	return &lnrpc.WalletBalanceResponse{ConfirmedBalance: 5}, nil
}

func (f *summaryMain) ChannelBalance(context.Context,
	*lnrpc.ChannelBalanceRequest, ...grpc.CallOption) (
	*lnrpc.ChannelBalanceResponse, error) {

	if f.channelErr != nil {
		return nil, f.channelErr
	}

	return &lnrpc.ChannelBalanceResponse{}, nil
}

func TestSummarySaysWhatItCannotRead(t *testing.T) {
	t.Parallel()

	denied := status.Error(codes.PermissionDenied, "permission denied")
	main := &summaryMain{
		fakeMain: &fakeMain{
			info: &lnrpc.GetInfoResponse{
				SyncedToChain: true, IdentityPubkey: "02ab",
			},
			channels: &lnrpc.ListChannelsResponse{},
		},
		walletErr: denied,
	}
	r := remote(nil, nil, nil)
	r.main = main

	srv, _, err := New(&Config{Enabled: true, ToSHA256: true})
	require.NoError(t, err)
	srv.mu.Lock()
	srv.remote = r
	srv.mu.Unlock()

	out := srv.sha256Summary(context.Background())
	require.Equal(t, "external", out.Mode)
	require.Equal(t, sha256Ready, out.State)
	require.Contains(t, out.Detail, "on-chain balance cannot be read")
	require.NotContains(t, out.Detail, "empty")

	// With everything readable, an empty node is called empty.
	main.walletErr = nil
	main.fakeMain.info.NumActiveChannels = 0
	out = srv.sha256Summary(context.Background())
	require.Contains(t, out.Detail, "coins and no channel",
		"5 sat on chain and no channel")
}

// The background restore runs once at a time, finishes the restore, and
// stops at quit.
func TestSupervisorRestoreRunsOnceAndStops(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_NON_EXISTING}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	ctx := context.Background()

	scb := s.nodeChannelBackup()
	require.NoError(t, os.MkdirAll(filepath.Dir(scb), 0700))
	require.NoError(t, os.WriteFile(scb, []byte("backup"), 0600))
	require.ErrorIs(t, s.prepare(ctx), errSha256Pending)
	startNode(t, cfg)
	require.NoError(t, s.prepare(ctx))

	quit := make(chan struct{})
	s.startRestore(quit)
	s.startRestore(quit) // a second connect while the first runs
	require.Eventually(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()

		return len(fake.restored) == 1
	}, 5*time.Second, 10*time.Millisecond)
	close(quit)
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()

		return !s.restoring
	}, 5*time.Second, 10*time.Millisecond)
	fake.mu.Lock()
	require.Len(t, fake.restored, 1)
	fake.mu.Unlock()
}

// A SHA256 node the bridge refused is reported as refused, with why, never as
// a node ready to be funded.
func TestSummarySaysWhyTheNodeWasRefused(t *testing.T) {
	t.Parallel()

	srv, _, err := New(&Config{Enabled: true, ToSHA256: true})
	require.NoError(t, err)
	refused := fmt.Errorf("the bridge will not use the SHA256 node at "+
		"x: %w: the SHA256 node follows the BLAKE2b chain", ErrConfig)
	srv.setStartErr(refused)

	out := srv.sha256Summary(context.Background())
	require.Equal(t, sha256Error, out.State)
	require.Contains(t, out.Detail, "follows the BLAKE2b chain")

	// Any other reason it is not up says nothing of the kind: the node is
	// asked as usual (here, not there at all).
	srv.setStartErr(errors.New("dial tcp: connection refused"))
	out = srv.sha256Summary(context.Background())
	require.NotContains(t, out.Detail, "BLAKE2b chain")
}

// A backup lnd cannot read is tried once, and the status says what to do; a
// peer that cannot be reached is tried again.
func TestSupervisorGivesUpOnAnUnreadableBackup(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_NON_EXISTING}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	ctx := context.Background()

	scb := s.nodeChannelBackup()
	require.NoError(t, os.MkdirAll(filepath.Dir(scb), 0700))
	require.NoError(t, os.WriteFile(scb, []byte("garbage"), 0600))
	require.ErrorIs(t, s.prepare(ctx), errSha256Pending)
	startNode(t, cfg)
	require.NoError(t, s.prepare(ctx))

	fake.restoreErr = errors.New("unable to unpack chan backup: " +
		"chacha20poly1305: message authentication failed")
	done := make(chan struct{})
	go func() {
		s.restoreUntilDone(ctx, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("kept trying a backup that cannot be read")
	}
	require.Equal(t, 1, fake.restoreCalls)
	require.Contains(t, s.restoreStatus(), "cannot be read")
	require.Contains(t, s.restoreStatus(), "restorechanbackup")

	fake.mu.Lock()
	fake.restoreErr = errors.New("unable to connect to peer")
	fake.restoreCalls = 0
	fake.mu.Unlock()
	ctx2, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	s.restoreUntilDone(ctx2, time.Millisecond)
	fake.mu.Lock()
	require.Greater(t, fake.restoreCalls, 1, "a peer may come back")
	fake.mu.Unlock()
	require.Contains(t, s.restoreStatus(), "trying again")
}

// Channels are restored after the chain check passes, and never when it fails.
func TestRestoreOnlyAfterTheChainCheck(t *testing.T) {
	setup := func(t *testing.T, blake2bID []byte) (*Server, *fakeSha256Node,
		*Remote) {

		fake := &fakeSha256Node{state: lnrpc.WalletState_NON_EXISTING}
		fake.identity = testIdentity(t)
		sup, cfg := newTestSupervisor(t, fake)
		scb := sup.nodeChannelBackup()
		require.NoError(t, os.MkdirAll(filepath.Dir(scb), 0700))
		require.NoError(t, os.WriteFile(scb, []byte("backup"), 0600))
		require.ErrorIs(t, sup.prepare(context.Background()),
			errSha256Pending)
		startNode(t, cfg)
		require.NoError(t, sup.prepare(context.Background()))

		var ours chainhash.Hash
		copy(ours[:], blake2bID)
		cfg.Deps = &Deps{Blake2bActivation: func(context.Context) (
			int32, [32]byte, bool, error) {

			return 300, ours, false, nil
		}}
		srv := &Server{cfg: cfg, sup: sup, quit: make(chan struct{})}
		t.Cleanup(func() { close(srv.quit) })

		header, id := sha256Block(9)
		r := remote(nil, nil, nil)
		r.chain = &headerChain{hash: id, header: header}

		return srv, fake, r
	}

	// The SHA256 node has the BLAKE2b block: refused, nothing restored.
	_, sameID := sha256Block(9)
	srv, fake, r := setup(t, sameID)
	require.ErrorIs(t, srv.checkChainThenRestore(context.Background(), r),
		ErrConfig)
	time.Sleep(50 * time.Millisecond)
	fake.mu.Lock()
	require.Zero(t, fake.restoreCalls)
	fake.mu.Unlock()

	// Another chain's block: passed, restored.
	srv, fake, r = setup(t, []byte("blake2b activation block id 32b!"))
	require.NoError(t, srv.checkChainThenRestore(context.Background(), r))
	require.Eventually(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()

		return len(fake.restored) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

// The console gets a macaroon of its own, with only what it does with the node:
// nothing that signs, changes policy or bakes. Baked once, again only when its
// permission list changes.
func TestTheConsoleGetsANarrowMacaroon(t *testing.T) {
	fake := &fakeSha256Node{state: lnrpc.WalletState_NON_EXISTING}
	fake.identity = testIdentity(t)
	s, cfg := newTestSupervisor(t, fake)
	cfg.SHA256OperatorMacaroonPath = filepath.Join(
		filepath.Dir(cfg.SHA256MacaroonPath), Sha256OperatorMacaroonName,
	)
	ctx := context.Background()

	require.ErrorIs(t, s.prepare(ctx), errSha256Pending)
	startNode(t, cfg)
	require.NoError(t, s.prepare(ctx))

	require.Len(t, fake.allBaked, 2)
	require.Equal(t, bridgeMacaroonPermissions, fake.allBaked[0])
	require.Equal(t, operatorMacaroonPermissions, fake.allBaked[1])
	for _, uri := range operatorMacaroonPermissions {
		for _, never := range []string{"Sign", "BakeMacaroon",
			"UpdateChannelPolicy", "SendPayment", "AddInvoice"} {

			require.NotContains(t, uri, never)
		}
	}
	info, err := os.Stat(cfg.SHA256OperatorMacaroonPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	// Again, as after a restart: not baked twice.
	require.NoError(t, s.prepare(ctx))
	require.Len(t, fake.allBaked, 2)

	// A list that has changed is baked again.
	require.NoError(t, os.WriteFile(
		cfg.SHA256OperatorMacaroonPath+".perms", []byte("old"), 0600,
	))
	require.NoError(t, s.prepare(ctx))
	require.Len(t, fake.allBaked, 3)
}
