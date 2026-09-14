//go:build offersrpc
// +build offersrpc

package offersrpc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offerpay"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/onionmsg"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/tlv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize this as the name of the
	// config file that we need.
	subServerName = "OffersRPC"
)

var (
	// macPermissions maps RPC calls to the permissions they require. The
	// "invoices" entity is reused rather than a new one, so a macaroon
	// baked before this sub-server existed still works: an offer is a
	// standing invitation to be invoiced.
	macPermissions = map[string][]bakery.Op{
		"/offersrpc.Offers/CreateOffer": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/offersrpc.Offers/ListOffers": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/offersrpc.Offers/DisableOffer": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/offersrpc.Offers/EnableOffer": {{
			Entity: "invoices",
			Action: "write",
		}},
		"/offersrpc.Offers/DecodeBolt12": {{
			Entity: "invoices",
			Action: "read",
		}},
		"/offersrpc.Offers/FetchInvoice": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/offersrpc.Offers/PayOffer": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/offersrpc.Offers/ListOfferInvoices": {{
			Entity: "invoices",
			Action: "read",
		}},
	}
)

const (
	// DefaultPayTimeout bounds a payment made by PayOffer when the
	// request sets no timeout.
	DefaultPayTimeout = 60 * time.Second

	// DefaultMaxParts is how many parts a payment may be split into when
	// the request does not say.
	DefaultMaxParts = 16
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	OffersServer
}

// Server is a sub-server of the main RPC server: it serves the offers API.
type Server struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedOffersServer

	cfg *Config
}

// A compile time check to ensure that Server fully implements the
// OffersServer gRPC service.
var _ OffersServer = (*Server)(nil)

// New returns a new instance of the offersrpc Offers sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	server := &Server{
		cfg: cfg,
	}

	return server, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	if atomic.AddInt32(&s.started, 1) != 1 {
		return nil
	}

	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until
// this is called, each sub-server won't be able to have requests routed
// towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterOffersServer(grpcServer, r)

	log.Debugf("Offers RPC server successfully registered with root " +
		"gRPC server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterOffersHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Offers REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Offers REST server successfully registered with " +
		"root REST server")
	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for
// all methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.OffersServer = subServer
	return subServer, macPermissions, nil
}

// CreateOffer mints an offer for this node's chain.
func (s *Server) CreateOffer(ctx context.Context,
	req *CreateOfferRequest) (*CreateOfferResponse, error) {

	params := offers.CreateParams{
		Description: req.Description,
		AmountMsat:  req.AmountMsat,
		Issuer:      req.Issuer,
		QuantityMax: req.QuantityMax,
		QuantityAny: req.QuantityAny,
		Label:       req.Label,
		WithPaths:   req.WithPaths,
		NoPaths:     req.NoPaths,
	}
	if req.AbsoluteExpiry != 0 {
		params.AbsoluteExpiry = time.Unix(int64(req.AbsoluteExpiry), 0)
	}

	record, created, err := s.cfg.Deps.Manager.CreateOffer(ctx, params)
	if err != nil {
		return nil, rpcError(err)
	}
	offer, err := recordToRPC(record)
	if err != nil {
		return nil, err
	}

	return &CreateOfferResponse{Offer: offer, Created: created}, nil
}

// ListOffers lists the node's offers, newest first.
func (s *Server) ListOffers(_ context.Context,
	req *ListOffersRequest) (*ListOffersResponse, error) {

	records, err := s.cfg.Deps.Manager.ListOffers(req.ActiveOnly)
	if err != nil {
		return nil, err
	}
	resp := &ListOffersResponse{}
	for _, record := range records {
		offer, err := recordToRPC(record)
		if err != nil {
			return nil, err
		}
		resp.Offers = append(resp.Offers, offer)
	}

	return resp, nil
}

// DisableOffer stops serving an offer.
func (s *Server) DisableOffer(_ context.Context,
	req *DisableOfferRequest) (*DisableOfferResponse, error) {

	id, err := parseOfferID(req.OfferId)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Deps.Manager.DisableOffer(id); err != nil {
		return nil, rpcError(err)
	}

	return &DisableOfferResponse{}, nil
}

// EnableOffer resumes serving an offer.
func (s *Server) EnableOffer(_ context.Context,
	req *EnableOfferRequest) (*EnableOfferResponse, error) {

	id, err := parseOfferID(req.OfferId)
	if err != nil {
		return nil, err
	}
	if err := s.cfg.Deps.Manager.EnableOffer(id); err != nil {
		return nil, rpcError(err)
	}

	return &EnableOfferResponse{}, nil
}

// DecodeBolt12 decodes any BOLT 12 string.
func (s *Server) DecodeBolt12(_ context.Context,
	req *DecodeBolt12Request) (*DecodeBolt12Response, error) {

	bolt12Str := strings.TrimSpace(req.Bolt12)
	decoded, err := s.cfg.Deps.Manager.DecodeBolt12(bolt12Str)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	resp := &DecodeBolt12Response{
		ForThisChain: decoded.ForThisChain,
		Ours:         decoded.Ours,
	}
	for _, chain := range decoded.Chains {
		c := chain
		resp.Chains = append(resp.Chains, c[:])
	}
	if decoded.OfferID != nil {
		resp.OfferId = decoded.OfferID[:]
	}

	switch {
	case decoded.Offer != nil:
		resp.Type = "offer"
		resp.Offer = offerToRPC(decoded.Offer, bolt12Str)
		resp.Valid = decoded.ValidationError == nil
		if decoded.ValidationError != nil {
			resp.ValidationError = decoded.ValidationError.Error()
		}

	case decoded.InvoiceRequest != nil:
		resp.Type = "invoice_request"
		ir := decoded.InvoiceRequest
		resp.Offer = offerToRPC(&bolt12.Offer{
			OfferChains:         ir.OfferChains,
			OfferMetadata:       ir.OfferMetadata,
			OfferCurrency:       ir.OfferCurrency,
			OfferAmount:         ir.OfferAmount,
			OfferDescription:    ir.OfferDescription,
			OfferFeatures:       ir.OfferFeatures,
			OfferAbsoluteExpiry: ir.OfferAbsoluteExpiry,
			OfferPaths:          ir.OfferPaths,
			OfferIssuer:         ir.OfferIssuer,
			OfferQuantityMax:    ir.OfferQuantityMax,
			OfferIssuerID:       ir.OfferIssuerID,
		}, "")
		resp.InvoiceRequest = &InvoiceRequestInfo{
			PayerId:        pubKeyBytes(optPubKey(ir.InvreqPayerID)),
			AmountMsat:     optU64(ir.InvreqAmount),
			Quantity:       optU64(ir.InvreqQuantity),
			PayerNote:      string(optBlob(ir.InvreqPayerNote)),
			Metadata:       optBlob(ir.InvreqMetadata),
			SignatureValid: bolt12.VerifyInvoiceRequest(ir) == nil,
		}

	case decoded.Invoice != nil:
		resp.Type = "invoice"
		inv := decoded.Invoice
		resp.Offer = offerToRPC(&bolt12.Offer{
			OfferChains:         inv.OfferChains,
			OfferMetadata:       inv.OfferMetadata,
			OfferCurrency:       inv.OfferCurrency,
			OfferAmount:         inv.OfferAmount,
			OfferDescription:    inv.OfferDescription,
			OfferFeatures:       inv.OfferFeatures,
			OfferAbsoluteExpiry: inv.OfferAbsoluteExpiry,
			OfferPaths:          inv.OfferPaths,
			OfferIssuer:         inv.OfferIssuer,
			OfferQuantityMax:    inv.OfferQuantityMax,
			OfferIssuerID:       inv.OfferIssuerID,
		}, "")
		resp.InvoiceRequest = &InvoiceRequestInfo{
			PayerId:    pubKeyBytes(optPubKey(inv.InvreqPayerID)),
			AmountMsat: optU64(inv.InvreqAmount),
			Quantity:   optU64(inv.InvreqQuantity),
			PayerNote:  string(optBlob(inv.InvreqPayerNote)),
			Metadata:   optBlob(inv.InvreqMetadata),
		}
		resp.Invoice = invoiceToRPC(inv)
	}

	return resp, nil
}

// parseOfferID reads a 32-byte offer id from a request.
func parseOfferID(b []byte) (offers.OfferID, error) {
	var id offers.OfferID
	if len(b) != len(id) {
		return id, status.Errorf(codes.InvalidArgument,
			"offer id must be %d bytes, got %d", len(id), len(b))
	}
	copy(id[:], b)

	return id, nil
}

// rpcError maps the offers package's errors to gRPC codes.
func rpcError(err error) error {
	switch {
	case errors.Is(err, offers.ErrOfferNotFound):
		return status.Error(codes.NotFound, err.Error())

	case errors.Is(err, offers.ErrDescriptionRequired),
		errors.Is(err, offers.ErrInvalidUTF8),
		errors.Is(err, offers.ErrControlCharacters),
		errors.Is(err, offers.ErrTextTooLong),
		errors.Is(err, offers.ErrExpiryInPast),
		errors.Is(err, offers.ErrPathsConflict),
		errors.Is(err, offers.ErrQuantityConflict):

		return status.Error(codes.InvalidArgument, err.Error())
	}

	if errors.Is(err, offers.ErrTooManyOffers) {
		return status.Error(codes.ResourceExhausted, err.Error())
	}

	return err
}

// recordToRPC converts a stored offer to its RPC form.
func recordToRPC(record *offers.Record) (*Offer, error) {
	offer, err := bolt12.DecodeOffer(record.Offer)
	if err != nil {
		return nil, fmt.Errorf("stored offer %v does not decode: %w",
			record.ID, err)
	}
	out := offerToRPC(offer, record.Bolt12)
	out.OfferId = record.ID[:]
	out.CreatedAt = uint64(record.CreatedAt.Unix())
	out.Active = record.Active
	out.InvoicesIssued = record.InvoicesIssued
	out.Label = record.Label

	return out, nil
}

// offerToRPC converts a decoded offer's fields to the RPC form; the fields a
// stored record adds are left zero.
func offerToRPC(o *bolt12.Offer, bolt12Str string) *Offer {
	out := &Offer{
		Bolt12:         bolt12Str,
		Description:    string(optBlob(o.OfferDescription)),
		AmountMsat:     optU64(o.OfferAmount),
		AbsoluteExpiry: optU64(o.OfferAbsoluteExpiry),
		IssuerId:       pubKeyBytes(optPubKey(o.OfferIssuerID)),
		Issuer:         string(optBlob(o.OfferIssuer)),
		QuantityMax:    optU64(o.OfferQuantityMax),
	}
	// offer_quantity_max present with zero is the spec's "any quantity".
	out.QuantityAny = o.OfferQuantityMax.IsSome() && out.QuantityMax == 0
	o.OfferPaths.WhenSome(
		func(r tlv.RecordT[tlv.TlvType16, lnwire.BlindedPaths]) {
			out.NumPaths = uint32(len(r.Val.Paths))
		},
	)
	o.OfferChains.WhenSome(
		func(r tlv.RecordT[tlv.TlvType2, bolt12.ChainsRecord]) {
			for _, c := range r.Val.Chains {
				chain := c
				out.Chains = append(out.Chains, chain[:])
			}
		},
	)
	if id, err := bolt12.OfferID(o); err == nil {
		out.OfferId = id[:]
	}

	return out
}

func optBlob[T tlv.TlvType](r tlv.OptionalRecordT[T, tlv.Blob]) []byte {
	var out []byte
	r.WhenSome(func(rec tlv.RecordT[T, tlv.Blob]) {
		out = []byte(rec.Val)
	})

	return out
}

func optU64[T tlv.TlvType](r tlv.OptionalRecordT[T, bolt12.TUint64]) uint64 {
	var out uint64
	r.WhenSome(func(rec tlv.RecordT[T, bolt12.TUint64]) {
		out = uint64(rec.Val)
	})

	return out
}

func optPubKey[T tlv.TlvType](
	r tlv.OptionalRecordT[T, *btcec.PublicKey]) *btcec.PublicKey {

	var out *btcec.PublicKey
	r.WhenSome(func(rec tlv.RecordT[T, *btcec.PublicKey]) {
		out = rec.Val
	})

	return out
}

func pubKeyBytes(k *btcec.PublicKey) []byte {
	if k == nil {
		return nil
	}

	return k.SerializeCompressed()
}

// FetchInvoice asks an offer's issuer for an invoice.
func (s *Server) FetchInvoice(ctx context.Context,
	req *FetchInvoiceRequest) (*FetchInvoiceResponse, error) {

	fetched, err := s.fetch(ctx, req.Offer, req.AmountMsat, req.Quantity,
		req.PayerNote, req.TimeoutSeconds)
	if err != nil {
		return nil, err
	}
	offerID, err := bolt12.InvoiceOfferID(fetched.Invoice)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &FetchInvoiceResponse{
		Bolt12:  fetched.Bolt12,
		Invoice: invoiceToRPC(fetched.Invoice),
		OfferId: offerID[:],
	}, nil
}

// fetch decodes an offer string and fetches an invoice for it.
func (s *Server) fetch(ctx context.Context, offerStr string, amount,
	quantity uint64, note string, timeoutSecs uint32) (*offerpay.Fetched,
	error) {

	if s.cfg.Deps.Client == nil {
		return nil, status.Error(codes.Unavailable,
			"onion messages are disabled")
	}
	decoded, err := s.cfg.Deps.Manager.DecodeBolt12(offerStr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if decoded.Offer == nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"not an offer but a %s", decoded.HRP)
	}
	if !decoded.ForThisChain {
		return nil, status.Error(codes.InvalidArgument,
			"the offer is not for this chain")
	}
	if timeoutSecs != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(
			ctx, time.Duration(timeoutSecs)*time.Second,
		)
		defer cancel()
	}
	fetched, err := s.cfg.Deps.Client.FetchInvoice(ctx, offerpay.FetchParams{
		Offer:      decoded.Offer,
		AmountMsat: amount,
		Quantity:   quantity,
		PayerNote:  note,
	})
	if err != nil {
		return nil, fetchError(err)
	}

	return fetched, nil
}

// fetchError maps a fetch failure to a gRPC code.
func fetchError(err error) error {
	switch {
	case errors.Is(err, offerpay.ErrAmountRequired),
		errors.Is(err, offerpay.ErrAmountBelowOffer),
		errors.Is(err, offerpay.ErrQuantityNotOffered),
		errors.Is(err, offerpay.ErrQuantityRequired),
		errors.Is(err, bolt12.ErrUnsupportedChain),
		errors.Is(err, bolt12.ErrOfferExpired):

		return status.Error(codes.InvalidArgument, err.Error())

	case errors.Is(err, offerpay.ErrTimeout),
		errors.Is(err, context.DeadlineExceeded):

		return status.Error(codes.DeadlineExceeded, err.Error())

	case errors.Is(err, offerpay.ErrInvoiceError):
		return status.Error(codes.Aborted, err.Error())

	case errors.Is(err, offerpay.ErrOfferUnreachable),
		errors.Is(err, onionmsg.ErrUnreachable):

		return status.Error(codes.Unavailable, err.Error())
	}

	return status.Error(codes.Unknown, err.Error())
}

// PayOffer fetches an invoice for an offer and pays it, or pays an invoice
// fetched earlier.
func (s *Server) PayOffer(ctx context.Context,
	req *PayOfferRequest) (*PayOfferResponse, error) {

	var (
		inv    *bolt12.Invoice
		bolt12 string
	)
	switch {
	case req.Offer != "" && req.Invoice != "":
		return nil, status.Error(codes.InvalidArgument,
			"offer and invoice exclude each other")

	case req.Offer != "":
		fetched, err := s.fetch(ctx, req.Offer, req.AmountMsat,
			req.Quantity, req.PayerNote, req.TimeoutSeconds)
		if err != nil {
			return nil, err
		}
		inv, bolt12 = fetched.Invoice, fetched.Bolt12

	case req.Invoice != "":
		decoded, err := s.cfg.Deps.Manager.DecodeBolt12(req.Invoice)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument,
				err.Error())
		}
		if decoded.Invoice == nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"not an invoice but a %s", decoded.HRP)
		}
		if !decoded.ForThisChain {
			return nil, status.Error(codes.InvalidArgument,
				"the invoice is not for this chain")
		}
		if err := offerpay.CheckInvoice(
			decoded.Invoice, s.cfg.Deps.Manager.ChainHash(),
			time.Now(),
		); err != nil {
			return nil, status.Error(codes.InvalidArgument,
				err.Error())
		}
		inv, bolt12 = decoded.Invoice, strings.TrimSpace(req.Invoice)

	default:
		return nil, status.Error(codes.InvalidArgument,
			"an offer or an invoice is required")
	}
	if s.cfg.Deps.PayInvoice == nil {
		return nil, status.Error(codes.Unavailable,
			"payments are not available")
	}

	amount := uint64(inv.InvoiceAmount.ValOpt().UnwrapOr(0))
	feeLimit := req.FeeLimitMsat
	if feeLimit == 0 {
		// The wallet's rule, the one lncli payinvoice applies too.
		feeLimit = uint64(lnwallet.DefaultRoutingFeeLimitForAmount(
			lnwire.MilliSatoshi(amount),
		))
	}
	timeout := DefaultPayTimeout
	if req.TimeoutSeconds != 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	maxParts := req.MaxParts
	if maxParts == 0 {
		maxParts = DefaultMaxParts
	}
	intent, err := offerpay.PaymentIntent(ctx, inv, offerpay.PaymentParams{
		FeeLimitMsat: feeLimit,
		Timeout:      timeout,
		MaxParts:     maxParts,
	}, offerpay.IntentHooks{
		NodeKey:            s.cfg.Deps.NodeKey,
		ResolveIntro:       s.cfg.Deps.ResolveIntro,
		PeerOverChannel:    s.cfg.Deps.PeerOverChannel,
		DecryptBlindedData: s.cfg.Deps.DecryptBlindedData,
		NextPathKey:        s.cfg.Deps.NextPathKey,
	})
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	hash := intent.Identifier()
	preimage, rt, err := s.cfg.Deps.PayInvoice(ctx, intent)
	switch {
	case err == nil:

	case errors.Is(err, paymentsdb.ErrAlreadyPaid):
		// Paying the same invoice again is the retry the caller is
		// told to make: it returns what the first payment got.
		if s.cfg.Deps.LookupPayment == nil {
			return nil, status.Error(codes.AlreadyExists,
				"invoice is already paid")
		}
		got, settled, _, lookupErr := s.cfg.Deps.LookupPayment(
			ctx, hash,
		)
		if lookupErr != nil || !settled {
			return nil, status.Errorf(codes.AlreadyExists,
				"invoice is already paid; payment hash %x",
				hash[:])
		}
		preimage = got

	case errors.Is(err, paymentsdb.ErrPaymentInFlight):
		return nil, status.Errorf(codes.AlreadyExists,
			"a payment for this invoice is in flight; track "+
				"payment hash %x", hash[:])

	default:
		// A failed attempt may still settle: the caller is given
		// the hash to track it and the invoice to retry with, since
		// fetching again would make a second invoice.
		return nil, status.Errorf(codes.Aborted,
			"payment failed: %v; payment hash %x; retry with "+
				"this invoice, not the offer: %s", err, hash[:],
			bolt12)
	}
	var fee uint64
	if rt != nil {
		fee = uint64(rt.TotalFees())
	}

	return &PayOfferResponse{
		Bolt12:          bolt12,
		PaymentHash:     hash[:],
		PaymentPreimage: preimage[:],
		AmountMsat:      amount,
		FeeMsat:         fee,
	}, nil
}

// ListOfferInvoices lists the invoices issued for this node's offers.
func (s *Server) ListOfferInvoices(ctx context.Context,
	req *ListOfferInvoicesRequest) (*ListOfferInvoicesResponse, error) {

	if s.cfg.Deps.Invoices == nil {
		return &ListOfferInvoicesResponse{}, nil
	}
	var (
		issued []*offers.IssuedInvoice
		err    error
	)
	if len(req.OfferId) > 0 {
		id, err := parseOfferID(req.OfferId)
		if err != nil {
			return nil, err
		}
		issued, err = s.cfg.Deps.Invoices.ListForOffer(id)
		if err != nil {
			return nil, err
		}
	} else {
		issued, err = s.cfg.Deps.Invoices.List()
		if err != nil {
			return nil, err
		}
	}

	resp := &ListOfferInvoicesResponse{}
	for _, inv := range issued {
		out := &OfferInvoice{
			PaymentHash: inv.PaymentHash[:],
			OfferId:     inv.OfferID[:],
			PayerId:     pubKeyBytes(inv.PayerID),
			AmountMsat:  inv.AmountMsat,
			Quantity:    inv.Quantity,
			CreatedAt:   uint64(inv.CreatedAt.Unix()),
			Bolt12:      inv.Bolt12,
			State:       "UNKNOWN",
		}
		if s.cfg.Deps.LookupInvoice != nil {
			registry, err := s.cfg.Deps.LookupInvoice(
				ctx, inv.PaymentHash,
			)
			if err == nil {
				out.State = strings.ToUpper(
					registry.State.String(),
				)
				out.AmountPaidMsat = uint64(registry.AmtPaid)
			}
		}
		resp.Invoices = append(resp.Invoices, out)
	}

	return resp, nil
}

// invoiceToRPC converts an invoice's own fields.
func invoiceToRPC(inv *bolt12.Invoice) *InvoiceInfo {
	var hash []byte
	inv.InvoicePaymentHash.WhenSome(
		func(r tlv.RecordT[tlv.TlvType168, [32]byte]) {
			h := r.Val
			hash = h[:]
		},
	)
	numPaths := 0
	inv.InvoicePaths.WhenSome(
		func(r tlv.RecordT[tlv.TlvType160, lnwire.BlindedPaths]) {
			numPaths = len(r.Val.Paths)
		},
	)
	var relExp uint32
	inv.InvoiceRelativeExp.WhenSome(
		func(r tlv.RecordT[tlv.TlvType166, bolt12.TUint32]) {
			relExp = uint32(r.Val)
		},
	)

	return &InvoiceInfo{
		PaymentHash:    hash,
		AmountMsat:     optU64(inv.InvoiceAmount),
		NodeId:         pubKeyBytes(optPubKey(inv.InvoiceNodeID)),
		CreatedAt:      optU64(inv.InvoiceCreatedAt),
		RelativeExpiry: relExp,
		NumPaths:       uint32(numPaths),
		PayerId:        pubKeyBytes(optPubKey(inv.InvreqPayerID)),
		SignatureValid: bolt12.VerifyInvoice(inv) == nil,
	}
}
