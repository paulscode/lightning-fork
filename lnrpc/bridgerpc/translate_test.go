package bridgerpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/routing/route"
)

// htlcs builds an invoice HTLC set from (state, expiry) pairs. The circuit
// keys only have to be distinct.
func htlcs(pairs ...struct {
	state  invoices.HtlcState
	expiry uint32
}) map[invoices.CircuitKey]*invoices.InvoiceHTLC {

	out := make(map[invoices.CircuitKey]*invoices.InvoiceHTLC, len(pairs))
	for i, p := range pairs {
		key := invoices.CircuitKey{HtlcID: uint64(i + 1)}
		out[key] = &invoices.InvoiceHTLC{
			State:  p.state,
			Expiry: p.expiry,
		}
	}

	return out
}

func accepted(expiry uint32) struct {
	state  invoices.HtlcState
	expiry uint32
} {
	return struct {
		state  invoices.HtlcState
		expiry uint32
	}{invoices.HtlcStateAccepted, expiry}
}

func htlcIn(state invoices.HtlcState, expiry uint32) struct {
	state  invoices.HtlcState
	expiry uint32
} {
	return struct {
		state  invoices.HtlcState
		expiry uint32
	}{state, expiry}
}

func TestTranslateInvoiceState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   invoices.ContractState
		want InvoiceState
	}{
		{"open", invoices.ContractOpen, InvoiceOpen},
		{"accepted", invoices.ContractAccepted, InvoiceAccepted},
		{"settled", invoices.ContractSettled, InvoiceSettled},
		{"cancelled", invoices.ContractCanceled, InvoiceCancelled},
		{
			// A state this build does not know must not be read as
			// claimable: paying out against one would authorise a
			// payment for an HTLC that may not be there.
			name: "a state from the future is not claimable",
			in:   invoices.ContractState(200),
			want: InvoiceCancelled,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := TranslateInvoice(&invoices.Invoice{
				State: test.in,
				Terms: invoices.ContractTerm{Value: 1000},
			})
			if got.State != test.want {
				t.Errorf("got %v, wanted %v", got.State,
					test.want)
			}
			if got.AmountMsat != 1000 {
				t.Errorf("amount %d, wanted 1000",
					got.AmountMsat)
			}
		})
	}
}

// A nil invoice must not read as claimable either.
func TestTranslateInvoiceNil(t *testing.T) {
	t.Parallel()

	got := TranslateInvoice(nil)
	if got.State != InvoiceCancelled {
		t.Errorf("a nil invoice translated to %v, which is claimable",
			got.State)
	}
	if got.State.claimable() {
		t.Error("a nil invoice must never be claimable")
	}
}

// claimable mirrors what the bridge does with the state, so the tests can
// assert on the property that matters rather than on the enum.
func (s InvoiceState) claimable() bool { return s == InvoiceAccepted }

// The expiry height is the minimum across accepted HTLCs. Reporting anything
// later tells the margin check there is more time than there is, which is the
// direction that pays out against a claim that can no longer be collected.
func TestEarliestAcceptedExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		htlcs map[invoices.CircuitKey]*invoices.InvoiceHTLC
		want  int32
	}{
		{
			name: "nothing locked in",
			want: 0,
		},
		{
			name:  "one accepted",
			htlcs: htlcs(accepted(800_100)),
			want:  800_100,
		},
		{
			name: "multi-part takes the earliest, not the last",
			htlcs: htlcs(
				accepted(800_500), accepted(800_100),
				accepted(800_900),
			),
			want: 800_100,
		},
		{
			name: "a cancelled htlc bounds nothing",
			htlcs: htlcs(
				htlcIn(invoices.HtlcStateCanceled, 800_001),
				accepted(800_400),
			),
			want: 800_400,
		},
		{
			name: "a settled htlc bounds nothing",
			htlcs: htlcs(
				htlcIn(invoices.HtlcStateSettled, 800_002),
				accepted(800_400),
			),
			want: 800_400,
		},
		{
			name: "only resolved htlcs is the same as none",
			htlcs: htlcs(
				htlcIn(invoices.HtlcStateCanceled, 800_001),
				htlcIn(invoices.HtlcStateSettled, 800_002),
			),
			want: 0,
		},
		{
			// A zero carries no information. Letting it through
			// would report "no time left" for an HTLC with plenty
			// and cancel a swap that was fine.
			name:  "a zero expiry is ignored, not treated as soonest",
			htlcs: htlcs(accepted(0), accepted(800_400)),
			want:  800_400,
		},
		{
			// Truncating to int32 would make this negative, which
			// would read as the earliest of all and cancel
			// everything.
			name:  "a height that does not fit is ignored",
			htlcs: htlcs(accepted(1<<31), accepted(800_400)),
			want:  800_400,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := TranslateInvoice(&invoices.Invoice{
				State: invoices.ContractAccepted,
				Htlcs: test.htlcs,
			})
			if got.ExpiryHeight != test.want {
				t.Errorf("expiry height %d, wanted %d",
					got.ExpiryHeight, test.want)
			}
		})
	}
}

// A nil entry in the map must not panic: this runs while an HTLC is holding
// someone's money, and a panic here takes the node down with it.
func TestEarliestAcceptedExpirySurvivesANilHTLC(t *testing.T) {
	t.Parallel()

	set := htlcs(accepted(800_400))
	set[invoices.CircuitKey{HtlcID: 99}] = nil

	got := TranslateInvoice(&invoices.Invoice{
		State: invoices.ContractAccepted,
		Htlcs: set,
	})
	if got.ExpiryHeight != 800_400 {
		t.Errorf("expiry height %d, wanted 800400", got.ExpiryHeight)
	}
}

// fakePayment is a DBMPPayment that answers only what TranslatePayment asks.
type fakePayment struct {
	paymentsdb.DBMPPayment

	terminated bool
	settled    *paymentsdb.HTLCAttempt
}

func (f *fakePayment) Terminated() bool { return f.terminated }

func (f *fakePayment) TerminalInfo() (*paymentsdb.HTLCAttempt,
	*paymentsdb.FailureReason) {

	return f.settled, nil
}

// settledAttempt builds a terminal attempt carrying a preimage and a route
// with a known fee.
func settledAttempt(pre [32]byte, feeMsat int64) *paymentsdb.HTLCAttempt {
	return &paymentsdb.HTLCAttempt{
		HTLCAttemptInfo: paymentsdb.HTLCAttemptInfo{
			Route: route.Route{
				TotalAmount: lnwire.MilliSatoshi(1000 + feeMsat),
				Hops: []*route.Hop{
					{AmtToForward: lnwire.MilliSatoshi(1000)},
				},
			},
		},
		Settle: &paymentsdb.HTLCSettleInfo{Preimage: pre},
	}
}

func TestTranslatePayment(t *testing.T) {
	t.Parallel()

	var pre [32]byte
	pre[0] = 0xab

	tests := []struct {
		name string
		in   paymentsdb.DBMPPayment
		want PaymentStatus
	}{
		{
			// "Never seen" is an answer, and the one that lets the
			// bridge cancel an incoming claim safely.
			name: "no record at all",
			in:   nil,
			want: PaymentStatus{Known: false},
		},
		{
			name: "a typed nil is still no record",
			in:   (*fakePayment)(nil),
			want: PaymentStatus{Known: false},
		},
		{
			// The dangerous case: still trying is not failure.
			// Concluding failure here cancels a claim against an
			// HTLC that is still in flight.
			name: "in flight is not failure",
			in:   &fakePayment{terminated: false},
			want: PaymentStatus{Known: true, InFlight: true},
		},
		{
			name: "terminated and unsettled is failure",
			in:   &fakePayment{terminated: true},
			want: PaymentStatus{Known: true},
		},
		{
			name: "settled carries the preimage",
			in: &fakePayment{
				terminated: true,
				settled:    settledAttempt(pre, 7),
			},
			want: PaymentStatus{
				Known: true, Settled: true, Preimage: pre,
				FeeMsat: 7,
			},
		},
		{
			// A settled attempt is settled whatever the terminated
			// flag says. Reading the flag first would lose the
			// preimage, which is the entire proceeds of the swap.
			name: "settled wins over a stale terminated flag",
			in: &fakePayment{
				terminated: false,
				settled:    settledAttempt(pre, 0),
			},
			want: PaymentStatus{
				Known: true, Settled: true, Preimage: pre,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := TranslatePayment(test.in)
			if got != test.want {
				t.Errorf("got %+v, wanted %+v", got, test.want)
			}
		})
	}
}

// A terminal attempt with no settle info is not a success. Reporting one would
// move the swap to a state whose only action, claiming with the preimage,
// cannot be carried out.
func TestTranslatePaymentTerminalWithoutSettleIsNotSuccess(t *testing.T) {
	t.Parallel()

	got := TranslatePayment(&fakePayment{
		terminated: true,
		settled:    &paymentsdb.HTLCAttempt{},
	})
	if got.Settled {
		t.Error("an attempt with no settle info read as settled")
	}
	if got.InFlight {
		t.Error("a terminated payment read as in flight")
	}
}
