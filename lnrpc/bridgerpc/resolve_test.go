//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/store"
	"github.com/paulscode/lightning-fork-bridge/swap"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lookupOut is a paying node that reads "sideA-" invoices as the SHA256
// chain's and says what became of a payment.
type lookupOut struct {
	*fakeOutDecoder

	pmt node.Payment

	mu     sync.Mutex
	asked  int
	looked node.Hash
}

func (o *lookupOut) LookupPayment(_ context.Context, hash node.Hash) (
	node.Payment, error) {

	o.mu.Lock()
	defer o.mu.Unlock()
	o.asked++
	o.looked = hash

	return o.pmt, nil
}

func (o *lookupOut) lookups() (int, node.Hash) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.asked, o.looked
}

// resolveFixture is a server whose toSHA256 swap h was stopped in paying,
// with the payer's payment held on this node. settled and cancelled receive
// what the runner does to that payment once the swap is resolved.
type resolveFixture struct {
	srv       *Server
	svc       *service
	out       *lookupOut
	pre       node.Preimage
	hash      node.Hash
	settled   chan node.Preimage
	cancelled chan node.Hash
}

func newResolveFixture(t *testing.T, pmt node.Payment) *resolveFixture {
	t.Helper()

	return newStoppedFixture(t, pmt, swap.Paying, true, nil)
}

// newStoppedFixture is a swap the runner stopped in state, whose hold invoice
// the incoming node has (invoiceKnown) or has deleted.
func newStoppedFixture(t *testing.T, pmt node.Payment, state swap.State,
	invoiceKnown bool, lookupErr error) *resolveFixture {

	t.Helper()

	fx := &resolveFixture{
		settled:   make(chan node.Preimage, 1),
		cancelled: make(chan node.Hash, 1),
	}
	fx.pre[0] = 7
	fx.hash = fx.pre.Hash()

	f := &fakeNode{synced: true}
	deps := f.deps()
	deps.LookupInvoice = func(context.Context, [32]byte) (InvoiceStatus,
		bool, error) {

		return InvoiceStatus{
			State: InvoiceAccepted, AmountMsat: 1000,
			ExpiryHeight: 500,
		}, invoiceKnown, lookupErr
	}
	deps.SettleInvoice = func(_ context.Context, pre [32]byte) error {
		select {
		case fx.settled <- pre:
		default:
		}

		return nil
	}
	deps.CancelInvoice = func(_ context.Context, hash [32]byte) error {
		select {
		case fx.cancelled <- hash:
		default:
		}

		return nil
	}

	cfg := usable()
	cfg.Journal = filepath.Join(t.TempDir(), "swaps.journal")
	svc, err := newService(&cfg, NewLocal(deps), remote(nil, nil, nil))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	svc.ctx = ctx
	t.Cleanup(func() {
		cancel()
		svc.wg.Wait()
		svc.close()
	})

	fx.out = &lookupOut{
		fakeOutDecoder: &fakeOutDecoder{accepts: "sideA-"},
		pmt:            pmt,
	}
	for _, sd := range svc.sides {
		if sd.name == "toSHA256" {
			sd.out = fx.out
			sd.runner.Driver.Out = fx.out
		}
	}

	now := time.Now()
	rec := store.Record{
		Hash: fx.hash, State: state, Binding: swap.HashLinked,
		Invoice: "sideA-payout", IncomingMsat: 1000,
		OutgoingMsat: 900, OutgoingCLTVLimit: 80,
		Created: now, Updated: now,
	}
	if state == swap.Paid {
		rec.Preimage = fx.pre
		rec.OutgoingPreimage = fx.pre
	}
	require.NoError(t, svc.journal.Put(ctx, rec))
	svc.markStopped(fx.hash, "the paying node has no record")

	fx.svc = svc
	fx.srv = serverWith(t, true, f)
	fx.srv.svc = svc

	return fx
}

func (fx *resolveFixture) resolve(t *testing.T) (*Swap, error) {
	t.Helper()

	return fx.srv.ResolveSwap(context.Background(),
		&ResolveSwapRequest{Hash: fx.hash[:]})
}

func (fx *resolveFixture) state(t *testing.T) swap.State {
	t.Helper()

	rec, err := fx.svc.journal.Get(context.Background(), fx.hash)
	require.NoError(t, err)

	return rec.State
}

// No record of the payment, once the operator has ruled out a restored or
// edited node: it never left, so the payer is refunded.
func TestResolveAPaymentTheNodeNeverRecordedRefunds(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, node.Payment{State: node.PaymentNone})

	got, err := fx.resolve(t)
	require.NoError(t, err)
	require.Equal(t, swap.Failing.String(), got.State)
	_, looked := fx.out.lookups()
	require.Equal(t, fx.hash, looked)

	select {
	case h := <-fx.cancelled:
		require.Equal(t, fx.hash, h)
	case <-time.After(10 * time.Second):
		t.Fatal("the payer's held payment was not cancelled")
	}
	require.Empty(t, fx.svc.needsOperator(context.Background()))
}

func TestResolveAFailedPaymentRefunds(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, node.Payment{State: node.PaymentFailed})

	got, err := fx.resolve(t)
	require.NoError(t, err)
	require.Equal(t, swap.Failing.String(), got.State)

	select {
	case <-fx.cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the payer's held payment was not cancelled")
	}
}

// A payment that went through after all is claimed with its preimage, never
// refunded.
func TestResolveASucceededPaymentSettles(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, node.Payment{
		State: node.PaymentSucceeded, Preimage: node.Preimage{7},
	})

	got, err := fx.resolve(t)
	require.NoError(t, err)
	require.Equal(t, swap.Paid.String(), got.State)
	require.Equal(t, fx.pre[:], got.Preimage)

	select {
	case pre := <-fx.settled:
		require.Equal(t, fx.pre, node.Preimage(pre))
	case <-time.After(10 * time.Second):
		t.Fatal("the payer's held payment was not settled")
	}
	select {
	case <-fx.cancelled:
		t.Fatal("a paid swap's incoming payment was cancelled")
	default:
	}
}

// Still in flight, the payment may go either way: nothing is decided.
func TestResolveRefusesAPaymentStillInFlight(t *testing.T) {
	t.Parallel()

	fx := newResolveFixture(t, node.Payment{State: node.PaymentInFlight})

	_, err := fx.resolve(t)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "in_flight")
	require.Equal(t, swap.Paying, fx.state(t))
	require.NotEmpty(t, fx.svc.needsOperator(context.Background()))
}

// Only a swap the bridge stopped in paying: one it is still driving decides
// itself, and one in another state has nothing to resolve.
func TestResolveRefusesASwapNotStoppedInPaying(t *testing.T) {
	t.Parallel()

	t.Run("still being driven", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, node.Payment{State: node.PaymentNone})
		fx.svc.stoppedMu.Lock()
		delete(fx.svc.stopped, fx.hash)
		fx.svc.stoppedMu.Unlock()

		_, err := fx.resolve(t)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.Contains(t, err.Error(), "not_stopped")
		require.Equal(t, swap.Paying, fx.state(t))
		asked, _ := fx.out.lookups()
		require.Zero(t, asked)
	})

	// Stopped on its record in another state: refused, saying a restart
	// drives it again rather than that it is not stopped.
	t.Run("stopped, not paying", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, node.Payment{State: node.PaymentNone})
		rec, err := fx.svc.journal.Get(context.Background(), fx.hash)
		require.NoError(t, err)
		rec.State = swap.Funded
		require.NoError(t, fx.svc.journal.Put(context.Background(), rec))

		_, err = fx.resolve(t)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.Contains(t, err.Error(), "stopped in funded")
		require.Contains(t, err.Error(), "when it restarts")
		require.Equal(t, swap.Funded, fx.state(t))
		require.NotEmpty(t, fx.svc.needsOperator(context.Background()))
	})

	t.Run("not stopped, not paying", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, node.Payment{State: node.PaymentNone})
		fx.svc.stoppedMu.Lock()
		delete(fx.svc.stopped, fx.hash)
		fx.svc.stoppedMu.Unlock()
		rec, err := fx.svc.journal.Get(context.Background(), fx.hash)
		require.NoError(t, err)
		rec.State = swap.Funded
		require.NoError(t, fx.svc.journal.Put(context.Background(), rec))

		_, err = fx.resolve(t)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.Contains(t, err.Error(), "it is funded")
		require.Equal(t, swap.Funded, fx.state(t))
	})

	t.Run("unknown", func(t *testing.T) {
		t.Parallel()

		fx := newResolveFixture(t, node.Payment{State: node.PaymentNone})
		_, err := fx.srv.ResolveSwap(context.Background(),
			&ResolveSwapRequest{Hash: make([]byte, 32)})
		require.Equal(t, codes.NotFound, status.Code(err))
	})
}

// Resolving settles or cancels a payer's payment on the operator's word, so a
// participant's macaroon, or any that cannot make macaroons, must not reach it.
func TestResolveIsTheOperatorsAlone(t *testing.T) {
	t.Parallel()

	ops := macPermissions["/bridgerpc.Bridge/ResolveSwap"]
	var generate bool
	for _, op := range ops {
		if op.Entity == "macaroon" && op.Action == "generate" {
			generate = true
		}
	}
	require.True(t, generate, "ResolveSwap must need macaroon:generate")
}

// A swap driven from a call that straddles a stop (a resolve, or a quote,
// finishing while a rebuild stops the service) is left for the next start
// rather than started: stop's wait would not cover it, and adding to the
// WaitGroup while stop waits on it can panic.
func TestADriveStraddlingStopIsNotStarted(t *testing.T) {
	t.Parallel()

	_, svc := measuredServer(t, 100_000_000_000)
	svc.ctx, svc.cancel = context.WithCancel(context.Background())
	hash := node.Hash{0x5f}
	svc.markAlerted(hash, "raised while driving")

	// A swap in flight, which stop waits for.
	svc.wg.Add(1)
	stopped := make(chan struct{})
	go func() {
		svc.stop()
		close(stopped)
	}()
	<-svc.ctx.Done()

	svc.drive(svc.sides[0], hash)
	svc.wg.Done()
	<-stopped

	// A drive that ran would have forgotten the alert when it ended,
	// before stop returned.
	svc.stoppedMu.Lock()
	defer svc.stoppedMu.Unlock()
	require.Contains(t, svc.alerted, hash, "a swap was driven on a "+
		"service that was stopping")
}

// Drives racing a stop, under -race: none is added to the WaitGroup while
// stop waits on it.
func TestDrivesRacingStop(t *testing.T) {
	t.Parallel()

	for i := 0; i < 10; i++ {
		_, svc := measuredServer(t, 100_000_000_000)
		svc.ctx, svc.cancel = context.WithCancel(context.Background())

		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				svc.drive(svc.sides[j%len(svc.sides)],
					node.Hash{byte(i), byte(j)})
			}()
		}
		svc.stop()
		wg.Wait()
	}
}

// TestResolveAPaidSwapWhoseInvoiceIsGoneIsLost: the operator has ruled out
// another node and a restored database, and the node still has no record of
// the hold invoice, so the payout cannot be claimed back.
func TestResolveAPaidSwapWhoseInvoiceIsGoneIsLost(t *testing.T) {
	t.Parallel()

	fx := newStoppedFixture(t, node.Payment{}, swap.Paid, false, nil)

	got, err := fx.resolve(t)
	require.NoError(t, err)
	require.Equal(t, swap.Lost.String(), got.State)
	require.Equal(t, swap.Lost, fx.state(t))

	asked, _ := fx.out.lookups()
	require.Zero(t, asked, "a paid swap asks the incoming node, not the "+
		"paying one")

	ops := fx.svc.needsOperator(context.Background())
	require.Len(t, ops, 1)
	require.Contains(t, ops[0], "lost")

	// Decided once: a second ask is refused, and the swap stays lost.
	_, err = fx.resolve(t)
	require.Error(t, err)
	require.Equal(t, swap.Lost, fx.state(t))
}

// TestResolveAPaidSwapChecksItsIssuer: "no record" decides a paid swap only
// from the node that issued its hold invoice. Read from another, the swap is
// left stopped and the operator told which node to point back at.
func TestResolveAPaidSwapChecksItsIssuer(t *testing.T) {
	t.Parallel()

	issued := func(fx *resolveFixture, key string) {
		rec, err := fx.svc.journal.Get(context.Background(), fx.hash)
		require.NoError(t, err)
		rec.IncomingNode = key
		require.NoError(t, fx.svc.journal.Put(context.Background(), rec))
	}

	t.Run("its issuer", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Paid, false, nil)
		issued(fx, "02"+strings.Repeat("11", 32))

		_, err := fx.resolve(t)
		require.NoError(t, err)
		require.Equal(t, swap.Lost, fx.state(t))
	})

	t.Run("another node", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Paid, false, nil)
		other := "03" + strings.Repeat("cd", 32)
		issued(fx, other)

		_, err := fx.resolve(t)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		require.Contains(t, err.Error(), "wrong_node")
		require.Contains(t, err.Error(), other)
		require.Equal(t, swap.Paid, fx.state(t))

		ops := fx.svc.needsOperator(context.Background())
		require.Len(t, ops, 1)
		require.Contains(t, ops[0], "stopped")
	})
}

// TestResolveAPaidSwapLeavesItStoppedWhenTheNodeDoesNotAnswer: no answer is
// not "no record", so nothing is decided.
func TestResolveAPaidSwapLeavesItStoppedWhenTheNodeDoesNotAnswer(t *testing.T) {
	t.Parallel()

	fx := newStoppedFixture(t, node.Payment{}, swap.Paid, true,
		errors.New("connection refused"))

	_, err := fx.resolve(t)
	require.Error(t, err)
	require.Equal(t, swap.Paid, fx.state(t))

	ops := fx.svc.needsOperator(context.Background())
	require.Len(t, ops, 1)
	require.Contains(t, ops[0], "stopped")
}

// TestResolveAPaidSwapWhoseInvoiceIsBackSettles: the node has the invoice
// after all, so the runner takes the swap back and claims it.
func TestResolveAPaidSwapWhoseInvoiceIsBackSettles(t *testing.T) {
	t.Parallel()

	fx := newStoppedFixture(t, node.Payment{}, swap.Paid, true, nil)

	got, err := fx.resolve(t)
	require.NoError(t, err)
	require.Equal(t, swap.Paid.String(), got.State)

	select {
	case pre := <-fx.settled:
		require.Equal(t, fx.pre, node.Preimage(pre))
	case <-time.After(10 * time.Second):
		t.Fatal("the payer's payment was not settled")
	}
	require.Empty(t, fx.svc.needsOperator(context.Background()))
}

// TestResolveIssuerGone: the operator says the node that issued a stopped
// swap's hold invoice is gone for good. Checked first, then the swap is
// decided without it: given back if it never paid out, lost if it did.
func TestResolveIssuerGone(t *testing.T) {
	t.Parallel()

	here := "02" + strings.Repeat("11", 32)
	gone := "03" + strings.Repeat("cd", 32)
	issued := func(t *testing.T, fx *resolveFixture, key string) {
		rec, err := fx.svc.journal.Get(context.Background(), fx.hash)
		require.NoError(t, err)
		rec.IncomingNode = key
		require.NoError(t, fx.svc.journal.Put(context.Background(), rec))
	}
	resolveGone := func(fx *resolveFixture) (*Swap, error) {
		return fx.srv.ResolveSwap(context.Background(),
			&ResolveSwapRequest{Hash: fx.hash[:], IssuerGone: true})
	}

	t.Run("offered, issuer gone", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Offered, false,
			nil)
		issued(t, fx, gone)

		// Without the flag, resolve says how to decide it.
		_, err := fx.resolve(t)
		require.Contains(t, err.Error(), "issuer_gone")

		_, err = resolveGone(fx)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return fx.state(t) == swap.Expired
		}, 10*time.Second, 50*time.Millisecond)
		rec, err := fx.svc.journal.Get(context.Background(), fx.hash)
		require.NoError(t, err)
		require.Empty(t, rec.IncomingNode)
		require.Contains(t, rec.Note, gone)
		require.Empty(t, fx.svc.needsOperator(context.Background()))
	})

	t.Run("paid, issuer gone", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Paid, false, nil)
		issued(t, fx, gone)

		got, err := resolveGone(fx)
		require.NoError(t, err)
		require.Equal(t, swap.Lost.String(), got.State)
		require.Equal(t, swap.Lost, fx.state(t))
	})

	t.Run("the bridge reads the issuer", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Offered, false,
			nil)
		issued(t, fx, here)

		_, err := resolveGone(fx)
		require.Contains(t, err.Error(), "issuer_here")
		require.Equal(t, swap.Offered, fx.state(t))
	})

	t.Run("the node read has the invoice", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Funded, true, nil)
		issued(t, fx, gone)

		_, err := resolveGone(fx)
		require.Contains(t, err.Error(), "issuer_here")
		require.Equal(t, swap.Funded, fx.state(t))
	})

	t.Run("still being driven", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Offered, false,
			nil)
		issued(t, fx, gone)
		fx.svc.stoppedMu.Lock()
		delete(fx.svc.stopped, fx.hash)
		fx.svc.stoppedMu.Unlock()

		_, err := resolveGone(fx)
		require.Contains(t, err.Error(), "not_stopped")
		require.Equal(t, swap.Offered, fx.state(t))
	})

	t.Run("no issuer recorded", func(t *testing.T) {
		t.Parallel()

		fx := newStoppedFixture(t, node.Payment{}, swap.Paid, false, nil)

		_, err := resolveGone(fx)
		require.Contains(t, err.Error(), "not_stopped")
		require.Equal(t, swap.Paid, fx.state(t))
	})
}
