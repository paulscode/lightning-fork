//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// ErrInvoice is returned when a payment request cannot be used.
var ErrInvoice = errors.New("the invoice cannot be used")

// Remote is the Bitcoin Lightning node, reached over gRPC.
//
// It uses lnd's own generated clients, which are already linked into this
// binary. The bridge module ships its own copy and importing that one panics
// the node at startup, which deps_guard_test.go exists to explain.
//
// As with Local, one value satisfies both interfaces because both directions
// need both halves.
type Remote struct {
	main     lnrpc.LightningClient
	invoices invoicesrpc.InvoicesClient
	router   routerrpc.RouterClient
	chain    chainrpc.ChainKitClient
}

// NewRemote wraps a connection to the Bitcoin node.
func NewRemote(conn grpc.ClientConnInterface) *Remote {
	return &Remote{
		main:     lnrpc.NewLightningClient(conn),
		invoices: invoicesrpc.NewInvoicesClient(conn),
		router:   routerrpc.NewRouterClient(conn),
		chain:    chainrpc.NewChainKitClient(conn),
	}
}

var (
	_ node.Incoming = (*Remote)(nil)
	_ node.Outgoing = (*Remote)(nil)
)

// AddHoldInvoice creates an invoice that will not settle until told to.
//
// It sends no route hints. That is correct while the bridge is its own users'
// LSP, which is the topology this is built for, and wrong the moment it is not:
// a payer with no public path to that node needs a hint to find one. Adding
// them is a change here plus a source of channel information, not a change
// anywhere above.
func (r *Remote) AddHoldInvoice(ctx context.Context, req node.HoldInvoice) (
	string, error) {

	// The same three refusals as the local side, for the same reasons: the
	// payer would otherwise choose what the bridge is paid, a node default
	// CLTV would not outlive the outgoing leg, and an invoice with no
	// expiry can be funded hours later at a rate that has moved.
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
	if req.AmountMsat > math.MaxInt64 {
		return "", fmt.Errorf("amount %d does not fit", req.AmountMsat)
	}

	resp, err := r.invoices.AddHoldInvoice(
		ctx, &invoicesrpc.AddHoldInvoiceRequest{
			Memo:       req.Memo,
			Hash:       req.Hash[:],
			ValueMsat:  int64(req.AmountMsat),
			CltvExpiry: uint64(req.CLTVDelta),
			Expiry:     int64(req.Expiry.Seconds()),
		},
	)
	if err != nil {
		return "", fmt.Errorf("adding a hold invoice: %w", err)
	}

	return resp.GetPaymentRequest(), nil
}

// LookupInvoice reports what a hold invoice is doing.
func (r *Remote) LookupInvoice(ctx context.Context, hash node.Hash) (
	node.Invoice, error) {

	inv, err := r.invoices.LookupInvoiceV2(
		ctx, &invoicesrpc.LookupInvoiceMsg{
			InvoiceRef: &invoicesrpc.LookupInvoiceMsg_PaymentHash{
				PaymentHash: hash[:],
			},
		},
	)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return node.Invoice{}, fmt.Errorf("%w: %x",
				node.ErrUnknownHash, hash)
		}

		return node.Invoice{}, fmt.Errorf("looking up %x: %w", hash,
			err)
	}

	st := TranslateRemoteInvoice(inv)

	return node.Invoice{
		State:        invoiceState(st.State),
		AmountMsat:   st.AmountMsat,
		ExpiryHeight: st.ExpiryHeight,
	}, nil
}

// SettleInvoice claims the HTLC. It is idempotent.
func (r *Remote) SettleInvoice(ctx context.Context, pre node.Preimage) error {
	_, err := r.invoices.SettleInvoice(
		ctx, &invoicesrpc.SettleInvoiceMsg{Preimage: pre[:]},
	)
	if err == nil {
		return nil
	}

	// Settling one that is already settled is success, not failure: after a
	// crash the bridge reissues this, and the reissue arriving at a node
	// that already did it must not read as a problem.
	//
	// Asking the node what state the invoice is in, rather than matching
	// the error's text, because the text is not an interface and a node
	// that rephrases it would turn a settled swap into one that retries
	// forever.
	if inv, err2 := r.LookupInvoice(ctx, pre.Hash()); err2 == nil &&
		inv.State == node.InvoiceSettled {

		return nil
	}

	return fmt.Errorf("settling: %w", err)
}

// CancelInvoice returns the HTLC. It is idempotent.
func (r *Remote) CancelInvoice(ctx context.Context, hash node.Hash) error {
	_, err := r.invoices.CancelInvoice(
		ctx, &invoicesrpc.CancelInvoiceMsg{PaymentHash: hash[:]},
	)
	if err == nil {
		return nil
	}

	if inv, err2 := r.LookupInvoice(ctx, hash); err2 == nil &&
		inv.State == node.InvoiceCancelled {

		return nil
	}

	return fmt.Errorf("cancelling %x: %w", hash, err)
}

// BlockHeight is the tip that node sees.
func (r *Remote) BlockHeight(ctx context.Context) (int32, error) {
	info, err := r.main.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return 0, fmt.Errorf("reading the node's height: %w", err)
	}
	if !info.GetSyncedToChain() {
		// Refuse rather than return a height that is not the present
		// one. Every margin decision is a comparison against this
		// number, and a stale one says an HTLC has more time left than
		// it does.
		return 0, fmt.Errorf("%w: at height %d", ErrNotSynced,
			info.GetBlockHeight())
	}

	h := info.GetBlockHeight()
	if h > math.MaxInt32 {
		return 0, fmt.Errorf("%w: implausible height %d", ErrNotSynced,
			h)
	}

	return int32(h), nil
}

// Decode reads a payment request with that node's own decoder.
//
// Deliberately the paying node's decoder rather than a second one in this
// process. A separate parser that disagreed by one field would let the bridge
// build a hold invoice around one payment hash while the node paid another, and
// the swap's entire security is that those are the same value.
func (r *Remote) Decode(ctx context.Context, invoice string) (node.Decoded,
	error) {

	if invoice == "" {
		return node.Decoded{}, fmt.Errorf("%w: empty", ErrInvoice)
	}

	req, err := r.main.DecodePayReq(
		ctx, &lnrpc.PayReqString{PayReq: invoice},
	)
	if err != nil {
		return node.Decoded{}, fmt.Errorf("%w: %w", ErrInvoice, err)
	}

	var out node.Decoded
	h, err := hex.DecodeString(req.GetPaymentHash())
	if err != nil || len(h) != len(out.Hash) {
		return node.Decoded{}, fmt.Errorf("%w: payment hash %q",
			ErrInvoice, req.GetPaymentHash())
	}
	copy(out.Hash[:], h)

	msat := req.GetNumMsat()
	if msat <= 0 {
		// Zero is refused along with negative: the outgoing leg would
		// be for whatever the bridge chose and the incoming one priced
		// against a guess.
		return node.Decoded{}, fmt.Errorf("%w: the invoice names no "+
			"amount (%d msat), so there is nothing to price the "+
			"swap against", ErrInvoice, msat)
	}
	out.AmountMsat = uint64(msat)

	cltv := req.GetCltvExpiry()
	if cltv < 0 || cltv > math.MaxInt32 {
		return node.Decoded{}, fmt.Errorf("%w: CLTV delta %d",
			ErrInvoice, cltv)
	}
	out.CLTVDelta = uint32(cltv)

	// Timestamp plus expiry, because an invoice's expiry is relative to
	// when it was created and the bridge cares about the wall-clock moment.
	if ts := req.GetTimestamp(); ts > 0 {
		out.Expiry = time.Unix(ts, 0).Add(
			time.Duration(req.GetExpiry()) * time.Second,
		)
	}

	out.Destination = req.GetDestination()
	out.Description = req.GetDescription()

	return out, nil
}

// Pay sends a payment and waits for it to resolve.
//
// It returns an in-flight payment, not an error, when the attempt runs out of
// time. That distinction is the point: an HTLC that has left is out there
// whatever this call reports, and the only safe reading of "I stopped waiting"
// is "I do not know yet".
func (r *Remote) Pay(ctx context.Context, req node.PayRequest) (node.Payment,
	error) {

	if req.Invoice == "" {
		return node.Payment{}, fmt.Errorf("%w: no invoice to pay",
			ErrInvoice)
	}
	if req.CLTVLimit == 0 {
		return node.Payment{}, errors.New("no CLTV limit; the " +
			"incoming leg is sized against this and an unbounded " +
			"route could outlive it")
	}
	if req.CLTVLimit > math.MaxInt32 {
		return node.Payment{}, fmt.Errorf("CLTV limit %d does not fit",
			req.CLTVLimit)
	}
	if req.Timeout <= 0 {
		return node.Payment{}, errors.New("no timeout; an attempt " +
			"that never gives up holds the incoming HTLC until it " +
			"expires")
	}
	if req.MaxFeeMsat > math.MaxInt64 {
		return node.Payment{}, fmt.Errorf("fee limit %d does not fit",
			req.MaxFeeMsat)
	}

	// The stream gets its own context, cancelled on the way out, so that
	// returning early does not leave it open for the life of the caller's.
	//
	// Cancelling it does not cancel the payment. An HTLC already in flight
	// resolves on its own schedule whatever this process does, which is why
	// the only way to learn its fate is to look it up, and why nothing here
	// may conclude failure from having stopped listening.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := r.router.SendPaymentV2(
		ctx, &routerrpc.SendPaymentRequest{
			PaymentRequest: req.Invoice,
			TimeoutSeconds: int32(req.Timeout.Seconds()),
			FeeLimitMsat:   int64(req.MaxFeeMsat),
			CltvLimit:      int32(req.CLTVLimit),
		},
	)
	if err != nil {
		return node.Payment{}, fmt.Errorf("sending: %w", err)
	}

	// Anything short of a terminal answer is in flight. Starting from that
	// rather than from the zero value means a stream that dies early cannot
	// be read as "never started".
	last := node.Payment{State: node.PaymentInFlight}
	for {
		update, err := stream.Recv()
		if err != nil {
			// EOF, a broken stream or a passed deadline. The
			// payment is not known to have failed, and saying so
			// would let the caller cancel the incoming claim
			// against an HTLC still in flight.
			if errors.Is(err, io.EOF) {
				return last, nil
			}

			return last, nil
		}

		status, err := TranslateRemotePayment(update)
		if err != nil {
			return node.Payment{}, err
		}
		pmt := payment(status)
		last = pmt

		switch pmt.State {
		case node.PaymentSucceeded, node.PaymentFailed:
			return pmt, nil
		}
	}
}

// LookupPayment reports what a payment is doing, without waiting for it.
func (r *Remote) LookupPayment(ctx context.Context, hash node.Hash) (
	node.Payment, error) {

	// The stream is closed as soon as one update has been read. Without
	// in-flight updates the node would hold it open until the payment
	// resolved, which is a wait, not a lookup, and recovery calls this for
	// every unfinished swap in turn.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// NotFound means the node has never seen this hash, which is a real
	// answer after a restart and not an error: it is how the driver tells
	// "never sent" apart from "sent and lost track of", and the difference
	// is one it refuses to guess at.
	stream, err := r.router.TrackPaymentV2(
		ctx, &routerrpc.TrackPaymentRequest{
			PaymentHash:       hash[:],
			NoInflightUpdates: false,
		},
	)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return node.Payment{State: node.PaymentNone}, nil
		}

		return node.Payment{}, fmt.Errorf("tracking %x: %w", hash, err)
	}

	update, err := stream.Recv()
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return node.Payment{State: node.PaymentNone}, nil
		}

		return node.Payment{}, fmt.Errorf("tracking %x: %w", hash, err)
	}

	st, err := TranslateRemotePayment(update)
	if err != nil {
		return node.Payment{}, err
	}

	return payment(st), nil
}

// Check confirms the node answers and is synced to its chain.
//
// Both conditions, so it reports whether the bridge can quote right now. Use
// Reachable at startup instead.
func (r *Remote) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := r.BlockHeight(ctx); err != nil {
		return fmt.Errorf("bitcoin node: %w", err)
	}

	return nil
}

// Reachable confirms the node answers, without requiring that it has caught up.
//
// This is what startup needs. Dialling succeeds against a node that is not
// there, so a wrong address, a wrong macaroon or a node that is simply down
// has to be found here rather than by a swap that has already accepted
// someone's money. Being merely behind is different: it is temporary, it is
// already refused at quote time, and refusing to start on it would mean a
// Bitcoin node restart takes this one down too.
func (r *Remote) Reachable(ctx context.Context) (BlockInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := r.BestBlock(ctx)
	if err != nil {
		return BlockInfo{}, fmt.Errorf("bitcoin node: %w", err)
	}

	return info, nil
}

// Balance is what that node can still send over its channels.
//
// Local balance rather than capacity: what the far end holds is not something
// the bridge can spend, and counting it would have the inventory policy think
// the side is better funded than it is and quote a spread too thin for what it
// can actually do.
func (r *Remote) Balance(ctx context.Context) (uint64, error) {
	// ListChannels rather than ChannelBalance, because the aggregate
	// includes each channel's reserve and channels whose peer is offline.
	// Neither can carry a payment, and counting them lets the bridge quote
	// a swap it cannot pay: that fails safely, but it wastes an HTLC and
	// the payer's time on a promise it should not have made.
	channels, err := r.main.ListChannels(
		ctx, &lnrpc.ListChannelsRequest{ActiveOnly: true},
	)
	if err != nil {
		return 0, fmt.Errorf("reading the Bitcoin node's channels: %w",
			err)
	}

	var outbound uint64
	for _, c := range channels.GetChannels() {
		if c == nil {
			continue
		}

		spendable := c.GetLocalBalance() -
			int64(c.GetLocalChanReserveSat())
		if spendable <= 0 {
			continue
		}
		if uint64(spendable) > math.MaxInt64/1000 {
			return 0, fmt.Errorf("the Bitcoin node reports an "+
				"implausible balance of %d sat", spendable)
		}

		outbound += uint64(spendable) * 1000
	}

	return outbound, nil
}

// BestBlock is the tip that node sees, with the time it was mined.
//
// The block's own timestamp, not when the node heard about it: block spacing is
// measured from it, and a node catching up sees a hundred blocks in a minute,
// which arrival times would read as a chain running a hundred times too fast.
func (r *Remote) BestBlock(ctx context.Context) (BlockInfo, error) {
	info, err := r.main.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return BlockInfo{}, fmt.Errorf("reading the Bitcoin node's "+
			"tip: %w", err)
	}

	h := info.GetBlockHeight()
	if h > math.MaxInt32 {
		return BlockInfo{}, fmt.Errorf("the Bitcoin node reports an "+
			"implausible height %d", h)
	}

	out := BlockInfo{
		Height:        int32(h),
		SyncedToChain: info.GetSyncedToChain(),
	}
	if ts := info.GetBestHeaderTimestamp(); ts > 0 {
		out.Time = time.Unix(ts, 0)
	}

	return out, nil
}

// BlockAt is a block's header by height, for seeding the chain observer.
//
// It goes through ChainKit, which a stock lnd only serves when built with the
// chainrpc tag. A node without it answers Unimplemented, which the caller
// treats as "no history available" and falls back to learning from tips. That
// is slow rather than wrong, so it is worth trying and not worth requiring.
func (r *Remote) BlockAt(ctx context.Context, height int32) (BlockInfo, error) {
	if height < 0 {
		return BlockInfo{}, fmt.Errorf("height %d is not a block",
			height)
	}

	hash, err := r.chain.GetBlockHash(
		ctx, &chainrpc.GetBlockHashRequest{BlockHeight: int64(height)},
	)
	if err != nil {
		return BlockInfo{}, fmt.Errorf("the Bitcoin node's hash for "+
			"height %d: %w", height, err)
	}

	hdr, err := r.chain.GetBlockHeader(
		ctx, &chainrpc.GetBlockHeaderRequest{
			BlockHash: hash.GetBlockHash(),
		},
	)
	if err != nil {
		return BlockInfo{}, fmt.Errorf("the Bitcoin node's header at "+
			"height %d: %w", height, err)
	}

	raw := hdr.GetRawBlockHeader()
	if len(raw) < 80 {
		return BlockInfo{}, fmt.Errorf("the Bitcoin node returned a "+
			"%d byte header at height %d", len(raw), height)
	}

	// Bytes 68 to 72 of a block header are its timestamp, little endian.
	// Decoding the four bytes rather than the whole header keeps this
	// independent of which chain's header format this build parses, which
	// matters because this binary's own parser is the BLAKE2b one.
	ts := binary.LittleEndian.Uint32(raw[68:72])

	return BlockInfo{
		Height: height, Time: time.Unix(int64(ts), 0),

		// Historical, so by definition already in the chain.
		SyncedToChain: true,
	}, nil
}
