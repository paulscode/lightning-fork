package bridgerpc

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// The two sides of a swap must agree about what "claimable" means. These run
// the same cases through both translations, because a divergence between them
// is the kind of bug that only shows up with money on the line.

// bothInvoiceStates pairs an lnd registry state with the lnrpc state that means
// the same thing, so a single table can drive both translations.
var bothInvoiceStates = []struct {
	name   string
	local  invoices.ContractState
	remote lnrpc.Invoice_InvoiceState
	want   InvoiceState
}{
	{
		"open", invoices.ContractOpen, lnrpc.Invoice_OPEN,
		InvoiceOpen,
	},
	{
		"accepted", invoices.ContractAccepted, lnrpc.Invoice_ACCEPTED,
		InvoiceAccepted,
	},
	{
		"settled", invoices.ContractSettled, lnrpc.Invoice_SETTLED,
		InvoiceSettled,
	},
	{
		"cancelled", invoices.ContractCanceled, lnrpc.Invoice_CANCELED,
		InvoiceCancelled,
	},
	{
		// Neither side may let a node newer than this build talk it
		// into paying out.
		name:   "unknown is not claimable on either side",
		local:  invoices.ContractState(200),
		remote: lnrpc.Invoice_InvoiceState(200),
		want:   InvoiceCancelled,
	},
}

func TestBothSidesAgreeOnInvoiceState(t *testing.T) {
	t.Parallel()

	for _, test := range bothInvoiceStates {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			local := TranslateInvoice(&invoices.Invoice{
				State: test.local,
			})
			remote := TranslateRemoteInvoice(&lnrpc.Invoice{
				State: test.remote,
			})

			if local.State != test.want {
				t.Errorf("local: got %v, wanted %v",
					local.State, test.want)
			}
			if remote.State != test.want {
				t.Errorf("remote: got %v, wanted %v",
					remote.State, test.want)
			}
			if local.State != remote.State {
				t.Errorf("the two sides disagree: local %v, "+
					"remote %v", local.State, remote.State)
			}
		})
	}
}

// remoteHTLCs builds an lnrpc HTLC set from (state, expiry) pairs.
func remoteHTLCs(pairs ...struct {
	state  lnrpc.InvoiceHTLCState
	expiry int32
}) []*lnrpc.InvoiceHTLC {

	out := make([]*lnrpc.InvoiceHTLC, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, &lnrpc.InvoiceHTLC{
			State: p.state, ExpiryHeight: p.expiry,
		})
	}

	return out
}

func remoteAccepted(expiry int32) struct {
	state  lnrpc.InvoiceHTLCState
	expiry int32
} {
	return struct {
		state  lnrpc.InvoiceHTLCState
		expiry int32
	}{lnrpc.InvoiceHTLCState_ACCEPTED, expiry}
}

func remoteHTLCIn(state lnrpc.InvoiceHTLCState, expiry int32) struct {
	state  lnrpc.InvoiceHTLCState
	expiry int32
} {
	return struct {
		state  lnrpc.InvoiceHTLCState
		expiry int32
	}{state, expiry}
}

func TestEarliestRemoteAcceptedExpiry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		htlcs []*lnrpc.InvoiceHTLC
		want  int32
	}{
		{name: "nothing locked in", want: 0},
		{
			name:  "one accepted",
			htlcs: remoteHTLCs(remoteAccepted(800_100)),
			want:  800_100,
		},
		{
			name: "multi-part takes the earliest, not the last",
			htlcs: remoteHTLCs(
				remoteAccepted(800_500), remoteAccepted(800_100),
				remoteAccepted(800_900),
			),
			want: 800_100,
		},
		{
			name: "a cancelled htlc bounds nothing",
			htlcs: remoteHTLCs(
				remoteHTLCIn(
					lnrpc.InvoiceHTLCState_CANCELED,
					800_001,
				),
				remoteAccepted(800_400),
			),
			want: 800_400,
		},
		{
			name: "a settled htlc bounds nothing",
			htlcs: remoteHTLCs(
				remoteHTLCIn(
					lnrpc.InvoiceHTLCState_SETTLED, 800_002,
				),
				remoteAccepted(800_400),
			),
			want: 800_400,
		},
		{
			name:  "a zero expiry is ignored",
			htlcs: remoteHTLCs(remoteAccepted(0), remoteAccepted(800_400)),
			want:  800_400,
		},
		{
			// Possible here in a way it is not locally, since the
			// wire type is signed. It would otherwise read as the
			// earliest of all and cancel every swap.
			name: "a negative expiry is ignored, not soonest",
			htlcs: remoteHTLCs(
				remoteAccepted(-5), remoteAccepted(800_400),
			),
			want: 800_400,
		},
		{
			name: "a nil htlc does not panic",
			htlcs: append(
				remoteHTLCs(remoteAccepted(800_400)), nil,
			),
			want: 800_400,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := TranslateRemoteInvoice(&lnrpc.Invoice{
				State: lnrpc.Invoice_ACCEPTED,
				Htlcs: test.htlcs,
			})
			if got.ExpiryHeight != test.want {
				t.Errorf("expiry height %d, wanted %d",
					got.ExpiryHeight, test.want)
			}
		})
	}
}

func TestTranslateRemoteInvoiceNil(t *testing.T) {
	t.Parallel()

	if got := TranslateRemoteInvoice(nil); got.State != InvoiceCancelled {
		t.Errorf("a nil invoice translated to %v, which is claimable",
			got.State)
	}
}

func TestTranslateRemotePayment(t *testing.T) {
	t.Parallel()

	pre := strings.Repeat("ab", 32)
	var want [32]byte
	raw, _ := hex.DecodeString(pre)
	copy(want[:], raw)

	tests := []struct {
		name    string
		in      *lnrpc.Payment
		wantOut PaymentStatus
		wantErr bool
	}{
		{
			name:    "no record at all",
			in:      nil,
			wantOut: PaymentStatus{Known: false},
		},
		{
			name: "in flight",
			in: &lnrpc.Payment{
				Status: lnrpc.Payment_IN_FLIGHT,
			},
			wantOut: PaymentStatus{Known: true, InFlight: true},
		},
		{
			// Initiated means an attempt exists. Reading it as
			// terminal would cancel a claim against an HTLC that
			// may already have left.
			name: "initiated is in flight, not failed",
			in: &lnrpc.Payment{
				Status: lnrpc.Payment_INITIATED,
			},
			wantOut: PaymentStatus{Known: true, InFlight: true},
		},
		{
			name: "failed is terminal",
			in: &lnrpc.Payment{
				Status: lnrpc.Payment_FAILED, FeeMsat: 3,
			},
			wantOut: PaymentStatus{Known: true, FeeMsat: 3},
		},
		{
			name: "succeeded carries the preimage",
			in: &lnrpc.Payment{
				Status:          lnrpc.Payment_SUCCEEDED,
				PaymentPreimage: pre,
				FeeMsat:         11,
			},
			wantOut: PaymentStatus{
				Known: true, Settled: true, Preimage: want,
				FeeMsat: 11,
			},
		},
		{
			// A status this build does not know must keep the
			// bridge waiting rather than let it conclude failure.
			name: "an unknown status is in flight, not failed",
			in: &lnrpc.Payment{
				Status: lnrpc.Payment_PaymentStatus(99),
			},
			wantOut: PaymentStatus{Known: true, InFlight: true},
		},
		{
			name: "succeeded with no preimage is a contradiction",
			in: &lnrpc.Payment{
				Status: lnrpc.Payment_SUCCEEDED,
			},
			wantErr: true,
		},
		{
			name: "succeeded with a short preimage is a contradiction",
			in: &lnrpc.Payment{
				Status:          lnrpc.Payment_SUCCEEDED,
				PaymentPreimage: "abcd",
			},
			wantErr: true,
		},
		{
			// lnd reports all zeroes for a payment with no
			// preimage. It settles nothing, so passing it on would
			// look like proceeds that cannot be collected.
			name: "succeeded with an all-zero preimage is refused",
			in: &lnrpc.Payment{
				Status:          lnrpc.Payment_SUCCEEDED,
				PaymentPreimage: strings.Repeat("00", 32),
			},
			wantErr: true,
		},
		{
			name: "succeeded with a non-hex preimage is refused",
			in: &lnrpc.Payment{
				Status:          lnrpc.Payment_SUCCEEDED,
				PaymentPreimage: strings.Repeat("zz", 32),
			},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := TranslateRemotePayment(test.in)
			if test.wantErr {
				if err == nil {
					t.Fatalf("wanted an error, got %+v",
						got)
				}
				if got.Settled {
					t.Error("a refused preimage must not " +
						"come back as settled")
				}

				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.wantOut {
				t.Errorf("got %+v, wanted %+v", got,
					test.wantOut)
			}
		})
	}
}

// Neither side may conclude "failed" from a state it does not recognise: the
// local side keeps it not-claimable, the remote side keeps it in flight, and
// both leave the incoming claim alone.
func TestNeitherSideConcludesFailureFromAnUnknownState(t *testing.T) {
	t.Parallel()

	local := TranslatePayment(&fakePayment{terminated: false})
	if !local.InFlight {
		t.Error("local: an unterminated payment must stay in flight")
	}

	remote, err := TranslateRemotePayment(&lnrpc.Payment{
		Status: lnrpc.Payment_PaymentStatus(99),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !remote.InFlight {
		t.Error("remote: an unknown status must stay in flight")
	}
}
