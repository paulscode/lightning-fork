package bridgerpc

import (
	"context"
	"time"

	"github.com/lightningnetwork/lnd/zpay32"
)

// Deps is what the bridge sub-server needs from the node it runs inside. It is
// defined for every build so the root server can hand it over whether or not
// the sub-server is compiled in.
//
// Everything here is in lnd's own types. That is deliberate: this file is in
// every build, and naming the bridge's types would make every build depend on
// the bridge module, including the builds of people who will never run one.
// The translation to those types happens behind the build tag.
//
// These are the local node's half of the swap. The bridge needs both halves of
// both interfaces, and the other half is a Bitcoin node reached over gRPC.
type Deps struct {
	// AddHoldInvoice creates an invoice on this node that will not settle
	// until told to, and returns the payment request.
	AddHoldInvoice func(ctx context.Context, req HoldInvoiceRequest) (string,
		error)

	// LookupInvoice reports what a hold invoice is doing.
	//
	// It returns found=false when the node has never seen the hash, which
	// the caller must be able to tell apart from an error: that is the
	// difference between "nobody has paid this" and "I cannot say", and
	// only the first is safe to act on.
	LookupInvoice func(ctx context.Context, hash [32]byte) (status InvoiceStatus,
		found bool, err error)

	// SettleInvoice claims a held HTLC with its preimage.
	SettleInvoice func(ctx context.Context, preimage [32]byte) error

	// CancelInvoice returns a held HTLC to its payer.
	CancelInvoice func(ctx context.Context, hash [32]byte) error

	// DecodeInvoice reads a payment request with this node's own decoder
	// and network parameters.
	//
	// Using the paying node's decoder rather than a second one in this
	// process is a requirement rather than a convenience: a parser that
	// disagreed by one field would let the bridge build a hold invoice
	// around one payment hash while the node paid another, and the swap's
	// whole security is that those are the same value.
	DecodeInvoice func(ctx context.Context,
		invoice string) (*zpay32.Invoice, error)

	// PayInvoice sends a payment and blocks until it resolves or the
	// request's timeout elapses.
	//
	// A timeout is not a failure. An HTLC that has left is out there
	// whatever this call reports, so running out of time returns an
	// in-flight status and the caller must look the payment up to learn
	// its fate. Returning an error here would let the bridge conclude
	// failure from having stopped listening.
	PayInvoice func(ctx context.Context, req PayRequest) (PaymentStatus,
		error)

	// LookupPayment reports what became of a payment made earlier, without
	// waiting for it.
	LookupPayment func(ctx context.Context, hash [32]byte) (PaymentStatus,
		error)

	// BestBlock is the tip this node sees.
	//
	// The sync flag is returned rather than acted on here so that the
	// refusal lives with the rest of the bridge's policy, where it is
	// tested. Every margin decision is a comparison against this height,
	// and a stale one says an HTLC has more time left than it does.
	BestBlock func(ctx context.Context) (BlockInfo, error)

	// ChannelBalance is what this node can still send over its channels.
	//
	// It is the side the bridge's own money leaves from, so it is what the
	// inventory policy prices the spread against: a node with little
	// outbound left should be charging more to part with what remains.
	ChannelBalance func(ctx context.Context) (uint64, error)
}

// HoldInvoiceRequest is what the bridge asks the local node to create.
type HoldInvoiceRequest struct {
	// Hash is the payment hash, which comes from the invoice on the other
	// chain that this swap will pay.
	Hash [32]byte

	// AmountMsat is what the invoice asks for.
	AmountMsat uint64

	// CLTVDelta is the minimum CLTV the payer must leave on the final hop,
	// in blocks of this chain. It is sized by the bridge's margin policy in
	// wall-clock and converted, so it is usually far larger than a
	// same-chain invoice would ask for.
	CLTVDelta uint32

	// Expiry is how long the invoice may go unpaid.
	Expiry time.Duration

	// Memo is what the payer sees.
	Memo string
}

// PaymentStatus is what became of a payment.
//
// It carries Known separately from the rest because "this node has never heard
// of that hash" is a real and useful answer after a restart, and one the bridge
// refuses to guess at: it is how a swap that was never paid out is told apart
// from one that was paid and then lost track of.
type PaymentStatus struct {
	// Known is whether the node has any record of this payment.
	Known bool

	// InFlight is whether HTLCs are still outstanding. Nothing may be
	// concluded from a payment in this state.
	InFlight bool

	// Settled is whether the payment succeeded. When true, Preimage is the
	// proceeds of the swap.
	Settled bool

	// Preimage is set only when Settled.
	Preimage [32]byte

	// FeeMsat is what routing cost, for accounting.
	FeeMsat uint64
}

// PayRequest is what the bridge asks the local node to send.
type PayRequest struct {
	// Invoice is the destination invoice, exactly as the payer supplied
	// it. The bridge never rewrites it: its payment hash is what the
	// incoming hold invoice was built on, and settling requires this exact
	// preimage.
	Invoice string

	// MaxFeeMsat caps routing fees.
	MaxFeeMsat uint64

	// CLTVLimit caps the total CLTV of the route, in blocks of this chain.
	// Keeping it small is the cheapest way to shrink what the incoming leg
	// has to outlive.
	CLTVLimit uint32

	// Timeout abandons the attempt. It bounds how long the payment may sit
	// in flight, not how long an HTLC that has already left takes to
	// resolve on chain, which is what CLTVLimit bounds.
	Timeout time.Duration
}

// BlockInfo is a chain tip as this node sees it.
type BlockInfo struct {
	// Height is the tip's height.
	Height int32

	// Time is when the tip was mined, which is the quantity block spacing
	// is measured from. When this node heard about it is a different
	// number and not the one wanted: a node catching up sees a hundred
	// blocks in a minute, and using local arrival times would read that as
	// a chain running a hundred times too fast.
	Time time.Time

	// SyncedToChain is whether this node has caught up.
	SyncedToChain bool
}
