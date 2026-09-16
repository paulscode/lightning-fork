package bridgerpc

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// ErrNodeContradiction is returned when a node says two things that cannot
// both be true. It is deliberately not recoverable by retrying: the bridge
// cannot act on an answer it does not believe, and an operator should look.
var ErrNodeContradiction = errors.New("the node contradicted itself")

// The Bitcoin side of a swap is a stock lnd reached over gRPC, so its answers
// arrive as lnrpc messages rather than as registry structures. The rules are
// the same as for the local side in translate.go, and are restated here rather
// than shared because the two inputs have nothing in common but their meaning.
//
// Keeping both in this file, in every build, is what lets them be tested
// together against the same table of cases: the two sides of a swap must agree
// about what "claimable" and "failed" mean, and a divergence between them is
// the kind of bug that only shows up with money on the line.

// TranslateRemoteInvoice reads what a stock lnd says about a hold invoice.
func TranslateRemoteInvoice(inv *lnrpc.Invoice) InvoiceStatus {
	if inv == nil {
		return InvoiceStatus{State: InvoiceCancelled}
	}

	out := InvoiceStatus{AmountMsat: uint64(inv.GetValueMsat())}

	switch inv.GetState() {
	case lnrpc.Invoice_OPEN:
		out.State = InvoiceOpen

	case lnrpc.Invoice_ACCEPTED:
		out.State = InvoiceAccepted

	case lnrpc.Invoice_SETTLED:
		out.State = InvoiceSettled

	case lnrpc.Invoice_CANCELED:
		out.State = InvoiceCancelled

	default:
		// Same rule as the local side: an unrecognised state is not
		// claimable. A node newer than this build must not be able to
		// talk it into paying out.
		out.State = InvoiceCancelled
	}

	out.ExpiryHeight = earliestRemoteAcceptedExpiry(inv)

	return out
}

// earliestRemoteAcceptedExpiry is the minimum expiry height across the locked
// in HTLCs, or zero when none are.
//
// See earliestAcceptedExpiry in translate.go for why it is the minimum and not
// the invoice's nominal delta: the claim on the whole invoice dies with the
// earliest of its parts, and reporting anything later says there is more time
// than there is.
func earliestRemoteAcceptedExpiry(inv *lnrpc.Invoice) int32 {
	var earliest int32
	for _, htlc := range inv.GetHtlcs() {
		if htlc == nil {
			continue
		}
		if htlc.GetState() != lnrpc.InvoiceHTLCState_ACCEPTED {
			continue
		}

		// Already an int32 on the wire, so a negative is possible here
		// in a way it is not locally. Either way it carries no
		// information, and letting one through would report "no time
		// left" for an HTLC that has plenty.
		h := htlc.GetExpiryHeight()
		if h <= 0 {
			continue
		}
		if earliest == 0 || h < earliest {
			earliest = h
		}
	}

	return earliest
}

// TranslateRemotePayment reads what a stock lnd says about a payment.
//
// A nil payment means the node has no record of it, which the caller gets from
// a NotFound and must pass through rather than turn into an error: "never
// started" is how the bridge tells a swap it never paid out from one it paid
// and lost track of.
func TranslateRemotePayment(p *lnrpc.Payment) (PaymentStatus, error) {
	if p == nil {
		return PaymentStatus{Known: false}, nil
	}

	out := PaymentStatus{Known: true, FeeMsat: uint64(p.GetFeeMsat())}

	switch p.GetStatus() {
	case lnrpc.Payment_SUCCEEDED:
		pre, err := preimageFromHex(p.GetPaymentPreimage())
		if err != nil {
			// A succeeded payment with no usable preimage is a
			// contradiction, and the preimage is the entire
			// proceeds of the swap: it is what claims the incoming
			// HTLC. Reporting success without it would move the
			// swap to a state whose only action cannot be carried
			// out.
			return PaymentStatus{}, fmt.Errorf("%w: succeeded "+
				"but %w", ErrNodeContradiction, err)
		}
		out.Settled = true
		out.Preimage = pre

	case lnrpc.Payment_FAILED:
		// Terminal, and the only status that authorises cancelling the
		// incoming claim.

	case lnrpc.Payment_IN_FLIGHT, lnrpc.Payment_INITIATED:
		out.InFlight = true

	default:
		// An unrecognised status is not a failure. Treating one as
		// terminal would cancel a claim against an HTLC that may still
		// be in flight, so it is read the safe way round: unknown
		// means still outstanding, and the bridge keeps waiting.
		out.InFlight = true
	}

	return out, nil
}

// preimageFromHex reads a preimage the node reported as hex.
//
// An all-zero preimage is refused rather than accepted: it is what lnd reports
// for a payment with no preimage, it is not a value that settles anything, and
// passing it on would look like proceeds.
func preimageFromHex(s string) ([32]byte, error) {
	var out [32]byte

	if s == "" {
		return out, errors.New("the preimage is empty")
	}

	raw, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("the preimage %q is not hex: %w", s, err)
	}
	if len(raw) != len(out) {
		return out, fmt.Errorf("the preimage is %d bytes, wanted %d",
			len(raw), len(out))
	}
	copy(out[:], raw)

	if out == ([32]byte{}) {
		return [32]byte{}, errors.New("the preimage is all zeroes")
	}

	return out, nil
}
