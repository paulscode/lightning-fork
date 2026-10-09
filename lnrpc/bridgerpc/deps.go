package bridgerpc

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/lightningnetwork/lnd/aezeed"
	"github.com/lightningnetwork/lnd/zpay32"
)

// ErrPaymentNotSent says a payment never left this node: it was refused
// before the node wrote any record of it, so nothing can be in flight.
var ErrPaymentNotSent = errors.New("the payment never left this node")

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
// both interfaces, and the other half is a SHA256 node reached over gRPC.
type Deps struct {
	// CheckPayable reports whether this node would pay an invoice, with
	// the checks its router makes before it records a payment. Quoting
	// runs it, so an invoice that would be refused at send time is refused
	// while nothing is held.
	CheckPayable func(ctx context.Context, invoice string) error

	// HoldExpiryDelta is this node's invoices.holdexpirydelta: how many
	// blocks before an accepted hold invoice's HTLC expires the node
	// cancels it. The timing margins count only the blocks before that.
	HoldExpiryDelta uint32

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

	// ForgetInvoice deletes a cancelled invoice so its payment hash can
	// carry a new one. It must refuse an invoice that is not cancelled and
	// succeed for one the node has never seen.
	//
	// A node keeps cancelled invoices and refuses a second invoice for the
	// same hash, so without this an invoice whose first attempt failed
	// could never be paid through the bridge again.
	ForgetInvoice func(ctx context.Context, hash [32]byte) error

	// NodeKey is this node's identity key, compressed and hex-encoded. An
	// invoice payable to it is refused, and a payer checks every hold
	// invoice against it.
	NodeKey string

	// Network is this node's network as lnd's GetInfo names it (mainnet,
	// testnet, regtest, ...). The SHA256 node must be on the same one.
	Network string

	// Dial reaches the world as this node does: through Tor when the node
	// is set to. The rate feed reads the market with it, so a node that
	// keeps its address to itself does not hand it to an exchange. Nil
	// dials directly.
	Dial func(network, address string, timeout time.Duration) (net.Conn,
		error)

	// DeriveSha256Seed is the supervised SHA256 node's aezeed entropy,
	// derived from this node's wallet as seed.go specifies.
	//
	// The entropy and not the key it comes from: the key is this wallet's,
	// and nothing in the bridge needs it beyond this one use.
	DeriveSha256Seed func() ([aezeed.EntropySize]byte, error)

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

	// BlockAt is the header of a block by height, for seeding the chain
	// observer at startup.
	//
	// Without it the observer can only learn from tips as they arrive, one
	// per poll, and it needs a hundred of them in the current difficulty
	// epoch before it will estimate. On a chain with ten minute blocks
	// that is most of a day after every restart, during which the bridge
	// refuses every swap. Reading the headers that already exist turns
	// that into a few seconds.
	BlockAt func(ctx context.Context, height int32) (BlockInfo, error)

	// ChannelBalance is what this node can still send over its channels.
	//
	// It is the side the bridge's own money leaves from, so it is what the
	// inventory policy prices the spread against: a node with little
	// outbound left should be charging more to part with what remains.
	ChannelBalance func(ctx context.Context) (uint64, error)

	// Blake2bActivation is this chain's first BLAKE2b block: its height
	// and id. The SHA256 node must not have that block, which is how a
	// stock lnd pointed at a node that follows BLAKE2b is caught. A height
	// of zero means this network has no activation block to compare.
	//
	// Strict is whether the network holds real money; there a SHA256 node
	// that cannot be checked is not used.
	Blake2bActivation func(ctx context.Context) (height int32,
		hash [32]byte, strict bool, err error)
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
