//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// fakeNode is a Deps whose answers each test sets.
type fakeNode struct {
	invoices map[[32]byte]InvoiceStatus

	settleErr error
	cancelErr error

	// settles and cancels count the calls that reached the node, so a test
	// can tell "swallowed the error" from "never asked".
	settles, cancels int

	height int32
	synced bool
	htErr  error

	payStatus PaymentStatus
	payErr    error

	lookup    PaymentStatus
	lookupErr error
}

func (f *fakeNode) deps() *Deps {
	return &Deps{
		AddHoldInvoice: func(context.Context, HoldInvoiceRequest) (
			string, error) {

			return "lnbcrt1payme", nil
		},
		LookupInvoice: func(_ context.Context, hash [32]byte) (
			InvoiceStatus, bool, error) {

			st, ok := f.invoices[hash]

			return st, ok, nil
		},
		SettleInvoice: func(_ context.Context, pre [32]byte) error {
			f.settles++

			return f.settleErr
		},
		CancelInvoice: func(_ context.Context, _ [32]byte) error {
			f.cancels++

			return f.cancelErr
		},
		// Always present so that ready() passes; the tests that care
		// about decoding replace it.
		DecodeInvoice: func(context.Context, string) (*zpay32.Invoice,
			error) {

			return nil, errors.New("not used in this test")
		},
		PayInvoice: func(context.Context, PayRequest) (PaymentStatus,
			error) {

			return f.payStatus, f.payErr
		},
		LookupPayment: func(context.Context, [32]byte) (PaymentStatus,
			error) {

			return f.lookup, f.lookupErr
		},
		BlockHeight: func(context.Context) (int32, bool, error) {
			return f.height, f.synced, f.htErr
		},
	}
}

// local builds an adapter over the fake.
func local(f *fakeNode) *Local {
	return NewLocal(f.deps())
}

func hash8(b byte) [32]byte {
	var h [32]byte
	h[0] = b

	return h
}

// A node that has not caught up must refuse to report a height. Every margin
// decision compares against it, and a stale one says an HTLC has more time
// left than it does.
func TestBlockHeightRefusesWhileSyncing(t *testing.T) {
	t.Parallel()

	l := local(&fakeNode{height: 800_000, synced: false})

	got, err := l.BlockHeight(context.Background())
	if !errors.Is(err, ErrNotSynced) {
		t.Fatalf("wanted ErrNotSynced, got %v (height %d)", err, got)
	}
	if got != 0 {
		t.Errorf("a refused height should be zero, got %d", got)
	}
}

func TestBlockHeightWhenSynced(t *testing.T) {
	t.Parallel()

	l := local(&fakeNode{height: 800_000, synced: true})

	got, err := l.BlockHeight(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 800_000 {
		t.Errorf("got %d, wanted 800000", got)
	}
}

// After a crash the bridge reissues a settle. The reissue arriving at a node
// that already did it must not read as a problem.
func TestSettleIsIdempotent(t *testing.T) {
	t.Parallel()

	var pre node.Preimage
	pre[0] = 0x11

	f := &fakeNode{
		settleErr: errors.New("invoice already settled"),
		invoices: map[[32]byte]InvoiceStatus{
			pre.Hash(): {State: InvoiceSettled},
		},
	}

	if err := local(f).SettleInvoice(context.Background(), pre); err != nil {
		t.Fatalf("settling an already settled invoice: %v", err)
	}
	if f.settles != 1 {
		t.Errorf("the node was asked %d times, wanted once", f.settles)
	}
}

// The idempotency must come from the invoice's state, not from the error text.
// A node that rephrases its error must not turn a settled swap into one that
// retries forever, and one that is genuinely failing must not be swallowed.
func TestSettleFailureIsNotSwallowedWhenTheInvoiceIsNotSettled(t *testing.T) {
	t.Parallel()

	var pre node.Preimage
	pre[0] = 0x22

	f := &fakeNode{
		settleErr: errors.New("invoice already settled"),
		invoices: map[[32]byte]InvoiceStatus{
			// The node said "already settled" and the invoice is
			// accepted. The error wins.
			pre.Hash(): {State: InvoiceAccepted},
		},
	}

	err := local(f).SettleInvoice(context.Background(), pre)
	if err == nil {
		t.Fatal("a settle that did not settle must not report success")
	}
}

func TestCancelIsIdempotent(t *testing.T) {
	t.Parallel()

	h := hash8(3)
	f := &fakeNode{
		cancelErr: errors.New("invoice already canceled"),
		invoices: map[[32]byte]InvoiceStatus{
			h: {State: InvoiceCancelled},
		},
	}

	if err := local(f).CancelInvoice(context.Background(), h); err != nil {
		t.Fatalf("cancelling an already cancelled invoice: %v", err)
	}
}

func TestCancelFailureIsNotSwallowed(t *testing.T) {
	t.Parallel()

	h := hash8(4)
	f := &fakeNode{
		cancelErr: errors.New("nope"),
		invoices: map[[32]byte]InvoiceStatus{
			h: {State: InvoiceAccepted},
		},
	}

	if err := local(f).CancelInvoice(context.Background(), h); err == nil {
		t.Fatal("a cancel that did not cancel must not report success")
	}
}

// An unknown hash is its own answer, not a generic failure: the driver tells
// "never created" from "cannot say" by this error.
func TestLookupInvoiceUnknownHash(t *testing.T) {
	t.Parallel()

	l := local(&fakeNode{invoices: map[[32]byte]InvoiceStatus{}})

	_, err := l.LookupInvoice(context.Background(), hash8(9))
	if !errors.Is(err, node.ErrUnknownHash) {
		t.Fatalf("wanted ErrUnknownHash, got %v", err)
	}
}

// The bridge's contract refuses these three, each of which is a way of losing
// money rather than a validation nicety.
func TestAddHoldInvoiceRefusesUnpricedRequests(t *testing.T) {
	t.Parallel()

	good := node.HoldInvoice{
		Hash: hash8(1), AmountMsat: 1000, CLTVDelta: 400,
		Expiry: time.Minute,
	}

	tests := []struct {
		name string
		req  node.HoldInvoice
		want string
	}{
		{
			name: "no amount lets the payer choose",
			req: func() node.HoldInvoice {
				r := good
				r.AmountMsat = 0

				return r
			}(),
			want: "any amount",
		},
		{
			name: "no CLTV delta will not outlive the outgoing leg",
			req: func() node.HoldInvoice {
				r := good
				r.CLTVDelta = 0

				return r
			}(),
			want: "CLTV delta",
		},
		{
			name: "no expiry outlives the quote",
			req: func() node.HoldInvoice {
				r := good
				r.Expiry = 0

				return r
			}(),
			want: "expiry",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := local(&fakeNode{}).AddHoldInvoice(
				context.Background(), test.req,
			)
			if err == nil {
				t.Fatal("wanted a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("the refusal should say why: %v", err)
			}
		})
	}

	if _, err := local(&fakeNode{}).AddHoldInvoice(
		context.Background(), good,
	); err != nil {

		t.Errorf("a well-formed request was refused: %v", err)
	}
}

// The order of these tests is the whole of the mapping, so each is pinned.
func TestPaymentStateMapping(t *testing.T) {
	t.Parallel()

	var pre [32]byte
	pre[0] = 0x77

	tests := []struct {
		name string
		in   PaymentStatus
		want node.PaymentState
	}{
		{
			name: "no record is none, not failed",
			in:   PaymentStatus{Known: false},
			want: node.PaymentNone,
		},
		{
			name: "settled is success",
			in: PaymentStatus{
				Known: true, Settled: true, Preimage: pre,
			},
			want: node.PaymentSucceeded,
		},
		{
			name: "in flight is not failure",
			in:   PaymentStatus{Known: true, InFlight: true},
			want: node.PaymentInFlight,
		},
		{
			name: "known, not settled, not in flight is failure",
			in:   PaymentStatus{Known: true},
			want: node.PaymentFailed,
		},
		{
			// Settled wins over in flight. The preimage is the
			// proceeds of the swap and must not be lost to a
			// contradictory flag.
			name: "settled beats in flight",
			in: PaymentStatus{
				Known: true, Settled: true, InFlight: true,
				Preimage: pre,
			},
			want: node.PaymentSucceeded,
		},
		{
			// An unknown payment that somehow claims to be settled
			// is still unknown: Known is the gate.
			name: "not known beats everything",
			in: PaymentStatus{
				Known: false, Settled: true, Preimage: pre,
			},
			want: node.PaymentNone,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := payment(test.in)
			if got.State != test.want {
				t.Errorf("got %v, wanted %v", got.State,
					test.want)
			}
			if test.want == node.PaymentSucceeded &&
				got.Preimage != node.Preimage(test.in.Preimage) {

				t.Error("a success lost its preimage")
			}
		})
	}
}

// Pay must report in flight rather than error when the node gives up waiting,
// and LookupPayment must pass the same states through.
func TestPayReportsInFlightRatherThanFailing(t *testing.T) {
	t.Parallel()

	f := &fakeNode{
		payStatus: PaymentStatus{Known: true, InFlight: true},
	}

	got, err := local(f).Pay(context.Background(), node.PayRequest{
		Invoice: "lnbcrt1payme", CLTVLimit: 80, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != node.PaymentInFlight {
		t.Errorf("got %v, wanted in flight", got.State)
	}
}

// Every method must refuse rather than panic when the node handed over
// nothing. A nil call mid-swap takes the node down while it holds an HTLC.
func TestEveryMethodRefusesWithoutDeps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for name, call := range map[string]func(*Local) error{
		"AddHoldInvoice": func(l *Local) error {
			_, err := l.AddHoldInvoice(ctx, node.HoldInvoice{})

			return err
		},
		"LookupInvoice": func(l *Local) error {
			_, err := l.LookupInvoice(ctx, hash8(1))

			return err
		},
		"SettleInvoice": func(l *Local) error {
			return l.SettleInvoice(ctx, node.Preimage{})
		},
		"CancelInvoice": func(l *Local) error {
			return l.CancelInvoice(ctx, hash8(1))
		},
		"BlockHeight": func(l *Local) error {
			_, err := l.BlockHeight(ctx)

			return err
		},
		"Decode": func(l *Local) error {
			_, err := l.Decode(ctx, "lnbcrt1payme")

			return err
		},
		"Pay": func(l *Local) error {
			_, err := l.Pay(ctx, node.PayRequest{})

			return err
		},
		"LookupPayment": func(l *Local) error {
			_, err := l.LookupPayment(ctx, hash8(1))

			return err
		},
	} {
		t.Run(name+" with no deps at all", func(t *testing.T) {
			if err := call(NewLocal(nil)); !errors.Is(err, ErrNoDeps) {
				t.Errorf("wanted ErrNoDeps, got %v", err)
			}
		})
		t.Run(name+" with a gap in deps", func(t *testing.T) {
			if err := call(NewLocal(&Deps{})); !errors.Is(err, ErrNoDeps) {
				t.Errorf("wanted ErrNoDeps, got %v", err)
			}
		})
	}
}

// decodingAs builds an adapter whose decoder returns a fixed invoice, so the
// cases below test what this adapter does with one rather than zpay32's
// parsing, which has its own tests.
func decodingAs(inv *zpay32.Invoice, err error) *Local {
	f := &fakeNode{}
	d := f.deps()
	d.DecodeInvoice = func(context.Context, string) (*zpay32.Invoice, error) {
		return inv, err
	}

	return NewLocal(d)
}

func msat(v lnwire.MilliSatoshi) *lnwire.MilliSatoshi { return &v }

// A zero-amount invoice is refused rather than defaulted: the outgoing leg
// would be for whatever the bridge chose and the incoming one priced against a
// guess.
func TestDecodeRefusesAnInvoiceWithNoAmount(t *testing.T) {
	t.Parallel()

	h := hash8(5)
	for _, test := range []struct {
		name string
		amt  *lnwire.MilliSatoshi
	}{
		{"no amount field", nil},
		{"an explicit zero", msat(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			l := decodingAs(&zpay32.Invoice{
				PaymentHash: &h, MilliSat: test.amt,
				Timestamp: time.Now(),
			}, nil)

			_, err := l.Decode(context.Background(), "lnbcrt1payme")
			if err == nil {
				t.Fatal("wanted a refusal")
			}
			if !strings.Contains(err.Error(), "no amount") {
				t.Errorf("the refusal should say why: %v", err)
			}
		})
	}
}

// The payment hash must survive decoding exactly. The incoming hold invoice is
// built on it, and the swap's whole security is that the two are equal.
func TestDecodeCarriesTheHashAndAmount(t *testing.T) {
	t.Parallel()

	h := hash8(6)
	desc := "a swap"
	now := time.Now().Truncate(time.Second)

	l := decodingAs(&zpay32.Invoice{
		PaymentHash: &h, MilliSat: msat(150_000),
		Timestamp: now, Description: &desc,
	}, nil)

	got, err := l.Decode(context.Background(), "lnbcrt1payme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != node.Hash(h) {
		t.Errorf("hash %x, wanted %x", got.Hash, h)
	}
	if got.AmountMsat != 150_000 {
		t.Errorf("amount %d, wanted 150000", got.AmountMsat)
	}
	if got.Description != desc {
		t.Errorf("description %q, wanted %q", got.Description, desc)
	}
	if want := now.Add(zpay32.DefaultInvoiceExpiry); !got.Expiry.Equal(want) {
		t.Errorf("expiry %v, wanted %v", got.Expiry, want)
	}
	if got.CLTVDelta != uint32(zpay32.DefaultAssumedFinalCLTVDelta) {
		t.Errorf("CLTV delta %d, wanted the assumed default %d",
			got.CLTVDelta, zpay32.DefaultAssumedFinalCLTVDelta)
	}
}

// A decoder that returns neither an invoice nor an error must not be
// dereferenced: a panic here happens while the swap is being priced.
func TestDecodeRefusesNothingAtAll(t *testing.T) {
	t.Parallel()

	l := decodingAs(nil, nil)

	if _, err := l.Decode(context.Background(), "lnbcrt1payme"); err == nil {
		t.Fatal("a nil invoice with no error must be refused")
	}
}

func TestDecodePassesThroughTheDecoderError(t *testing.T) {
	t.Parallel()

	l := decodingAs(nil, errors.New("checksum failed"))

	_, err := l.Decode(context.Background(), "lnbcrt1nope")
	if err == nil || !strings.Contains(err.Error(), "checksum failed") {
		t.Fatalf("wanted the decoder's own error, got %v", err)
	}
}
