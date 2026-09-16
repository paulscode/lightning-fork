package bridgerpc

import (
	"reflect"

	"github.com/lightningnetwork/lnd/invoices"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
)

// InvoiceState is what a hold invoice on this node is doing.
//
// It is a small enum of this package's own rather than lnd's ContractState
// because the bridge distinguishes exactly four cases and must fail closed on
// anything else. Passing lnd's type through would make "a state this build
// does not recognise" indistinguishable from a state it does.
type InvoiceState uint8

const (
	// InvoiceOpen: created and waiting to be paid.
	InvoiceOpen InvoiceState = iota

	// InvoiceAccepted: an HTLC is locked in and claimable with the
	// preimage. This is the state a hold invoice sits in while the bridge
	// works, and the only state it is safe to pay out against.
	InvoiceAccepted

	// InvoiceSettled: claimed.
	InvoiceSettled

	// InvoiceCancelled: returned to the payer, or expired.
	InvoiceCancelled
)

// InvoiceStatus is what this node says about a hold invoice, in the terms the
// bridge needs.
type InvoiceStatus struct {
	// State is what the invoice is doing.
	State InvoiceState

	// AmountMsat is what the invoice asks for.
	AmountMsat uint64

	// ExpiryHeight is the chain height at which the incoming HTLC stops
	// being claimable, or zero while nothing is locked in.
	ExpiryHeight int32
}

// TranslateInvoice is the part of this package worth reading twice.
//
// Two things here decide whether the bridge pays out against a claim it can
// still collect, and both fail closed.
func TranslateInvoice(inv *invoices.Invoice) InvoiceStatus {
	if inv == nil {
		return InvoiceStatus{State: InvoiceCancelled}
	}

	out := InvoiceStatus{AmountMsat: uint64(inv.Terms.Value)}

	switch inv.State {
	case invoices.ContractOpen:
		out.State = InvoiceOpen

	case invoices.ContractAccepted:
		out.State = InvoiceAccepted

	case invoices.ContractSettled:
		out.State = InvoiceSettled

	case invoices.ContractCanceled:
		out.State = InvoiceCancelled

	default:
		// An unrecognised state is not claimable as far as the bridge
		// is concerned. Treating an unknown as accepted would
		// authorise a payment against something that may not be there.
		out.State = InvoiceCancelled
	}

	out.ExpiryHeight = earliestAcceptedExpiry(inv)

	return out
}

// earliestAcceptedExpiry is the minimum expiry height across the HTLCs that
// are locked in, or zero when none are.
//
// It is the minimum, not the invoice's nominal delta and not the maximum. A
// multi-part payment arrives as several HTLCs that took different routes, so
// they expire at different heights, and the claim on the whole invoice dies
// with the earliest of them: once that one times out, its payer's channel
// force-closes and takes that share back, and settling afterwards claims less
// than the swap was priced at. Reporting the nominal delta, or the largest,
// would tell the margin check there is more time than there is, which is
// exactly the direction that pays out against a claim the bridge can no longer
// collect.
//
// Settled and cancelled HTLCs are skipped: a cancelled one is gone and a
// settled one is already collected, so neither bounds anything.
func earliestAcceptedExpiry(inv *invoices.Invoice) int32 {
	var earliest int32
	for _, htlc := range inv.Htlcs {
		if htlc == nil || htlc.State != invoices.HtlcStateAccepted {
			continue
		}

		// A zero or negative height carries no information, and
		// letting one through would report "no time left" for an HTLC
		// that has plenty, cancelling a swap that was fine.
		if htlc.Expiry == 0 || htlc.Expiry > 1<<31-1 {
			continue
		}
		h := int32(htlc.Expiry)
		if earliest == 0 || h < earliest {
			earliest = h
		}
	}

	return earliest
}

// TranslatePayment reports what became of a payment, keeping "never seen"
// distinct from "failed".
//
// Those two are the same shape to most callers and must not be here. After a
// restart the bridge asks this about every unfinished swap, and the answer
// decides whether it may cancel the incoming claim. Reading "I have no record"
// as "it failed" would cancel a claim against an HTLC that is still in flight,
// and reading "still trying" as "failed" would do the same.
//
// A nil payment means the node has no record, which is what the caller must
// pass for a not-initiated lookup.
func TranslatePayment(payment paymentsdb.DBMPPayment) PaymentStatus {
	// Both a nil interface and a typed nil inside one mean the same thing
	// here, and only the first is caught by a plain comparison. A caller
	// that passed a typed nil and got "known" back would have the bridge
	// conclude a payment exists from a value that says nothing.
	if isNil(payment) {
		return PaymentStatus{Known: false}
	}

	status := PaymentStatus{Known: true}

	settled, _ := payment.TerminalInfo()
	if settled != nil && settled.Settle != nil {
		status.Settled = true
		status.Preimage = settled.Settle.Preimage
		status.FeeMsat = uint64(settled.Route.TotalFees())

		return status
	}

	// Not settled. Anything short of terminated is still outstanding, and
	// an outstanding payment authorises no conclusion at all. Only a
	// terminated, unsettled payment is a failure.
	status.InFlight = !payment.Terminated()

	return status
}

// isNil reports whether an interface value holds nothing, including the typed
// nil case that a plain comparison misses.
func isNil(v any) bool {
	if v == nil {
		return true
	}

	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice,
		reflect.Chan, reflect.Func:

		return rv.IsNil()

	default:
		return false
	}
}
