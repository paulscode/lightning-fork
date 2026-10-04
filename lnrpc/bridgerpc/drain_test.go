//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
	"github.com/stretchr/testify/require"
)

// A journal with one swap in the given state.
func journalWith(t *testing.T, state swap.State) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "swaps.journal")
	j, err := store.Open(path)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, j.Put(context.Background(), store.Record{
		Hash: [32]byte{7}, State: state, OutgoingCLTVLimit: 40,
		Invoice: "lnbc1x", IncomingMsat: 1_000, OutgoingMsat: 3,
		Rate: 0.003, Spread: 0.01, Created: now, Updated: now,
	}))
	require.NoError(t, j.Close())

	return path
}

func TestUnfinishedInJournal(t *testing.T) {
	t.Parallel()

	n, err := unfinishedInJournal("")
	require.NoError(t, err)
	require.Zero(t, n)

	n, err = unfinishedInJournal(filepath.Join(t.TempDir(), "none"))
	require.NoError(t, err)
	require.Zero(t, n, "no journal is nothing unfinished")

	n, err = unfinishedInJournal(journalWith(t, swap.Funded))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	n, err = unfinishedInJournal(journalWith(t, swap.Settled))
	require.NoError(t, err)
	require.Zero(t, n)
}

// Off with nothing unfinished: off. Off with a swap unfinished and a node to
// finish it through: draining, quoting nothing. Off with a swap and no node:
// said, and nothing started.
func TestAnOffBridgeDrainsWhatIsUnfinished(t *testing.T) {
	t.Parallel()

	off := usable()
	off.Enabled = false
	off.Journal = journalWith(t, swap.Settled)
	srv, _, err := New(&off)
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	require.False(t, srv.draining)
	require.NoError(t, srv.Stop())

	busy := usable()
	busy.Enabled = false
	busy.Journal = journalWith(t, swap.Funded)
	// A node that is not there: the drain keeps trying to reach it.
	busy.SHA256RPCHost = "127.0.0.1:1"
	srv, _, err = New(&busy)
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	require.True(t, srv.draining)
	status, err := srv.Status(context.Background(), &StatusRequest{})
	require.NoError(t, err)
	require.False(t, status.Enabled)
	require.Contains(t, status.Refusals[len(status.Refusals)-1],
		"finishing the swaps already under way")
	_, err = srv.Quote(context.Background(), &QuoteRequest{Invoice: "x"})
	require.Error(t, err, "nothing is quoted while draining")
	require.NoError(t, srv.Stop())

	stuck := usable()
	stuck.Enabled = false
	stuck.Journal = journalWith(t, swap.Funded)
	stuck.SHA256RPCHost, stuck.SHA256MacaroonPath = "", ""
	srv, _, err = New(&stuck)
	require.NoError(t, err)
	require.NoError(t, srv.Start())
	require.False(t, srv.draining)
	status, err = srv.Status(context.Background(), &StatusRequest{})
	require.NoError(t, err)
	require.Contains(t, status.Refusals[len(status.Refusals)-1],
		"turn the bridge back on")
	require.NoError(t, srv.Stop())
}

// A draining bridge stops once its journal has nothing unfinished.
func TestADrainedBridgeStops(t *testing.T) {
	cfg := usable()
	svc := serviceFor(t, cfg, &fakeNode{synced: true}, remote(nil, nil, nil))

	old := drainCheckInterval
	drainCheckInterval = time.Millisecond
	t.Cleanup(func() { drainCheckInterval = old })

	srv, _, err := New(&cfg)
	require.NoError(t, err)
	srv.draining = true
	srv.quit = make(chan struct{})
	srv.mu.Lock()
	srv.svc = svc
	srv.mu.Unlock()

	srv.wg.Add(1)
	go srv.watchDrain()
	require.Eventually(t, func() bool {
		return srv.service() == nil
	}, 5*time.Second, 5*time.Millisecond)
	srv.mu.RLock()
	require.True(t, srv.drained)
	srv.mu.RUnlock()
	close(srv.quit)
	srv.wg.Wait()
}

// A certificate the SHA256 node renewed is dialled with: the bridge rebuilds
// its connection rather than fail every call until a restart.
func TestANewTLSCertificateIsDialledWith(t *testing.T) {
	cfg := usable()
	cfg.SHA256TLSCertPath = filepath.Join(t.TempDir(), "tls.cert")
	require.NoError(t, os.WriteFile(cfg.SHA256TLSCertPath, []byte("one"), 0600))
	svc := serviceFor(t, cfg, &fakeNode{synced: true}, remote(nil, nil, nil))

	old := chainRecheckInterval
	chainRecheckInterval = time.Millisecond
	t.Cleanup(func() { chainRecheckInterval = old })

	srv, _, err := New(&cfg)
	require.NoError(t, err)
	srv.quit = make(chan struct{})
	srv.mu.Lock()
	srv.svc = svc
	srv.mu.Unlock()

	srv.wg.Add(1)
	go srv.watchChain(svc, remote(nil, nil, nil),
		certFingerprint(cfg.SHA256TLSCertPath))

	// Unchanged: left alone.
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, svc, srv.service())

	require.NoError(t, os.WriteFile(cfg.SHA256TLSCertPath, []byte("two"), 0600))
	require.Eventually(t, func() bool {
		return srv.service() != svc
	}, 5*time.Second, 5*time.Millisecond)

	close(srv.quit)
	srv.wg.Wait()
}

// A direction turned off with swaps unfinished is built to finish them and
// quotes nothing; with none unfinished it is not built at all.
func TestAnOffDirectionFinishesWhatIsUnfinished(t *testing.T) {
	t.Parallel()

	cfg := usable()
	cfg.ToBLAKE2b = false
	cfg.Journal = journalWith(t, swap.Funded)
	svc, err := newService(&cfg, NewLocal((&fakeNode{synced: true}).deps()),
		remote(nil, nil, nil))
	require.NoError(t, err)
	t.Cleanup(svc.close)
	require.Len(t, svc.sides, 2)
	require.True(t, svc.sides[0].quoting())
	require.Equal(t, "toBLAKE2b", svc.sides[1].name)
	require.False(t, svc.sides[1].quoting())
	require.Equal(t, []string{"toBLAKE2b"}, svc.disabled)

	cfg.Journal = journalWith(t, swap.Settled)
	svc, err = newService(&cfg, NewLocal((&fakeNode{synced: true}).deps()),
		remote(nil, nil, nil))
	require.NoError(t, err)
	t.Cleanup(svc.close)
	require.Len(t, svc.sides, 1)

	// Routed to, for the swaps it finishes; refused, for a new one.
	routed := sidedService(t, 10_000_000)
	routed.sides[1].finishing = true
	sd, _, err := routed.route(context.Background(), "sideB-payme")
	require.NoError(t, err)
	require.Equal(t, "toBLAKE2b", sd.name)
	_, _, err = routed.routeQuote(context.Background(), "sideB-payme")
	require.Equal(t, quote.CodeNoDirection, quote.CodeOf(err))
	require.Contains(t, err.Error(), "configured but not enabled")
	sd, _, err = routed.routeQuote(context.Background(), "sideA-payme")
	require.NoError(t, err)
	require.Equal(t, "toSHA256", sd.name)
}
