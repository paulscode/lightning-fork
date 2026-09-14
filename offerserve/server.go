// Package offerserve answers invoice requests for this node's offers: it
// checks a request against the offer it names, creates a Lightning invoice
// with blinded payment paths, wraps it as a signed BOLT 12 invoice and sends
// it back over the requester's reply path. This is the path a mining pool's
// payout takes.
package offerserve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/neutrino/cache/lru"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/onionmsg"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/zpay32"
	"golang.org/x/time/rate"
)

const (
	// DefaultInvoiceExpiry is how long an invoice issued for an offer is
	// payable.
	DefaultInvoiceExpiry = time.Hour

	// DefaultRequestsPerSecond and DefaultRequestBurst bound how many
	// requests the node answers, over all peers; DefaultPeerRequestsPerSecond
	// and DefaultPeerRequestBurst bound one peer. Each answered request
	// creates an invoice in the registry, so these are kept low.
	DefaultRequestsPerSecond     = 5
	DefaultRequestBurst          = 20
	DefaultPeerRequestsPerSecond = 1
	DefaultPeerRequestBurst      = 5

	// peerLimiters is how many peers' limiters are remembered.
	peerLimiters = 1000

	// DefaultInvoiceRetention is how long an issued invoice's record is
	// kept after it can no longer be paid, unless it was settled.
	DefaultInvoiceRetention = 7 * 24 * time.Hour

	// pruneInterval is how often unpaid, expired records are pruned.
	pruneInterval = time.Hour

	// recentRequests is how many recent requests are remembered so a
	// repeated request gets its invoice again instead of a new one.
	recentRequests = 1000

	// invoiceSignatureTag is the BOLT 12 tag for an invoice signature.
	invoiceSignatureTag = "lightninginvoicesignature"
)

var (
	// ErrAmountRequired is sent back when neither the offer nor the
	// request says how much to invoice.
	ErrAmountRequired = errors.New("amount required")

	// ErrNoReplyPath is logged when a request carries no reply path, so
	// nothing can be sent back.
	ErrNoReplyPath = errors.New("request carries no reply path")
)

// Messenger is what the server needs from the onion-message layer.
type Messenger interface {
	// OnInvoiceRequest registers the handler for delivered requests.
	OnInvoiceRequest(onionmsg.Handler)

	// Send sends a payload to a destination.
	Send(ctx context.Context, dest onionmsg.Destination,
		payload []*lnwire.FinalHopTLV, replyPath *lnwire.BlindedPath,
		opts ...onionmsg.SendOption) error
}

// Signer signs with a key the node holds, possibly on a remote signer.
type Signer interface {
	SignMessageSchnorr(keyLoc keychain.KeyLocator, msg []byte,
		doubleHash bool, taprootTweak []byte,
		tag []byte) (*schnorr.Signature, error)
}

// CreatedInvoice is the Lightning invoice created for a request.
type CreatedInvoice struct {
	// PaymentHash is the invoice's payment hash.
	PaymentHash [32]byte

	// Paths are the blinded payment paths to this node.
	Paths []*zpay32.BlindedPaymentPath

	// CreatedAt is when the invoice was created.
	CreatedAt time.Time

	// Expiry is how long after CreatedAt the invoice is payable.
	Expiry time.Duration

	// Features are the invoice's features.
	Features *lnwire.FeatureVector
}

// Config holds what the server needs.
type Config struct {
	// Manager holds the offers.
	Manager *offers.Manager

	// Invoices records the invoices issued.
	Invoices *offers.InvoiceStore

	// Messenger delivers requests and sends invoices.
	Messenger Messenger

	// ChainHash is the chain requests must name.
	ChainHash [32]byte

	// NodeKey is the key published as invoice_node_id, which must be the
	// offer's issuer key, and its locator for signing.
	NodeKey keychain.KeyDescriptor

	// Signer signs invoices with NodeKey.
	Signer Signer

	// AddInvoice creates a Lightning invoice with blinded payment paths
	// for the amount, payable for expiry.
	AddInvoice func(ctx context.Context, amountMsat uint64,
		description string, expiry time.Duration) (*CreatedInvoice,
		error)

	// InvoiceSettled reports whether the registry's invoice with the
	// payment hash was settled, for pruning records of invoices that
	// were not. Optional: without it nothing is pruned.
	InvoiceSettled func(ctx context.Context, hash [32]byte) (bool, error)

	// Clock is the source of time.
	Clock clock.Clock

	// InvoiceExpiry is how long issued invoices are payable; zero means
	// DefaultInvoiceExpiry.
	InvoiceExpiry time.Duration

	// RequestsPerSecond and RequestBurst bound how many requests are
	// answered over all peers; PeerRequestsPerSecond and PeerRequestBurst
	// bound one peer. Zero means the defaults.
	RequestsPerSecond     float64
	RequestBurst          int
	PeerRequestsPerSecond float64
	PeerRequestBurst      int

	// InvoiceRetention is how long the record of an invoice that was not
	// settled is kept after it expired; zero means the default.
	InvoiceRetention time.Duration
}

// Server answers invoice requests for this node's offers.
type Server struct {
	cfg     Config
	limiter *rate.Limiter

	// peers holds a limiter per peer that has sent requests.
	peers *lru.Cache[[33]byte, *peerLimiter]

	// recent maps the hash of a request's bytes to the invoice sent for
	// it, so a repeated request gets the same invoice back.
	recent *lru.Cache[[32]byte, *recentInvoice]

	started sync.Once
	quit    chan struct{}
	wg      sync.WaitGroup
}

// peerLimiter is one peer's rate limiter.
type peerLimiter struct {
	limiter *rate.Limiter
}

// Size implements lru.CacheableValue.
func (p *peerLimiter) Size() (uint64, error) { return 1, nil }

// recentInvoice is an invoice sent for a request, kept until it expires.
type recentInvoice struct {
	encoded []byte
	expiry  time.Time
}

// Size implements lru.CacheableValue.
func (r *recentInvoice) Size() (uint64, error) { return 1, nil }

// New returns a server.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Manager == nil, cfg.Invoices == nil, cfg.Messenger == nil,
		cfg.Signer == nil, cfg.AddInvoice == nil:

		return nil, errors.New("offerserve: manager, invoice store, " +
			"messenger, signer and add-invoice hook required")
	case cfg.NodeKey.PubKey == nil:
		return nil, errors.New("offerserve: node key required")
	case cfg.ChainHash == [32]byte{}:
		return nil, errors.New("offerserve: chain hash required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewDefaultClock()
	}
	if cfg.InvoiceExpiry <= 0 {
		cfg.InvoiceExpiry = DefaultInvoiceExpiry
	}
	if cfg.RequestsPerSecond <= 0 {
		cfg.RequestsPerSecond = DefaultRequestsPerSecond
	}
	if cfg.RequestBurst <= 0 {
		cfg.RequestBurst = DefaultRequestBurst
	}
	if cfg.PeerRequestsPerSecond <= 0 {
		cfg.PeerRequestsPerSecond = DefaultPeerRequestsPerSecond
	}
	if cfg.PeerRequestBurst <= 0 {
		cfg.PeerRequestBurst = DefaultPeerRequestBurst
	}
	if cfg.InvoiceRetention <= 0 {
		cfg.InvoiceRetention = DefaultInvoiceRetention
	}

	return &Server{
		cfg: cfg,
		limiter: rate.NewLimiter(
			rate.Limit(cfg.RequestsPerSecond), cfg.RequestBurst,
		),
		peers:  lru.NewCache[[33]byte, *peerLimiter](peerLimiters),
		recent: lru.NewCache[[32]byte, *recentInvoice](recentRequests),
		quit:   make(chan struct{}),
	}, nil
}

// Start registers the server with the messenger and starts pruning.
func (s *Server) Start() error {
	s.started.Do(func() {
		s.cfg.Messenger.OnInvoiceRequest(s.Handle)
		if s.cfg.InvoiceSettled != nil {
			s.wg.Add(1)
			go s.pruneLoop()
		}
	})

	return nil
}

// Stop stops pruning; handlers stop with the messenger.
func (s *Server) Stop() error {
	select {
	case <-s.quit:
	default:
		close(s.quit)
	}
	s.wg.Wait()

	return nil
}

// allowPeer applies the per-peer limit.
func (s *Server) allowPeer(peer [33]byte) bool {
	l, err := s.peers.Get(peer)
	if err != nil {
		l = &peerLimiter{limiter: rate.NewLimiter(
			rate.Limit(s.cfg.PeerRequestsPerSecond),
			s.cfg.PeerRequestBurst,
		)}
		_, _ = s.peers.Put(peer, l)
	}

	return l.limiter.Allow()
}

// pruneLoop drops the records of invoices that expired unpaid, once they
// are past the retention.
func (s *Server) pruneLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.Prune(context.Background())
		case <-s.quit:
			return
		}
	}
}

// Prune drops the records of invoices that expired unpaid and are past the
// retention. Exported for tests.
func (s *Server) Prune(ctx context.Context) {
	if s.cfg.InvoiceSettled == nil {
		return
	}
	issued, err := s.cfg.Invoices.List()
	if err != nil {
		log.Errorf("Listing issued invoices to prune: %v", err)

		return
	}
	cutoff := s.cfg.Clock.Now().Add(
		-s.cfg.InvoiceExpiry - s.cfg.InvoiceRetention,
	)
	for _, inv := range issued {
		if !inv.CreatedAt.Before(cutoff) {
			continue
		}
		settled, err := s.cfg.InvoiceSettled(ctx, inv.PaymentHash)
		if err != nil || settled {
			continue
		}
		if err := s.cfg.Invoices.Delete(inv.PaymentHash); err != nil {
			log.Errorf("Pruning issued invoice %x: %v",
				inv.PaymentHash, err)
		}
	}
}

// Handle answers one delivered invoice request. It is the messenger's
// handler, exported for tests.
func (s *Server) Handle(ctx context.Context, msg *onionmsg.Inbound) {
	if msg.InvoiceRequest == nil {
		return
	}
	// Nothing can be sent back without a reply path, so nothing is
	// done: no invoice is made for a request that cannot be answered.
	if msg.ReplyPath == nil {
		log.Debugf("Ignoring invoice request from peer %x: %v",
			msg.Peer, ErrNoReplyPath)

		return
	}
	if !s.allowPeer(msg.Peer) || !s.limiter.Allow() {
		log.Warnf("Dropping invoice request from peer %x: rate limit",
			msg.Peer)

		return
	}

	encoded, err := s.answer(ctx, msg)
	if err != nil {
		var reject *rejection
		if errors.As(err, &reject) {
			log.Infof("Refusing invoice request from peer %x: %v",
				msg.Peer, reject.cause)
			s.sendError(ctx, msg, reject.message)

			return
		}
		log.Warnf("Ignoring invoice request from peer %x: %v",
			msg.Peer, err)

		return
	}
	err = s.cfg.Messenger.Send(ctx, onionmsg.Destination{
		Path: msg.ReplyPath,
	}, []*lnwire.FinalHopTLV{{
		TLVType: onionmsg.TypeInvoice, Value: encoded,
	}}, nil)
	if err != nil {
		log.Warnf("Sending invoice for request from peer %x: %v",
			msg.Peer, err)
	}
}

// rejection is a failure the requester is told about, in words that give
// nothing away, with the real cause for the log.
type rejection struct {
	message string
	cause   error
}

func (r *rejection) Error() string { return r.cause.Error() }

func (r *rejection) Unwrap() error { return r.cause }

func reject(message string, cause error) error {
	return &rejection{message: message, cause: cause}
}

// answer produces the encoded invoice for a request, or an error: a
// rejection to reply to, or anything else to ignore.
func (s *Server) answer(ctx context.Context,
	msg *onionmsg.Inbound) ([]byte, error) {

	ir := msg.InvoiceRequest
	raw := msg.Records[uint64(onionmsg.TypeInvoiceRequest)]
	key := sha256.Sum256(raw)

	// The reader checks for our chain, including the signature.
	err := bolt12.ValidateInvoiceRequestRead(ir, s.cfg.ChainHash, nil)
	if err != nil {
		if errors.Is(err, bolt12.ErrUnsupportedChain) {
			return nil, reject("wrong chain", err)
		}

		return nil, reject("invalid invoice request", err)
	}

	// The same request again gets the same invoice, while it lasts.
	if cached, err := s.recent.Get(key); err == nil &&
		s.cfg.Clock.Now().Before(cached.expiry) {

		return cached.encoded, nil
	}

	// The offer it is for, and whether it came the right way.
	record, _, err := s.cfg.Manager.OfferForRequest(ir, msg.PathID)
	switch {
	case err == nil:
	case errors.Is(err, offers.ErrNotOurOffer),
		errors.Is(err, offers.ErrWrongPath):

		// The spec says to ignore these.
		return nil, err

	case errors.Is(err, offers.ErrOfferDisabled),
		errors.Is(err, offers.ErrOfferExpired):

		return nil, reject("offer is no longer available", err)

	default:
		return nil, reject("offer is not available", err)
	}

	amount, err := invoiceAmount(ir)
	if err != nil {
		return nil, reject(err.Error(), err)
	}
	quantity := uint64(ir.InvreqQuantity.ValOpt().UnwrapOr(0))

	created, err := s.cfg.AddInvoice(
		ctx, amount, record.Description, s.cfg.InvoiceExpiry,
	)
	if err != nil {
		return nil, reject("cannot issue an invoice right now",
			fmt.Errorf("add invoice: %w", err))
	}
	inv, err := s.buildInvoice(ir, created)
	if err != nil {
		return nil, reject("cannot issue an invoice right now",
			fmt.Errorf("build invoice: %w", err))
	}
	encoded, err := inv.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode invoice: %w", err)
	}
	str, err := bolt12.Encode(offers.InvoiceHRP, encoded)
	if err != nil {
		return nil, fmt.Errorf("bech32 encode: %w", err)
	}

	var payer *btcec.PublicKey
	ir.InvreqPayerID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType88, *btcec.PublicKey]) {
			payer = r.Val
		},
	)
	err = s.cfg.Invoices.Put(&offers.IssuedInvoice{
		PaymentHash: created.PaymentHash,
		OfferID:     record.ID,
		PayerID:     payer,
		AmountMsat:  amount,
		Quantity:    quantity,
		CreatedAt:   created.CreatedAt.Truncate(time.Second).UTC(),
		Bolt12:      str,
	})
	if err != nil {
		log.Errorf("Recording invoice %x for offer %v: %v",
			created.PaymentHash, record.ID, err)
	}
	if err := s.cfg.Manager.CountInvoice(record.ID); err != nil {
		log.Errorf("Counting invoice for offer %v: %v", record.ID, err)
	}
	_, _ = s.recent.Put(key, &recentInvoice{
		encoded: encoded,
		expiry:  created.CreatedAt.Add(created.Expiry),
	})

	log.Infof("Issued invoice %x for %d msat on offer %v (%q) to payer %x",
		created.PaymentHash, amount, record.ID, record.Description,
		pubKeyBytes(payer))

	return encoded, nil
}

// invoiceAmount is what the request asks to be invoiced: its own amount,
// else the offer's amount times the quantity.
func invoiceAmount(ir *bolt12.InvoiceRequest) (uint64, error) {
	if amount := uint64(ir.InvreqAmount.ValOpt().UnwrapOr(0)); amount != 0 {
		return amount, nil
	}
	offerAmount := uint64(ir.OfferAmount.ValOpt().UnwrapOr(0))
	if offerAmount == 0 {
		return 0, ErrAmountRequired
	}
	quantity := uint64(ir.InvreqQuantity.ValOpt().UnwrapOr(1))
	if quantity == 0 {
		quantity = 1
	}
	if offerAmount > (1<<63)/quantity {
		return 0, errors.New("amount overflows")
	}

	return offerAmount * quantity, nil
}

// buildInvoice makes the signed BOLT 12 invoice for a request from the
// Lightning invoice created for it.
func (s *Server) buildInvoice(ir *bolt12.InvoiceRequest,
	created *CreatedInvoice) (*bolt12.Invoice, error) {

	if len(created.Paths) == 0 {
		return nil, errors.New("invoice has no blinded payment paths")
	}
	amount, err := invoiceAmount(ir)
	if err != nil {
		return nil, err
	}

	inv := bolt12.NewInvoiceFromRequest(ir)
	var (
		paths lnwire.BlindedPaths
		infos bolt12.BlindedPayInfos
	)
	for _, p := range created.Paths {
		path, info, err := convertPaymentPath(p)
		if err != nil {
			return nil, err
		}
		paths.Paths = append(paths.Paths, *path)
		infos.Infos = append(infos.Infos, *info)
	}
	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType160](paths),
	)
	inv.InvoiceBlindedPay = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType162](infos),
	)
	inv.InvoiceCreatedAt = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType164](
			bolt12.TUint64(created.CreatedAt.Unix()),
		),
	)
	inv.InvoiceRelativeExp = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType166](
			bolt12.TUint32(created.Expiry / time.Second),
		),
	)
	inv.InvoicePaymentHash = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType168](created.PaymentHash),
	)
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType170](bolt12.TUint64(amount)),
	)
	if created.Features != nil &&
		created.Features.HasFeature(lnwire.MPPOptional) {

		inv.InvoiceFeatures = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType174](
				*lnwire.NewRawFeatureVector(lnwire.MPPOptional),
			),
		)
	}
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](s.cfg.NodeKey.PubKey),
	)

	root, err := bolt12.MerkleRoot(inv.AllRecords())
	if err != nil {
		return nil, fmt.Errorf("invoice merkle root: %w", err)
	}
	sig, err := s.cfg.Signer.SignMessageSchnorr(
		s.cfg.NodeKey.KeyLocator, root[:], false, nil,
		[]byte(invoiceSignatureTag),
	)
	if err != nil {
		return nil, fmt.Errorf("sign invoice: %w", err)
	}
	var sigBytes [64]byte
	copy(sigBytes[:], sig.Serialize())
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sigBytes),
	)

	// The checks a payer will make, before anything leaves.
	if err := bolt12.VerifyInvoice(inv); err != nil {
		return nil, fmt.Errorf("invoice signature: %w", err)
	}
	if err := bolt12.ValidateInvoiceWriteOnChain(
		inv, s.cfg.ChainHash,
	); err != nil {
		return nil, fmt.Errorf("invoice does not validate: %w", err)
	}
	if err := bolt12.ValidateInvoiceAgainstRequest(inv, ir); err != nil {
		return nil, fmt.Errorf("invoice does not answer the request: "+
			"%w", err)
	}

	return inv, nil
}

// convertPaymentPath turns a blinded payment path as the invoice registry
// builds it into the BOLT 12 pair of a blinded path and its payment info.
func convertPaymentPath(p *zpay32.BlindedPaymentPath) (*lnwire.BlindedPath,
	*bolt12.BlindedPayInfo, error) {

	if len(p.Hops) == 0 || p.FirstEphemeralBlindingPoint == nil {
		return nil, nil, errors.New("blinded payment path without hops")
	}
	// The first hop's blinded node id is the introduction node's real
	// id.
	intro, err := lnwire.NewPubkeyIntro(p.Hops[0].BlindedNodePub)
	if err != nil {
		return nil, nil, err
	}
	path := &lnwire.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    p.FirstEphemeralBlindingPoint,
	}
	for _, h := range p.Hops {
		path.Hops = append(path.Hops, lnwire.BlindedHop{
			BlindedNodeID: h.BlindedNodePub,
			EncryptedData: h.CipherText,
		})
	}
	info := &bolt12.BlindedPayInfo{
		FeeBaseMsat:               p.FeeBaseMsat,
		FeeProportionalMillionths: p.FeeRate,
		CltvExpiryDelta:           p.CltvExpiryDelta,
		HtlcMinimumMsat:           p.HTLCMinMsat,
		HtlcMaximumMsat:           p.HTLCMaxMsat,
	}
	if p.Features != nil && p.Features.RawFeatureVector != nil {
		info.Features = *p.Features.RawFeatureVector.Clone()
	} else {
		info.Features = *lnwire.NewRawFeatureVector()
	}

	return path, info, nil
}

// sendError sends an invoice_error back over the reply path, if there is
// one.
func (s *Server) sendError(ctx context.Context, msg *onionmsg.Inbound,
	text string) {

	if msg.ReplyPath == nil {
		return
	}
	encoded, err := (&onionmsg.InvoiceError{Message: text}).Encode()
	if err != nil {
		log.Errorf("Encoding invoice error: %v", err)

		return
	}
	err = s.cfg.Messenger.Send(ctx, onionmsg.Destination{
		Path: msg.ReplyPath,
	}, []*lnwire.FinalHopTLV{{
		TLVType: onionmsg.TypeInvoiceError, Value: encoded,
	}}, nil)
	if err != nil {
		log.Debugf("Sending invoice error to peer %x: %v", msg.Peer,
			err)
	}
}

func pubKeyBytes(k *btcec.PublicKey) []byte {
	if k == nil {
		return nil
	}

	return k.SerializeCompressed()
}
