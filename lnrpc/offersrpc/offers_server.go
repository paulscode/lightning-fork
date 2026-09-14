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
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
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
	}
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

	record, created, err := s.cfg.Manager.CreateOffer(ctx, params)
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

	records, err := s.cfg.Manager.ListOffers(req.ActiveOnly)
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
	if err := s.cfg.Manager.DisableOffer(id); err != nil {
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
	if err := s.cfg.Manager.EnableOffer(id); err != nil {
		return nil, rpcError(err)
	}

	return &EnableOfferResponse{}, nil
}

// DecodeBolt12 decodes any BOLT 12 string.
func (s *Server) DecodeBolt12(_ context.Context,
	req *DecodeBolt12Request) (*DecodeBolt12Response, error) {

	bolt12Str := strings.TrimSpace(req.Bolt12)
	decoded, err := s.cfg.Manager.DecodeBolt12(bolt12Str)
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
		resp.Invoice = &InvoiceInfo{
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
