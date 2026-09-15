// Package offerpay is the payer's side of BOLT 12: it fetches an invoice for
// an offer by sending a signed invoice request over onion messages and
// waiting for the invoice on a reply path, checks the invoice against the
// request, and turns it into a payment the router can make.
package offerpay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmsg"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// DefaultTimeout is how long a fetch waits for the invoice.
	DefaultTimeout = 60 * time.Second

	// metadataBytes is the size of invreq_metadata, which the spec wants
	// unpredictable.
	metadataBytes = 16

	// MaxPayerNoteBytes bounds invreq_payer_note.
	MaxPayerNoteBytes = 1024
)

var (
	// ErrAmountRequired is returned when the offer names no amount and
	// the caller gave none.
	ErrAmountRequired = errors.New("the offer has no amount; one must " +
		"be given")

	// ErrAmountNotAllowed is returned when the caller gives an amount
	// below what the offer asks.
	ErrAmountBelowOffer = errors.New("amount is below the offer's amount")

	// ErrQuantityNotOffered is returned when a quantity is given for an
	// offer that does not sell by quantity.
	ErrQuantityNotOffered = errors.New("the offer does not take a " +
		"quantity")

	// ErrQuantityRequired is returned when an offer that sells by
	// quantity is fetched without one.
	ErrQuantityRequired = errors.New("the offer needs a quantity")

	// ErrTimeout is returned when no invoice arrives in time.
	ErrTimeout = errors.New("no invoice arrived in time")

	// ErrInvoiceError is returned, wrapped around the message, when the
	// issuer answers with an invoice_error.
	ErrInvoiceError = errors.New("the issuer refused")

	// ErrUnreachable is returned when the offer names neither a node nor
	// a path that a request could be sent over.
	ErrOfferUnreachable = errors.New("the offer names no way to reach " +
		"its issuer")
)

// Messenger is what the client needs from the onion-message layer.
type Messenger interface {
	// Send sends a payload to a destination.
	Send(ctx context.Context, dest onionmsg.Destination,
		payload []*lnwire.FinalHopTLV, replyPath *lnwire.BlindedPath,
		opts ...onionmsg.SendOption) error

	// BuildReplyPath builds a path to this node carrying pathID.
	BuildReplyPath(ctx context.Context,
		pathID []byte) (*lnwire.BlindedPath, error)

	// OnInvoice and OnInvoiceError register handlers.
	OnInvoice(onionmsg.Handler)
	OnInvoiceError(onionmsg.Handler)
}

// Config holds what the client needs.
type Config struct {
	// Messenger sends requests and delivers replies.
	Messenger Messenger

	// ChainHash is the chain the offer must name.
	ChainHash [32]byte

	// Clock is the source of time.
	Clock clock.Clock

	// Timeout is how long a fetch waits; zero means DefaultTimeout.
	Timeout time.Duration
}

// FetchParams describes the invoice to ask for.
type FetchParams struct {
	// Offer is the offer to fetch an invoice for.
	Offer *bolt12.Offer

	// AmountMsat is the amount to ask to be invoiced. Required when the
	// offer names no amount; otherwise it may raise the offer's amount
	// (a tip) but not lower it.
	AmountMsat uint64

	// Quantity is how many of the offer's item, for an offer that sells
	// by quantity.
	Quantity uint64

	// PayerNote is a note for the issuer, invreq_payer_note.
	PayerNote string
}

// Fetched is an invoice fetched for an offer, with what was asked.
type Fetched struct {
	// Invoice is the signed invoice.
	Invoice *bolt12.Invoice

	// Encoded is the invoice's TLV bytes; Bolt12 its lni1... string.
	Encoded []byte
	Bolt12  string

	// Request is the request the invoice answers.
	Request *bolt12.InvoiceRequest

	// PayerKey is the key the request was signed with. Keeping it lets
	// the payer later prove it was the one that paid.
	PayerKey *btcec.PrivateKey

	// AmountMsat is the invoiced amount.
	AmountMsat uint64

	// PaymentHash is the invoice's payment hash.
	PaymentHash [32]byte
}

// pending is a fetch waiting for its reply.
type pending struct {
	reply chan *onionmsg.Inbound
}

// Client fetches invoices for offers.
type Client struct {
	cfg Config

	mu      sync.Mutex
	pending map[[32]byte]*pending
	started sync.Once
}

// New returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.Messenger == nil {
		return nil, errors.New("offerpay: messenger required")
	}
	if cfg.ChainHash == [32]byte{} {
		return nil, errors.New("offerpay: chain hash required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}

	return &Client{
		cfg:     cfg,
		pending: make(map[[32]byte]*pending),
	}, nil
}

// Start registers the client's reply handlers.
func (c *Client) Start() error {
	c.started.Do(func() {
		c.cfg.Messenger.OnInvoice(c.deliver)
		c.cfg.Messenger.OnInvoiceError(c.deliver)
	})

	return nil
}

// Stop is here for symmetry; handlers stop with the messenger.
func (c *Client) Stop() error {
	return nil
}

// deliver hands a reply to the fetch waiting for it, matched by the path_id
// of the reply path it came over. Replies over no path of ours, or for a
// fetch that is no longer waiting, are dropped.
func (c *Client) deliver(_ context.Context, msg *onionmsg.Inbound) {
	if len(msg.PathID) != 32 {
		// Loud, because a reply that reaches this node and is dropped
		// here looks from the outside exactly like a reply that never
		// arrived: the fetch waits out its timeout with nothing in the
		// log to say why.
		log.Debugf("Dropping reply from peer %x: path_id is %d bytes, "+
			"want 32 (%x)", msg.Peer, len(msg.PathID), msg.PathID)

		return
	}
	var key [32]byte
	copy(key[:], msg.PathID)

	c.mu.Lock()
	p, ok := c.pending[key]
	c.mu.Unlock()
	if !ok {
		log.Debugf("Reply from peer %x for no pending fetch", msg.Peer)

		return
	}
	select {
	case p.reply <- msg:
	default:
		// One reply is all a fetch takes.
	}
}

// FetchInvoice asks the offer's issuer for an invoice and waits for it.
func (c *Client) FetchInvoice(ctx context.Context,
	params FetchParams) (*Fetched, error) {

	if err := c.Start(); err != nil {
		return nil, err
	}
	ir, payerKey, err := c.buildRequest(params)
	if err != nil {
		return nil, err
	}
	encoded, err := ir.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	// A reply path of ours with a fresh secret, so the reply is matched
	// to this fetch and nothing else can pose as it. The messenger builds
	// the path once it knows how the request leaves (see
	// onionmsg.ReplyPathID).
	var pathID [32]byte
	if _, err := rand.Read(pathID[:]); err != nil {
		return nil, err
	}
	p := &pending{reply: make(chan *onionmsg.Inbound, 1)}
	c.mu.Lock()
	c.pending[pathID] = p
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, pathID)
		c.mu.Unlock()
	}()

	usedPath, err := c.send(ctx, params.Offer, encoded, pathID[:])
	if err != nil {
		return nil, err
	}

	timeout := time.NewTimer(c.cfg.Timeout)
	defer timeout.Stop()
	var msg *onionmsg.Inbound
	select {
	case msg = <-p.reply:
	case <-timeout.C:
		return nil, ErrTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if msg.InvoiceError != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvoiceError,
			msg.InvoiceError.Message)
	}
	inv := msg.Invoice
	if inv == nil {
		return nil, errors.New("reply carries no invoice")
	}
	if err := c.checkInvoice(inv, ir, params, usedPath); err != nil {
		return nil, fmt.Errorf("invoice refused: %w", err)
	}
	raw := msg.Records[uint64(onionmsg.TypeInvoice)]
	str, err := bolt12.Encode(bolt12InvoiceHRP, raw)
	if err != nil {
		return nil, err
	}
	var hash [32]byte
	inv.InvoicePaymentHash.WhenSome(
		func(r tlv.RecordT[tlv.TlvType168, [32]byte]) { hash = r.Val },
	)

	return &Fetched{
		Invoice:     inv,
		Encoded:     raw,
		Bolt12:      str,
		Request:     ir,
		PayerKey:    payerKey,
		AmountMsat:  uint64(inv.InvoiceAmount.ValOpt().UnwrapOr(0)),
		PaymentHash: hash,
	}, nil
}

// bolt12InvoiceHRP is the bech32 prefix of an invoice.
const bolt12InvoiceHRP = "lni"

// buildRequest makes and signs the request for the parameters.
func (c *Client) buildRequest(params FetchParams) (*bolt12.InvoiceRequest,
	*btcec.PrivateKey, error) {

	offer := params.Offer
	if offer == nil {
		return nil, nil, errors.New("offer required")
	}
	now := c.cfg.Clock.Now()
	if err := bolt12.ValidateOfferRead(
		offer, now, c.cfg.ChainHash, nil,
	); err != nil {
		return nil, nil, fmt.Errorf("offer: %w", err)
	}
	if len(params.PayerNote) > MaxPayerNoteBytes {
		return nil, nil, errors.New("payer note too long")
	}

	// Amount and quantity, per the offer's terms.
	offerAmount := uint64(offer.OfferAmount.ValOpt().UnwrapOr(0))
	quantityMax, sellsByQuantity := uint64(0), false
	offer.OfferQuantityMax.WhenSome(
		func(r tlv.RecordT[tlv.TlvType20, bolt12.TUint64]) {
			quantityMax, sellsByQuantity = uint64(r.Val), true
		},
	)
	switch {
	case params.Quantity != 0 && !sellsByQuantity:
		return nil, nil, ErrQuantityNotOffered
	case params.Quantity == 0 && sellsByQuantity:
		return nil, nil, ErrQuantityRequired
	case sellsByQuantity && quantityMax != 0 && params.Quantity > quantityMax:
		return nil, nil, fmt.Errorf("quantity above the offer's "+
			"maximum of %d", quantityMax)
	}
	quantity := params.Quantity
	if quantity == 0 {
		quantity = 1
	}
	switch {
	case offerAmount == 0 && params.AmountMsat == 0:
		return nil, nil, ErrAmountRequired
	case offerAmount != 0 && params.AmountMsat != 0 &&
		params.AmountMsat < offerAmount*quantity:

		return nil, nil, ErrAmountBelowOffer
	}

	payerKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, nil, err
	}
	metadata := make([]byte, metadataBytes)
	if _, err := rand.Read(metadata); err != nil {
		return nil, nil, err
	}
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payerKey.PubKey(), metadata, c.cfg.ChainHash,
	)
	if err != nil {
		return nil, nil, err
	}
	if params.AmountMsat != 0 {
		ir.InvreqAmount = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType82](
				bolt12.TUint64(params.AmountMsat),
			),
		)
	}
	if params.Quantity != 0 {
		ir.InvreqQuantity = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType86](
				bolt12.TUint64(params.Quantity),
			),
		)
	}
	if params.PayerNote != "" {
		ir.InvreqPayerNote = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType89](
				tlv.Blob(params.PayerNote),
			),
		)
	}
	sig, err := bolt12.SignInvoiceRequest(ir, payerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sign request: %w", err)
	}
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)
	if err := bolt12.ValidateInvoiceRequestWriteOnChain(
		ir, c.cfg.ChainHash,
	); err != nil {
		return nil, nil, fmt.Errorf("request: %w", err)
	}

	return ir, payerKey, nil
}

// send delivers the request to the offer's issuer: over one of its paths,
// trying each until one is sent, or, for an offer without paths, to its
// node id. The spec has requests go over the paths when there are any, and
// an issuer ignores a request for such an offer that comes any other way.
// The path used is returned, or nil for the node id.
func (c *Client) send(ctx context.Context, offer *bolt12.Offer,
	encoded []byte, replyPathID []byte) (*lnwire.BlindedPath, error) {

	payload := []*lnwire.FinalHopTLV{{
		TLVType: onionmsg.TypeInvoiceRequest, Value: encoded,
	}}

	var paths []lnwire.BlindedPath
	offer.OfferPaths.WhenSome(
		func(r tlv.RecordT[tlv.TlvType16, lnwire.BlindedPaths]) {
			paths = r.Val.Paths
		},
	)
	if len(paths) > 0 {
		var lastErr error
		for i := range paths {
			err := c.cfg.Messenger.Send(ctx, onionmsg.Destination{
				Path: &paths[i],
			}, payload, nil, onionmsg.AllowConnect(),
				onionmsg.ReplyPathID(replyPathID))
			if err == nil {
				return &paths[i], nil
			}
			lastErr = err
			log.Debugf("Offer path %d could not be used: %v", i, err)
		}

		return nil, fmt.Errorf("sending the request over the offer's "+
			"paths: %w", lastErr)
	}

	var issuer *btcec.PublicKey
	offer.OfferIssuerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType22, *btcec.PublicKey]) {
			issuer = r.Val
		},
	)
	if issuer == nil {
		return nil, ErrOfferUnreachable
	}
	err := c.cfg.Messenger.Send(ctx, onionmsg.Destination{
		NodeID: issuer,
	}, payload, nil, onionmsg.AllowConnect(),
		onionmsg.ReplyPathID(replyPathID))
	if err != nil {
		return nil, fmt.Errorf("sending the request: %w", err)
	}

	return nil, nil
}

// checkInvoice makes the payer's checks on an invoice.
func (c *Client) checkInvoice(inv *bolt12.Invoice, ir *bolt12.InvoiceRequest,
	params FetchParams, usedPath *lnwire.BlindedPath) error {

	if err := CheckInvoice(inv, c.cfg.ChainHash, c.cfg.Clock.Now()); err != nil {
		return err
	}
	if err := bolt12.ValidateInvoiceAgainstRequest(inv, ir); err != nil {
		return err
	}

	// An offer with no issuer id names its issuer by the last blinded
	// node of the path the request went over; the invoice must be
	// signed by that key.
	if !params.Offer.OfferIssuerID.IsSome() {
		if usedPath == nil || len(usedPath.Hops) == 0 {
			return errors.New("the offer names no issuer and the " +
				"request went over no path")
		}
		last := usedPath.Hops[len(usedPath.Hops)-1].BlindedNodeID
		nodeID := inv.InvoiceNodeID.ValOpt().UnwrapOr(nil)
		if nodeID == nil || last == nil || !nodeID.IsEqual(last) {
			return errors.New("invoice_node_id is not the path's " +
				"final blinded node")
		}
	}

	// The amount is what was asked, or the offer's price.
	amount := uint64(inv.InvoiceAmount.ValOpt().UnwrapOr(0))
	want := params.AmountMsat
	if want == 0 {
		quantity := params.Quantity
		if quantity == 0 {
			quantity = 1
		}
		want = uint64(params.Offer.OfferAmount.ValOpt().UnwrapOr(0)) *
			quantity
	}
	if amount != want {
		return fmt.Errorf("invoice is for %d msat, not %d", amount, want)
	}

	return nil
}

// CheckInvoice makes the checks a payer makes on an invoice it did not just
// fetch: the signature, the reader checks for the chain, and that it has
// not expired. The invoice's request is not at hand, so it is not checked
// against one.
func CheckInvoice(inv *bolt12.Invoice, chain [32]byte, now time.Time) error {
	if err := bolt12.VerifyInvoice(inv); err != nil {
		return err
	}
	if err := bolt12.ValidateInvoiceRead(
		inv, chain, bolt12.InvoiceFeatureCatalogues{},
	); err != nil {
		return err
	}
	created := int64(inv.InvoiceCreatedAt.ValOpt().UnwrapOr(0))
	relExp := int64(inv.InvoiceRelativeExp.ValOpt().UnwrapOr(7200))
	if now.Unix() > created+relExp {
		return errors.New("invoice has already expired")
	}

	return nil
}
