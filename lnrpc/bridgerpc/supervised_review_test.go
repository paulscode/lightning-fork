//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// toBLAKE2b and a bridge that starts with no rate.
// ---------------------------------------------------------------------------

// Its bounds are converted at the rate in force when it is built, so without a
// rate it is held back rather than built with bounds in the wrong unit.
func TestToBLAKE2bWaitsForARate(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.FixedRate = 0
	svc := serviceFor(t, cfg, &fakeNode{synced: true},
		remote(nil, nil, nil))

	require.True(t, svc.heldForRate)
	for _, sd := range svc.sides {
		require.NotEqual(t, "toBLAKE2b", sd.name,
			"built with no rate to convert its bounds at")
	}
	require.Len(t, svc.sides, 1, "toSHA256 still comes up")
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
