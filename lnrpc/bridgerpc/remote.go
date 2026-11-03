//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// ErrInvoice is returned when a payment request cannot be used.
var ErrInvoice = errors.New("the invoice cannot be used")

// Remote is the SHA256 node, reached over gRPC.
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

	// tip is the last height BestBlock read from the node synced to its
	// chain, and tipAt when; see routeHeight. key is the identity key the
	// last GetInfo gave, and keyAt when; see IdentityKey. now is the clock,
	// for tests.
	tipMu sync.Mutex
	tip   int32
	tipAt time.Time
	key   string
	keyAt time.Time
	now   func() time.Time
}

// NewRemote wraps a connection to the SHA256 node.
func NewRemote(conn grpc.ClientConnInterface) *Remote {
	return &Remote{
		main:     lnrpc.NewLightningClient(conn),
		invoices: invoicesrpc.NewInvoicesClient(conn),
		router:   routerrpc.NewRouterClient(conn),
		chain:    chainrpc.NewChainKitClient(conn),
		now:      time.Now,
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
			Memo:            req.Memo,
			Hash:            req.Hash[:],
			ValueMsat:       int64(req.AmountMsat),
			CltvExpiry:      uint64(req.CLTVDelta),
			Expiry:          int64(req.Expiry.Seconds()),
			PaymentMetadata: req.Metadata,
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
		if invoiceAbsent(err) {
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
		HTLCs:        st.HTLCs,
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

	// A stock lnd lists the bits an invoice sets even when it does not
	// know them, marked unknown, and that is the case here: the SHA256
	// node must still be able to tell an invoice is for the other chain.
	for _, bit := range node.BLAKE2bFeatureBits {
		if _, ok := req.GetFeatures()[bit]; ok {
			out.BLAKE2b = true
		}
	}

	return out, nil
}

// Payable reports whether this node would pay the invoice, by the checks a
// stock lnd makes before it records a payment. Asked when quoting, while
// nothing is held: refused at send time, before the node writes any payment
// record, the swap would be left paying with nothing to look up.
func (r *Remote) Payable(ctx context.Context, invoice string) error {
	req, err := r.main.DecodePayReq(
		ctx, &lnrpc.PayReqString{PayReq: invoice},
	)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvoice, err)
	}

	return remotePayable(req)
}

// remotePayable applies the checks a stock lnd makes before it records a
// payment: a payment address or blinded paths, no AMP, and no required
// feature it does not know.
func remotePayable(req *lnrpc.PayReq) error {
	if len(req.GetPaymentAddr()) == 0 && len(req.GetBlindedPaths()) == 0 {
		return errors.New("the invoice has no payment address, which " +
			"the paying node requires")
	}
	for bit, f := range req.GetFeatures() {
		if bit == uint32(lnwire.AMPRequired) ||
			bit == uint32(lnwire.AMPOptional) {

			return errors.New("the invoice asks for AMP, which the " +
				"bridge does not pay")
		}
		if f.GetIsRequired() && !f.GetIsKnown() {
			return fmt.Errorf("the invoice requires feature %d, "+
				"which the paying node does not know", bit)
		}
	}

	return nil
}

// ForgetInvoice deletes a cancelled invoice on the SHA256 node so its hash can
// carry a new one.
//
// The state is read first rather than left to the node to refuse, so that
// nothing but a cancelled invoice is ever deleted, and a hash the node has
// never seen counts as already forgotten.
func (r *Remote) ForgetInvoice(ctx context.Context, hash node.Hash) error {
	inv, err := r.LookupInvoice(ctx, hash)
	switch {
	case errors.Is(err, node.ErrUnknownHash):
		return nil
	case err != nil:
		return fmt.Errorf("reading %x before forgetting it: %w", hash, err)
	case inv.State != node.InvoiceCancelled:
		return fmt.Errorf("%x is %v, and only a cancelled invoice may be "+
			"forgotten", hash, inv.State)
	}

	_, err = r.main.DeleteCanceledInvoice(ctx, &lnrpc.DelCanceledInvoiceReq{
		InvoiceHash: hex.EncodeToString(hash[:]),
	})
	if err == nil {
		return nil
	}
	if _, lookupErr := r.LookupInvoice(ctx, hash); errors.Is(lookupErr,
		node.ErrUnknownHash) {

		return nil
	}

	return fmt.Errorf("forgetting %x: %w", hash, err)
}

// NodeKey is the SHA256 node's identity key, hex.
func (r *Remote) NodeKey(ctx context.Context) (string, error) {
	info, err := r.main.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return "", fmt.Errorf("reading the SHA256 node's identity: %w",
			err)
	}
	r.noteKey(info.GetIdentityPubkey())

	return info.GetIdentityPubkey(), nil
}

// IdentityKey is the SHA256 node's identity key, as a swap records the node
// that issued its hold invoice.
//
// From the bridge's last GetInfo when that is recent, as routeHeight takes
// the tip: every toBLAKE2b quote asks for this, and stock lnd can take most
// of a minute to answer GetInfo, which a quote would spend out of the
// invoice's remaining life. The bridge polls GetInfo every pollInterval, and
// a node behind this address is replaced by another only by an operator, not
// within that.
func (r *Remote) IdentityKey(ctx context.Context) (string, error) {
	r.tipMu.Lock()
	key, at := r.key, r.keyAt
	r.tipMu.Unlock()
	if key != "" && r.now().Sub(at) <= routeTipAge {
		return key, nil
	}

	got, err := r.NodeKey(ctx)
	if err != nil {
		return "", err
	}

	return checkedKey(got)
}

// noteKey keeps an identity key a GetInfo gave, if it is one.
func (r *Remote) noteKey(key string) {
	key, err := checkedKey(key)
	if err != nil {
		return
	}
	r.tipMu.Lock()
	r.key, r.keyAt = key, r.now()
	r.tipMu.Unlock()
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
			TimeoutSeconds: node.TimeoutSeconds(req.Timeout),
			FeeLimitMsat:   int64(req.MaxFeeMsat),
			CltvLimit:      int32(req.CLTVLimit),
		},
	)
	if err != nil {
		if neverDelivered(err) || r.refusedUnrecorded(ctx, req, err) {
			return node.Payment{}, fmt.Errorf("sending: %w: %w",
				node.ErrNotSent, err)
		}

		return node.Payment{}, fmt.Errorf("sending: %w", err)
	}

	// Anything short of a terminal answer is in flight. Starting from that
	// rather than from the zero value means a stream that dies early cannot
	// be read as "never started".
	last := node.Payment{State: node.PaymentInFlight}
	for received := false; ; received = true {
		update, err := stream.Recv()
		if err != nil {
			// A refusal before the node has said anything about the
			// payment, from the part of lnd that runs before any
			// handler, means the router never saw it.
			if !received && neverDelivered(err) {
				return node.Payment{}, fmt.Errorf("sending: %w: %w",
					node.ErrNotSent, err)
			}

			// A refusal from the handler itself, before it wrote
			// a payment record (a CLTV limit too tight for the
			// invoice, say), is as unsent, and the node says so
			// by having no record of the hash.
			if !received && r.refusedUnrecorded(ctx, req, err) {
				return node.Payment{}, fmt.Errorf("sending: %w: %w",
					node.ErrNotSent, err)
			}

			// EOF, a broken stream or a passed deadline. The
			// payment is not known to have failed, and saying so
			// would let the caller cancel the incoming claim
			// against an HTLC still in flight.
			return last, nil
		}

		status, err := TranslateRemotePayment(update)
		if err != nil {
			return node.Payment{}, contradiction(err)
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
		return node.Payment{}, contradiction(err)
	}

	return payment(st), nil
}

// contradiction reports a node contradicting itself as the bridge module's
// node.ErrContradiction as well as this package's ErrNodeContradiction, so the
// runner can count it.
func contradiction(err error) error {
	if errors.Is(err, ErrNodeContradiction) {
		return fmt.Errorf("%w: %w", node.ErrContradiction, err)
	}

	return err
}

// Check confirms the node answers and is synced to its chain.
//
// Both conditions, so it reports whether the bridge can quote right now. Use
// Reachable at startup instead.
func (r *Remote) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := r.BlockHeight(ctx); err != nil {
		return fmt.Errorf("SHA256 node: %w", err)
	}

	return nil
}

// sha256InfoTimeout bounds one GetInfo on the SHA256 node.
//
// Longer than any other call is given: stock lnd answers it only after asking
// its chain node for the best block and whether it is synced, through a
// client that queues every request and retries, so a busy chain node holds it
// up for tens of seconds while the node is otherwise fine. Every wait on it
// ends with the bridge too, so stopping is no slower for it.
const sha256InfoTimeout = time.Minute

// Reachable confirms the node answers, without requiring that it has caught up.
//
// This is what startup needs. Dialling succeeds against a node that is not
// there, so a wrong address, a wrong macaroon or a node that is simply down
// has to be found here rather than by a swap that has already accepted
// someone's money. Being merely behind is different: it is temporary, it is
// already refused at quote time, and refusing to start on it would mean a
// SHA256 node restart takes this one down too.
func (r *Remote) Reachable(ctx context.Context) (BlockInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, sha256InfoTimeout)
	defer cancel()

	info, err := r.BestBlock(ctx)
	if err != nil {
		return BlockInfo{}, fmt.Errorf("SHA256 node: %w", err)
	}

	return info, nil
}

// CheckChain confirms the node is a Lightning node on the SHA256 chain, on the
// same network as this one, and not this node.
//
// Both chains run lnd, both report "bitcoin" and the same network names, and
// both read each other's invoices, so a misconfigured address that pointed at
// another Lightning Fork node would answer every call and start. What tells
// them apart is option_blake2b: a Lightning Fork node always advertises it,
// and a stock lnd never does. Checking a chain's identity at the node, not
// only at its chain backend, is the lesson this exists for.
func (r *Remote) CheckChain(ctx context.Context, network,
	localKey string) error {

	ctx, cancel := context.WithTimeout(ctx, sha256InfoTimeout)
	defer cancel()

	info, err := r.main.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return fmt.Errorf("SHA256 node: %w", err)
	}

	for _, bit := range node.BLAKE2bFeatureBits {
		if _, ok := info.GetFeatures()[bit]; ok {
			return fmt.Errorf("%w: the node at the SHA256 address "+
				"advertises option_blake2b, so it is a Lightning "+
				"Fork node on the BLAKE2b chain; point "+
				"bridgerpc.sha256.rpchost at a stock lnd on the "+
				"SHA256 chain", ErrConfig)
		}
	}
	if localKey != "" && strings.EqualFold(info.GetIdentityPubkey(),
		localKey) {

		// Same identity key: this node itself, or a node created from
		// the same seed. The second is the dangerous one. A seed from
		// before the fork controls coins that exist on both chains, and
		// the SHA256 node's plain SIGHASH_ALL spends of them replay
		// onto this chain, so the two must never share one.
		return fmt.Errorf("%w: the SHA256 address reaches a node with "+
			"this node's own identity: either this node itself, or "+
			"one created from the same seed. The two must not share "+
			"a seed (coins from before the fork exist on both "+
			"chains, and the SHA256 node's spends of them replay "+
			"onto this one); give the SHA256 node a seed of its "+
			"own, or use bridgerpc.sha256.supervised, which derives "+
			"one", ErrConfig)
	}

	var got string
	for _, c := range info.GetChains() {
		if c.GetChain() == "bitcoin" {
			got = c.GetNetwork()
		}
	}
	if network != "" && got != network {
		return fmt.Errorf("%w: the SHA256 node is on %q and this node "+
			"on %q", ErrConfig, got, network)
	}

	return nil
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
		return 0, fmt.Errorf("reading the SHA256 node's channels: %w",
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
			return 0, fmt.Errorf("the SHA256 node reports an "+
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
		return BlockInfo{}, fmt.Errorf("reading the SHA256 node's "+
			"tip: %w", err)
	}

	h := info.GetBlockHeight()
	if h > math.MaxInt32 {
		return BlockInfo{}, fmt.Errorf("the SHA256 node reports an "+
			"implausible height %d", h)
	}

	out := BlockInfo{
		Height:        int32(h),
		SyncedToChain: info.GetSyncedToChain(),
	}
	if ts := info.GetBestHeaderTimestamp(); ts > 0 {
		out.Time = time.Unix(ts, 0)
	}
	// A node that has fallen behind leaves no tip for routeHeight, which
	// then asks the height and is refused, as before.
	r.noteKey(info.GetIdentityPubkey())
	r.tipMu.Lock()
	if out.SyncedToChain {
		r.tip, r.tipAt = out.Height, r.now()
	} else {
		r.tip, r.tipAt = 0, time.Time{}
	}
	r.tipMu.Unlock()

	return out, nil
}

// routeTipAge is how old a tip BestBlock read may be for routeHeight to use
// it: the bridge reads one every pollInterval, and on a node slow to answer
// GetInfo a read can take most of a minute more.
const routeTipAge = 3 * pollInterval

// routeHeight is the height a route's CLTV is counted from: the tip the
// bridge last read, when it is recent, and otherwise the node's height now.
//
// A route search runs at every quote and again before every payout, and on a
// node slow to answer GetInfo (stock lnd asks its chain node first) reading
// the height there each time made the route check outlast its deadline, so a
// funded swap waited at the gate until it was given back. A tip read a little
// earlier is at most the node's height now, so the route's CLTV counted from
// it is at least the real one: the payer is asked to hold a block or two
// longer, never shorter. (The height the payout gate measures the held
// payment against is read fresh, by BlockHeight.)
func (r *Remote) routeHeight(ctx context.Context) (int32, error) {
	r.tipMu.Lock()
	tip, at := r.tip, r.tipAt
	r.tipMu.Unlock()
	if !at.IsZero() && r.now().Sub(at) <= routeTipAge {
		return tip, nil
	}

	return r.BlockHeight(ctx)
}

// FindRoute asks the SHA256 node for a route to the invoice's destination
// within a routing budget and a CLTV limit, before a quote commits anything.
// node.ErrNoRoute when it has none. The route's CLTV is counted from the
// node's height, as the payment's limit is.
//
// The invoice is read by the node again for what pathfinding needs (its
// destination, final CLTV, route hints and features, or its blinded paths): a
// route to a private destination exists only through its hints. A payment the
// search finds no single path for but which may be split is routable when the
// node's balance can send it (see splitRoute).
func (r *Remote) FindRoute(ctx context.Context, invoice string,
	maxFeeMsat uint64, cltvLimit uint32) (node.Route, error) {

	if maxFeeMsat > math.MaxInt64 || cltvLimit > math.MaxInt32 {
		return node.Route{}, fmt.Errorf("limits %d msat, %d blocks do "+
			"not fit", maxFeeMsat, cltvLimit)
	}
	req, err := r.main.DecodePayReq(
		ctx, &lnrpc.PayReqString{PayReq: invoice},
	)
	if err != nil {
		return node.Route{}, fmt.Errorf("%w: %w", ErrInvoice, err)
	}
	height, err := r.routeHeight(ctx)
	if err != nil {
		return node.Route{}, err
	}

	features := make([]lnrpc.FeatureBit, 0, len(req.GetFeatures()))
	for bit := range req.GetFeatures() {
		features = append(features, lnrpc.FeatureBit(bit))
	}
	search := func(ctx context.Context, amtMsat uint64) (node.Route,
		error) {

		res, err := r.main.QueryRoutes(ctx, routeQuery(
			req.GetDestination(), int64(amtMsat),
			int32(req.GetCltvExpiry()), req.GetRouteHints(),
			features, req.GetBlindedPaths(), maxFeeMsat,
			cltvLimit,
		))

		return routeFrom(res, err, height)
	}

	if req.GetNumMsat() < 0 {
		return node.Route{}, fmt.Errorf("%w: amount %d msat",
			ErrInvoice, req.GetNumMsat())
	}
	route, err := search(ctx, uint64(req.GetNumMsat()))
	if !errors.Is(err, errNoSinglePath) || !remoteSplits(req) ||
		req.GetNumMsat() <= 0 {

		return route, err
	}

	return splitRoute(ctx, err, r.Balance, search,
		uint64(req.GetNumMsat()), maxFeeMsat, cltvLimit)
}

// remoteSplits reports whether the SHA256 node's payment to the invoice may be
// split, by the rule its router applies: the invoice declares multi-part
// payments (feature bit 16 or 17), and has a payment address or blinded paths
// to carry the parts. For an invoice with blinded paths the router reads the
// paths' features in place of the invoice's when they have any (they all have
// the same).
func remoteSplits(req *lnrpc.PayReq) bool {
	mpp := func(bit uint32) bool {
		return bit == uint32(lnwire.MPPOptional) ||
			bit == uint32(lnwire.MPPRequired)
	}

	declared := false
	if paths := req.GetBlindedPaths(); len(paths) > 0 &&
		len(paths[0].GetFeatures()) > 0 {

		for _, bit := range paths[0].GetFeatures() {
			declared = declared || mpp(uint32(bit))
		}
	} else {
		for bit := range req.GetFeatures() {
			declared = declared || mpp(bit)
		}
	}
	if !declared {
		return false
	}

	return len(req.GetPaymentAddr()) > 0 || len(req.GetBlindedPaths()) > 0
}

// BlockAt is a block's header by height, for seeding the chain observer.
//
// It goes through ChainKit, which a stock lnd only serves when built with the
// chainrpc tag. A node without it answers Unimplemented, which the caller
// treats as "no history available" and falls back to learning from tips. That
// is slow rather than wrong, so it is worth trying and not worth requiring.
func (r *Remote) BlockAt(ctx context.Context, height int32) (BlockInfo, error) {
	// Backfill runs inside the node, so a nil client here would take the
	// whole daemon down for a history read it can do without.
	if r == nil || r.chain == nil {
		return BlockInfo{}, errors.New("no chain client for the " +
			"SHA256 node")
	}
	if height < 0 {
		return BlockInfo{}, fmt.Errorf("height %d is not a block",
			height)
	}

	hash, err := r.chain.GetBlockHash(
		ctx, &chainrpc.GetBlockHashRequest{BlockHeight: int64(height)},
	)
	if err != nil {
		return BlockInfo{}, fmt.Errorf("the SHA256 node's hash for "+
			"height %d: %w", height, err)
	}

	hdr, err := r.chain.GetBlockHeader(
		ctx, &chainrpc.GetBlockHeaderRequest{
			BlockHash: hash.GetBlockHash(),
		},
	)
	if err != nil {
		return BlockInfo{}, fmt.Errorf("the SHA256 node's header at "+
			"height %d: %w", height, err)
	}

	raw := hdr.GetRawBlockHeader()
	if len(raw) < 80 {
		return BlockInfo{}, fmt.Errorf("the SHA256 node returned a "+
			"%d byte header at height %d", len(raw), height)
	}

	// Bytes 68 to 72 of a block header are its timestamp, little endian.
	// Decoding the four bytes rather than the whole header keeps this
	// independent of which chain's header format this build parses, which
	// matters because this binary's own parser is the BLAKE2b one.
	ts := binary.LittleEndian.Uint32(raw[68:72])

	return BlockInfo{
		Height: height, Time: time.Unix(int64(ts), 0),

		// Bytes 72 to 76 are the compact difficulty target, which the
		// observer needs to carry spacing across a retarget.
		Bits: binary.LittleEndian.Uint32(raw[72:76]),

		// Historical, so by definition already in the chain.
		SyncedToChain: true,
	}, nil
}

// neverDelivered reports whether a failed call provably never reached the
// node's handler, so that a payment it carried cannot be in flight.
//
// Two kinds of failure qualify, and only these:
//
//   - No connection at all. gRPC fails a call at once when it has no
//     transport, before writing anything, and says it was dialling.
//   - lnd's interceptor refusing the call before any handler runs, which it
//     does while starting and while its wallet is locked or absent, and for
//     a macaroon it does not accept.
//
// Everything else is left ambiguous on purpose. A connection that drops after
// the request was written, a reset, a timeout: the request may have been
// delivered, and calling it unsent would let the swap be paid again.
func neverDelivered(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	msg := st.Message()

	switch st.Code() {
	case codes.Unavailable:
		// The transport could not be made. "transport is closing" and
		// the like are absent deliberately: they follow a connection
		// that existed.
		return strings.Contains(msg, "Error while dialing") ||
			strings.Contains(msg, "produced zero addresses")

	case codes.Unimplemented:
		// The router sub-server is not registered yet: lnd registers
		// it once the wallet is unlocked.
		return strings.Contains(msg, "unknown service routerrpc.Router")
	}

	// lnd's own refusals before any handler (rpcperms), matched whole:
	// a payment that has started can fail with text of its own (an RPC
	// middleware's, a database's "permission denied for table"), and only
	// these exact sentences come from before the payment exists.
	switch msg {
	case rpcperms.ErrWaitingToStart.Error(),
		rpcperms.ErrWalletLocked.Error(),
		rpcperms.ErrNoWallet.Error(),
		rpcperms.ErrRPCStarting.Error():

		return true
	}

	// The macaroon check, also before the handler: macaroon-bakery's own
	// errors (a refused permission or signature, a revoked root key),
	// which reach the client with codes.Unknown.
	if st.Code() == codes.Unknown && (msg == "permission denied" ||
		strings.HasPrefix(msg, "verification failed: ") ||
		strings.HasPrefix(msg, "cannot get macaroon: ")) {

		return true
	}

	return false
}

// refusedUnrecorded reports whether SendPaymentV2 failed with a refusal its
// handler sent before dispatching anything, for a payment the node holds no
// record of.
//
// The handler checks the request (the invoice, the CLTV limit against the
// invoice's final delta) before it writes the payment record, and answers a
// failed check with a status of its own: Unknown, InvalidArgument, or
// AlreadyExists for a hash it has already paid or is paying. Read as in
// flight, such a refusal leaves the swap paying with nothing for the node to
// look up, and an operator to sort it out. The code alone is not enough,
// because a payment that has started can fail with Unknown too, so the node is
// asked: no record of the hash is what makes it unsent.
//
// Unavailable, Canceled, DeadlineExceeded and Internal are left alone: they
// come from the transport or from a failure partway, after which the request
// may have been delivered. So is any doubt in the asking: an invoice that
// cannot be read back, or a lookup that fails, keeps the payment in flight.
func (r *Remote) refusedUnrecorded(ctx context.Context, req node.PayRequest,
	err error) bool {

	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unknown, codes.InvalidArgument, codes.AlreadyExists:
	default:
		return false
	}

	dec, decErr := r.main.DecodePayReq(
		ctx, &lnrpc.PayReqString{PayReq: req.Invoice},
	)
	if decErr != nil {
		return false
	}
	h, decErr := hex.DecodeString(dec.GetPaymentHash())
	var hash node.Hash
	if decErr != nil || len(h) != len(hash) {
		return false
	}
	copy(hash[:], h)

	pmt, lookupErr := r.LookupPayment(ctx, hash)

	return lookupErr == nil && pmt.State == node.PaymentNone
}

// A quote asks the SHA256 node for a route before committing anything.
var _ node.RouteFinder = (*Remote)(nil)

// invoiceAbsent reports whether a lookup failed because the node has no such
// invoice. lnd answers NotFound for that, except on a node that has never
// made an invoice at all, which answers "there are no existing invoices" with
// no code; a new SHA256 node is one, until its first swap.
func invoiceAbsent(err error) bool {
	return status.Code(err) == codes.NotFound ||
		strings.Contains(err.Error(), "there are no existing invoices")
}
