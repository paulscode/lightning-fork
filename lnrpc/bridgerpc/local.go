//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/paulscode/lightning-fork-bridge/node"
)

// ErrNotSynced is returned when this node has not caught up with its chain.
//
// A node behind the tip reports a height that is not the present one, and every
// margin decision is a comparison against that height. Reading it while syncing
// would overstate how long an incoming HTLC has left, in the one direction that
// loses money.
var ErrNotSynced = errors.New("node is not synced to its chain")

// ErrNoDeps is returned when the sub-server was built without the node handing
// over what it needs. It is a programming error rather than an operational one,
// but it must not be a nil dereference while an HTLC is held.
var ErrNoDeps = errors.New("the bridge was not given access to this node")

// Local is this node, as the two halves of a swap see it.
//
// The same value satisfies both interfaces because both directions need both:
// a swap to Bitcoin receives here and pays there, and a swap the other way
// receives there and pays here.
type Local struct {
	deps *Deps
}

// NewLocal wraps the node's dependencies.
func NewLocal(deps *Deps) *Local {
	return &Local{deps: deps}
}

var (
	_ node.Incoming = (*Local)(nil)
	_ node.Outgoing = (*Local)(nil)
)

// ready reports whether every function this adapter calls is present.
//
// Deps is filled by one constructor, so a gap is a programming error. It is
// checked anyway because the alternative is a nil call in the middle of a swap,
// and a panic there takes the node down while it is holding someone's money.
func (l *Local) ready() error {
	switch {
	case l == nil || l.deps == nil:
		return ErrNoDeps

	case l.deps.AddHoldInvoice == nil, l.deps.LookupInvoice == nil,
		l.deps.SettleInvoice == nil, l.deps.CancelInvoice == nil,
		l.deps.DecodeInvoice == nil, l.deps.PayInvoice == nil,
		l.deps.LookupPayment == nil, l.deps.BlockHeight == nil:

		return ErrNoDeps
	}

	return nil
}

// AddHoldInvoice creates an invoice that will not settle until told to.
func (l *Local) AddHoldInvoice(ctx context.Context, req node.HoldInvoice) (
	string, error) {

	if err := l.ready(); err != nil {
		return "", err
	}

	// The node checks these again at the boundary. They are here as well
	// because this is where the bridge's own contract lives, and because
	// each one is a way of losing money rather than a validation nicety: an
	// invoice for any amount lets the payer choose what the bridge is paid,
	// a node-default CLTV will not outlive the outgoing leg, and an invoice
	// with no expiry can be funded hours later at a rate that has moved.
	if req.AmountMsat == 0 {
		return "", errors.New("a hold invoice for any amount would " +
			"let the payer choose what the bridge is paid")
	}
	if req.CLTVDelta == 0 {
		return "", errors.New("no CLTV delta; the margin policy sizes " +
			"this and a node default would not survive the " +
			"outgoing leg")
	}
	if req.Expiry <= 0 {
		return "", errors.New("no invoice expiry; an invoice that " +
			"outlives its quote can be funded at a rate that moved")
	}

	return l.deps.AddHoldInvoice(ctx, HoldInvoiceRequest{
		Hash:       req.Hash,
		AmountMsat: req.AmountMsat,
		CLTVDelta:  req.CLTVDelta,
		Expiry:     req.Expiry,
		Memo:       req.Memo,
	})
}

// LookupInvoice reports what a hold invoice is doing.
func (l *Local) LookupInvoice(ctx context.Context, hash node.Hash) (
	node.Invoice, error) {

	if err := l.ready(); err != nil {
		return node.Invoice{}, err
	}

	status, found, err := l.deps.LookupInvoice(ctx, hash)
	if err != nil {
		return node.Invoice{}, fmt.Errorf("looking up %x: %w", hash,
			err)
	}
	if !found {
		return node.Invoice{}, fmt.Errorf("%w: %x",
			node.ErrUnknownHash, hash)
	}

	return node.Invoice{
		State:        invoiceState(status.State),
		AmountMsat:   status.AmountMsat,
		ExpiryHeight: status.ExpiryHeight,
	}, nil
}

// invoiceState maps this package's enum onto the bridge's.
//
// The two are deliberately separate types with the same shape. A single shared
// one would make the bridge module's numbering part of lnd's ABI, and an
// implicit conversion would silently follow it if either ever changed.
func invoiceState(s InvoiceState) node.InvoiceState {
	switch s {
	case InvoiceOpen:
		return node.InvoiceOpen

	case InvoiceAccepted:
		return node.InvoiceAccepted

	case InvoiceSettled:
		return node.InvoiceSettled

	case InvoiceCancelled:
		return node.InvoiceCancelled

	default:
		// Unreachable from TranslateInvoice, which already folds
		// anything unrecognised into cancelled. Kept so that adding a
		// state here without deciding what it means cannot make it
		// claimable by accident.
		return node.InvoiceCancelled
	}
}

// SettleInvoice claims the HTLC. It is idempotent.
func (l *Local) SettleInvoice(ctx context.Context, pre node.Preimage) error {
	if err := l.ready(); err != nil {
		return err
	}

	err := l.deps.SettleInvoice(ctx, pre)
	if err == nil {
		return nil
	}

	// Settling one that is already settled is success, not failure: after a
	// crash the bridge reissues this, and the reissue arriving at a node
	// that already did it must not read as a problem.
	//
	// Asking the node what state the invoice is in, rather than matching
	// the error, because the registry's error values are not this
	// adapter's interface and a rename would turn a settled swap into one
	// that retries forever.
	if inv, err2 := l.LookupInvoice(ctx, pre.Hash()); err2 == nil &&
		inv.State == node.InvoiceSettled {

		return nil
	}

	return fmt.Errorf("settling: %w", err)
}

// CancelInvoice returns the HTLC. It is idempotent.
func (l *Local) CancelInvoice(ctx context.Context, hash node.Hash) error {
	if err := l.ready(); err != nil {
		return err
	}

	err := l.deps.CancelInvoice(ctx, hash)
	if err == nil {
		return nil
	}

	if inv, err2 := l.LookupInvoice(ctx, hash); err2 == nil &&
		inv.State == node.InvoiceCancelled {

		return nil
	}

	return fmt.Errorf("cancelling %x: %w", hash, err)
}

// BlockHeight is the tip this node sees.
func (l *Local) BlockHeight(ctx context.Context) (int32, error) {
	if err := l.ready(); err != nil {
		return 0, err
	}

	height, synced, err := l.deps.BlockHeight(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading this node's height: %w", err)
	}
	if !synced {
		// Refuse rather than return a height that may not be the
		// present one.
		return 0, fmt.Errorf("%w: at height %d", ErrNotSynced, height)
	}

	return height, nil
}

// Decode reads a payment request with this node's own decoder.
func (l *Local) Decode(ctx context.Context, invoice string) (node.Decoded,
	error) {

	if err := l.ready(); err != nil {
		return node.Decoded{}, err
	}

	pay, err := l.deps.DecodeInvoice(ctx, invoice)
	if err != nil {
		return node.Decoded{}, fmt.Errorf("decoding: %w", err)
	}
	if pay == nil {
		return node.Decoded{}, errors.New("decoding: the node " +
			"returned nothing")
	}

	out := node.Decoded{
		Hash:        node.Hash(*pay.PaymentHash),
		Expiry:      pay.Timestamp.Add(pay.Expiry()),
		Description: description(pay.Description),
	}

	// A zero-amount invoice is refused rather than defaulted. The outgoing
	// leg would be for whatever the bridge chose and the incoming one
	// priced against a guess.
	if pay.MilliSat == nil || *pay.MilliSat == 0 {
		return node.Decoded{}, errors.New("the invoice names no " +
			"amount, so there is nothing to price the swap against")
	}
	out.AmountMsat = uint64(*pay.MilliSat)

	delta := pay.MinFinalCLTVExpiry()
	if delta > math.MaxUint32 {
		return node.Decoded{}, fmt.Errorf("the invoice asks for a "+
			"final CLTV delta of %d, which does not fit", delta)
	}
	out.CLTVDelta = uint32(delta)

	if pay.Destination != nil {
		out.Destination = fmt.Sprintf("%x",
			pay.Destination.SerializeCompressed())
	}

	return out, nil
}

// description reads an optional description without dereferencing nothing.
func description(d *string) string {
	if d == nil {
		return ""
	}

	return *d
}

// Pay sends a payment and waits for it to resolve.
//
// It returns an in-flight payment, not an error, when the attempt runs out of
// time. That distinction is the point: an HTLC that has left is out there
// whatever this call reports, and the only safe reading of "I stopped waiting"
// is "I do not know yet".
func (l *Local) Pay(ctx context.Context, req node.PayRequest) (node.Payment,
	error) {

	if err := l.ready(); err != nil {
		return node.Payment{}, err
	}

	status, err := l.deps.PayInvoice(ctx, PayRequest{
		Invoice:    req.Invoice,
		MaxFeeMsat: req.MaxFeeMsat,
		CLTVLimit:  req.CLTVLimit,
		Timeout:    req.Timeout,
	})
	if err != nil {
		return node.Payment{}, fmt.Errorf("sending: %w", err)
	}

	return payment(status), nil
}

// LookupPayment reports what a payment is doing, without waiting for it.
func (l *Local) LookupPayment(ctx context.Context, hash node.Hash) (
	node.Payment, error) {

	if err := l.ready(); err != nil {
		return node.Payment{}, err
	}

	status, err := l.deps.LookupPayment(ctx, hash)
	if err != nil {
		return node.Payment{}, fmt.Errorf("looking up payment %x: %w",
			hash, err)
	}

	return payment(status), nil
}

// payment maps a status onto the bridge's four states.
//
// The order of the tests is the whole of it. Settled is checked first because
// the preimage is the proceeds of the swap; in flight before failed because an
// outstanding HTLC authorises no conclusion; and failed only when the node has
// a record that is neither.
func payment(s PaymentStatus) node.Payment {
	switch {
	case !s.Known:
		return node.Payment{State: node.PaymentNone}

	case s.Settled:
		return node.Payment{
			State:    node.PaymentSucceeded,
			Preimage: node.Preimage(s.Preimage),
			FeeMsat:  s.FeeMsat,
		}

	case s.InFlight:
		return node.Payment{
			State: node.PaymentInFlight, FeeMsat: s.FeeMsat,
		}

	default:
		return node.Payment{
			State: node.PaymentFailed, FeeMsat: s.FeeMsat,
		}
	}
}

// Check confirms this node can answer and has caught up with its chain.
//
// Worth calling before the bridge opens: the first sign of a node still
// catching up should not be a swap that has already accepted someone's money.
func (l *Local) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := l.BlockHeight(ctx); err != nil {
		return fmt.Errorf("local node: %w", err)
	}

	return nil
}
