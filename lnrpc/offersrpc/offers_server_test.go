//go:build offersrpc
// +build offersrpc

package offersrpc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offerpay"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/onionmsg"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testChain = [32]byte{0xb2, 0xb2, 0xb2, 1}

type testServer struct {
	*Server
	issuer *btcec.PrivateKey
	clock  *clock.TestClock
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "offers")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	store, err := offers.NewKVStore(backend)
	require.NoError(t, err)
	invStore, err := offers.NewInvoiceStore(backend)
	require.NoError(t, err)

	issuer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	testClock := clock.NewTestClock(time.Unix(1_800_000_000, 0))
	manager, err := offers.NewManager(offers.Config{
		ChainHash: testChain,
		Secret:    [32]byte{0x5e, 0xc7},
		IssuerKey: keychain.KeyDescriptor{
			PubKey: issuer.PubKey(),
		},
		Store:         store,
		Clock:         testClock,
		NodeReachable: func() bool { return true },
		MaxOffers:     4,
	})
	require.NoError(t, err)

	server, perms, err := New(&Config{Deps: &Deps{
		Manager: manager, Invoices: invStore,
	}})
	require.NoError(t, err)
	require.Len(t, perms, 8, "every RPC needs a permission")
	require.NoError(t, server.Start())
	t.Cleanup(func() { require.NoError(t, server.Stop()) })

	return &testServer{Server: server, issuer: issuer, clock: testClock}
}

func fakeBlindedPath(t *testing.T) lnwire.BlindedPath {
	t.Helper()

	intro, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	blinding, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hop, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	node, err := lnwire.NewPubkeyIntro(intro.PubKey())
	require.NoError(t, err)

	return lnwire.BlindedPath{
		IntroductionNode: node,
		BlindingPoint:    blinding.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: hop.PubKey(),
			EncryptedData: []byte{1, 2, 3, 4},
		}},
	}
}

func requireCode(t *testing.T, err error, code codes.Code) {
	t.Helper()

	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "not a gRPC status: %v", err)
	require.Equal(t, code, st.Code(), "%v", err)
}

// TestCreateListDisableEnable walks an offer through its life over the RPC
// surface and checks the error codes on the way.
func TestCreateListDisableEnable(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()

	resp, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "OCEAN Payouts for bc1qexample",
		Label:       "pool",
	})
	require.NoError(t, err)
	require.True(t, resp.Created)
	offer := resp.Offer
	require.Len(t, offer.OfferId, 32)
	require.True(t, strings.HasPrefix(offer.Bolt12, "lno1"), offer.Bolt12)
	require.Equal(t, "OCEAN Payouts for bc1qexample", offer.Description)
	require.Zero(t, offer.AmountMsat, "the pool picks the amount")
	require.Equal(t, [][]byte{testChain[:]}, offer.Chains)
	require.Equal(t, s.issuer.PubKey().SerializeCompressed(),
		offer.IssuerId)
	require.True(t, offer.Active)
	require.Equal(t, "pool", offer.Label)
	require.Equal(t, uint64(s.clock.Now().Unix()), offer.CreatedAt)
	require.Zero(t, offer.NumPaths, "reachable node, no paths")
	require.False(t, offer.QuantityAny)

	// Minting the same offer again returns it, not a new one.
	again, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "OCEAN Payouts for bc1qexample",
	})
	require.NoError(t, err)
	require.False(t, again.Created)
	require.Equal(t, offer.OfferId, again.Offer.OfferId)
	require.Equal(t, offer.Bolt12, again.Offer.Bolt12)
	require.Equal(t, "pool", again.Offer.Label, "the stored record")

	// A second offer with an amount, expiry and quantity.
	s.clock.SetTime(s.clock.Now().Add(time.Second))
	expiry := s.clock.Now().Add(time.Hour)
	resp2, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description:    "coffee",
		AmountMsat:     50_000,
		AbsoluteExpiry: uint64(expiry.Unix()),
		Issuer:         "cafe",
		QuantityMax:    3,
	})
	require.NoError(t, err)
	require.True(t, resp2.Created)
	require.Equal(t, uint64(50_000), resp2.Offer.AmountMsat)
	require.Equal(t, uint64(expiry.Unix()), resp2.Offer.AbsoluteExpiry)
	require.Equal(t, "cafe", resp2.Offer.Issuer)
	require.Equal(t, uint64(3), resp2.Offer.QuantityMax)
	require.False(t, resp2.Offer.QuantityAny)
	require.NotEqual(t, offer.OfferId, resp2.Offer.OfferId)

	// Any quantity is reported as such.
	anyResp, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "any", QuantityAny: true,
	})
	require.NoError(t, err)
	require.True(t, anyResp.Offer.QuantityAny)
	require.Zero(t, anyResp.Offer.QuantityMax)
	_, err = s.DisableOffer(ctx, &DisableOfferRequest{
		OfferId: anyResp.Offer.OfferId,
	})
	require.NoError(t, err)

	list, err := s.ListOffers(ctx, &ListOffersRequest{ActiveOnly: true})
	require.NoError(t, err)
	require.Len(t, list.Offers, 2)
	require.Equal(t, resp2.Offer.OfferId, list.Offers[0].OfferId,
		"newest first")

	// Disable the first, list active only.
	_, err = s.DisableOffer(ctx, &DisableOfferRequest{
		OfferId: offer.OfferId,
	})
	require.NoError(t, err)
	list, err = s.ListOffers(ctx, &ListOffersRequest{ActiveOnly: true})
	require.NoError(t, err)
	require.Len(t, list.Offers, 1)
	require.Equal(t, resp2.Offer.OfferId, list.Offers[0].OfferId)
	list, err = s.ListOffers(ctx, &ListOffersRequest{})
	require.NoError(t, err)
	require.Len(t, list.Offers, 3, "disabled offers stay listed")
	require.False(t, list.Offers[2].Active)

	_, err = s.EnableOffer(ctx, &EnableOfferRequest{
		OfferId: offer.OfferId,
	})
	require.NoError(t, err)
	list, err = s.ListOffers(ctx, &ListOffersRequest{ActiveOnly: true})
	require.NoError(t, err)
	require.Len(t, list.Offers, 2)

	// Error codes.
	unknown := make([]byte, 32)
	unknown[0] = 0xff
	_, err = s.DisableOffer(ctx, &DisableOfferRequest{OfferId: unknown})
	requireCode(t, err, codes.NotFound)
	_, err = s.EnableOffer(ctx, &EnableOfferRequest{OfferId: unknown})
	requireCode(t, err, codes.NotFound)
	_, err = s.DisableOffer(ctx, &DisableOfferRequest{OfferId: []byte{1}})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.EnableOffer(ctx, &EnableOfferRequest{OfferId: nil})
	requireCode(t, err, codes.InvalidArgument)

	_, err = s.CreateOffer(ctx, &CreateOfferRequest{AmountMsat: 1000})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "bad\x00bytes",
	})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "bad\xffutf8",
	})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description:    "late",
		AbsoluteExpiry: uint64(s.clock.Now().Add(-time.Second).Unix()),
	})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description: strings.Repeat("d", offers.MaxDescriptionBytes+1),
	})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "x", Label: strings.Repeat("l", offers.MaxLabelBytes+1),
	})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "x", WithPaths: true, NoPaths: true,
	})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "x", QuantityMax: 2, QuantityAny: true,
	})
	requireCode(t, err, codes.InvalidArgument)

	// Three offers exist and the cap is four.
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{Description: "four"})
	require.NoError(t, err)
	_, err = s.CreateOffer(ctx, &CreateOfferRequest{Description: "five"})
	requireCode(t, err, codes.ResourceExhausted)
}

// TestDecodeBolt12 decodes an offer, a signed request for it and a signed
// invoice for the request, plus the things that must be refused.
func TestDecodeBolt12(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()

	created, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "OCEAN Payouts for bc1qexample",
	})
	require.NoError(t, err)

	// The offer itself, with whitespace around it.
	dec, err := s.DecodeBolt12(ctx, &DecodeBolt12Request{
		Bolt12: " " + created.Offer.Bolt12 + "\n",
	})
	require.NoError(t, err)
	require.Equal(t, "offer", dec.Type)
	require.True(t, dec.ForThisChain)
	require.True(t, dec.Ours)
	require.True(t, dec.Valid)
	require.Empty(t, dec.ValidationError)
	require.Empty(t, dec.Offer.Label, "no label on a decoded offer")
	require.Equal(t, [][]byte{testChain[:]}, dec.Chains)
	require.Equal(t, created.Offer.OfferId, dec.OfferId)
	require.NotNil(t, dec.Offer)
	require.Equal(t, created.Offer.Bolt12, dec.Offer.Bolt12)
	require.Equal(t, created.Offer.Description, dec.Offer.Description)
	require.Nil(t, dec.InvoiceRequest)
	require.Nil(t, dec.Invoice)

	// A signed request from a payer.
	_, data, err := bolt12.Decode(created.Offer.Bolt12)
	require.NoError(t, err)
	offer, err := bolt12.DecodeOffer(data)
	require.NoError(t, err)
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{1, 2, 3, 4}, testChain,
	)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(123_000)),
	)
	ir.InvreqPayerNote = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType89](tlv.Blob("thanks")),
	)
	sig, err := bolt12.SignInvoiceRequest(ir, payer)
	require.NoError(t, err)
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)
	irBytes, err := ir.Encode()
	require.NoError(t, err)
	irStr, err := bolt12.Encode(offers.InvoiceRequestHRP, irBytes)
	require.NoError(t, err)

	dec, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{Bolt12: irStr})
	require.NoError(t, err)
	require.Equal(t, "invoice_request", dec.Type)
	require.True(t, dec.ForThisChain)
	require.Equal(t, created.Offer.OfferId, dec.OfferId,
		"the request names our offer")
	require.NotNil(t, dec.InvoiceRequest)
	require.Equal(t, payer.PubKey().SerializeCompressed(),
		dec.InvoiceRequest.PayerId)
	require.Equal(t, uint64(123_000), dec.InvoiceRequest.AmountMsat)
	require.Equal(t, "thanks", dec.InvoiceRequest.PayerNote)
	require.Equal(t, []byte{1, 2, 3, 4}, dec.InvoiceRequest.Metadata)
	require.True(t, dec.InvoiceRequest.SignatureValid)
	require.Equal(t, created.Offer.Description, dec.Offer.Description)

	// Tamper with the signed bytes: the signature no longer verifies.
	ir.InvreqPayerNote = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType89](tlv.Blob("THANKS")),
	)
	irBytes, err = ir.Encode()
	require.NoError(t, err)
	irStr, err = bolt12.Encode(offers.InvoiceRequestHRP, irBytes)
	require.NoError(t, err)
	dec, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{Bolt12: irStr})
	require.NoError(t, err)
	require.False(t, dec.InvoiceRequest.SignatureValid)

	// An invoice for the request, signed by the issuer.
	ir.InvreqPayerNote = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType89](tlv.Blob("thanks")),
	)
	inv := bolt12.NewInvoiceFromRequest(ir)
	inv.InvoiceCreatedAt = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType164](
			bolt12.TUint64(s.clock.Now().Unix()),
		),
	)
	inv.InvoiceRelativeExp = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType166](bolt12.TUint32(3600)),
	)
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType170](bolt12.TUint64(123_000)),
	)
	inv.InvoicePaymentHash = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType168]([32]byte{7, 7, 7}),
	)
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](s.issuer.PubKey()),
	)
	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType160](lnwire.BlindedPaths{
			Paths: []lnwire.BlindedPath{fakeBlindedPath(t)},
		}),
	)
	inv.InvoiceBlindedPay = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType162](bolt12.BlindedPayInfos{
			Infos: []bolt12.BlindedPayInfo{{
				FeeBaseMsat:               1000,
				FeeProportionalMillionths: 100,
				CltvExpiryDelta:           80,
				HtlcMinimumMsat:           1,
				HtlcMaximumMsat:           1_000_000_000,
			}},
		}),
	)
	invSig, err := bolt12.SignInvoice(inv, s.issuer)
	require.NoError(t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](invSig),
	)
	invBytes, err := inv.Encode()
	require.NoError(t, err)
	invStr, err := bolt12.Encode(offers.InvoiceHRP, invBytes)
	require.NoError(t, err)

	dec, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{Bolt12: invStr})
	require.NoError(t, err)
	require.Equal(t, "invoice", dec.Type)
	require.True(t, dec.ForThisChain)
	require.Equal(t, created.Offer.OfferId, dec.OfferId)
	require.NotNil(t, dec.Invoice)
	require.Equal(t, []byte{7, 7, 7}, dec.Invoice.PaymentHash[:3])
	require.Equal(t, uint64(123_000), dec.Invoice.AmountMsat)
	require.Equal(t, s.issuer.PubKey().SerializeCompressed(),
		dec.Invoice.NodeId)
	require.Equal(t, uint64(s.clock.Now().Unix()), dec.Invoice.CreatedAt)
	require.Equal(t, uint32(3600), dec.Invoice.RelativeExpiry)
	require.Equal(t, uint32(1), dec.Invoice.NumPaths)
	require.Equal(t, payer.PubKey().SerializeCompressed(),
		dec.Invoice.PayerId)
	require.True(t, dec.Invoice.SignatureValid)
	require.NotNil(t, dec.InvoiceRequest)
	require.Equal(t, uint64(123_000), dec.InvoiceRequest.AmountMsat)

	// A chain-less offer is a Bitcoin offer, not ours.
	issuer2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	btcOffer := &bolt12.Offer{
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("btc")),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](issuer2.PubKey()),
		),
	}
	btcBytes, err := btcOffer.Encode()
	require.NoError(t, err)
	btcStr, err := bolt12.Encode(offers.OfferHRP, btcBytes)
	require.NoError(t, err)
	dec, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{Bolt12: btcStr})
	require.NoError(t, err)
	require.Equal(t, "offer", dec.Type)
	require.False(t, dec.ForThisChain)
	require.False(t, dec.Ours)
	require.False(t, dec.Valid)
	require.Contains(t, dec.ValidationError, "chain")
	require.Empty(t, dec.Chains)

	// A chain-less offer with a defect the validator reports before the
	// chain is still not for this chain.
	unknownEven := append(append([]byte{}, btcBytes...), 24, 1, 0)
	unknownStr, err := bolt12.Encode(offers.OfferHRP, unknownEven)
	require.NoError(t, err)
	dec, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{Bolt12: unknownStr})
	require.NoError(t, err)
	require.False(t, dec.ForThisChain)
	require.False(t, dec.Valid)
	require.NotContains(t, dec.ValidationError, "chain")
	require.Empty(t, dec.Offer.Label)

	// Our own offer, expired: ours, for this chain, not valid.
	expiring, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description:    "soon",
		AbsoluteExpiry: uint64(s.clock.Now().Add(time.Minute).Unix()),
	})
	require.NoError(t, err)
	s.clock.SetTime(s.clock.Now().Add(2 * time.Minute))
	dec, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{
		Bolt12: expiring.Offer.Bolt12,
	})
	require.NoError(t, err)
	require.True(t, dec.ForThisChain)
	require.True(t, dec.Ours)
	require.False(t, dec.Valid)
	require.Contains(t, dec.ValidationError, "expired")

	// Junk.
	for _, junk := range []string{"", "lno1", "hello", "lnbc1notbolt12"} {
		_, err = s.DecodeBolt12(ctx, &DecodeBolt12Request{Bolt12: junk})
		requireCode(t, err, codes.InvalidArgument)
	}
}

// answeringMessenger plays the issuer for the payer client: it answers
// every invoice request with a signed invoice from the issuer key.
type answeringMessenger struct {
	t       *testing.T
	issuer  *btcec.PrivateKey
	node    *btcec.PrivateKey
	invoice onionmsg.Handler
	errs    onionmsg.Handler
	pathIDs [][]byte
	sent    int
	refuse  string
}

func (a *answeringMessenger) BuildReplyPath(_ context.Context,
	pathID []byte) (*lnwire.BlindedPath, error) {

	a.pathIDs = append(a.pathIDs, pathID)
	intro, err := lnwire.NewPubkeyIntro(a.node.PubKey())
	require.NoError(a.t, err)

	return &lnwire.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    a.node.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: a.node.PubKey(), EncryptedData: pathID,
		}},
	}, nil
}

func (a *answeringMessenger) OnInvoice(h onionmsg.Handler)      { a.invoice = h }
func (a *answeringMessenger) OnInvoiceError(h onionmsg.Handler) { a.errs = h }

func (a *answeringMessenger) Send(_ context.Context, _ onionmsg.Destination,
	payload []*lnwire.FinalHopTLV, _ *lnwire.BlindedPath,
	_ ...onionmsg.SendOption) error {

	a.sent++
	require.Len(a.t, payload, 1)
	ir, err := bolt12.DecodeInvoiceRequest(payload[0].Value)
	require.NoError(a.t, err)
	pathID := a.pathIDs[len(a.pathIDs)-1]

	if a.refuse != "" {
		go a.errs(context.Background(), &onionmsg.Inbound{
			PathID:       pathID,
			InvoiceError: &onionmsg.InvoiceError{Message: a.refuse},
		})

		return nil
	}

	inv := bolt12.NewInvoiceFromRequest(ir)
	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType160](lnwire.BlindedPaths{
			Paths: []lnwire.BlindedPath{fakeBlindedPath(a.t)},
		}),
	)
	inv.InvoiceBlindedPay = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType162](bolt12.BlindedPayInfos{
			Infos: []bolt12.BlindedPayInfo{{
				CltvExpiryDelta: 80, HtlcMaximumMsat: 1_000_000_000,
			}},
		}),
	)
	inv.InvoiceCreatedAt = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType164](
			bolt12.TUint64(time.Now().Unix()),
		),
	)
	inv.InvoiceRelativeExp = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType166](bolt12.TUint32(3600)),
	)
	inv.InvoicePaymentHash = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType168]([32]byte{4, 4}),
	)
	amount := ir.InvreqAmount.ValOpt().UnwrapOr(0)
	if amount == 0 {
		amount = ir.OfferAmount.ValOpt().UnwrapOr(0)
	}
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType170](amount),
	)
	inv.InvoiceFeatures = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType174](
			*lnwire.NewRawFeatureVector(lnwire.MPPOptional),
		),
	)
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](a.issuer.PubKey()),
	)
	sig, err := bolt12.SignInvoice(inv, a.issuer)
	require.NoError(a.t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)
	raw, err := inv.Encode()
	require.NoError(a.t, err)
	go a.invoice(context.Background(), &onionmsg.Inbound{
		PathID:  pathID,
		Invoice: inv,
		Records: record.CustomSet{uint64(onionmsg.TypeInvoice): raw},
	})

	return nil
}

// TestFetchPayAndList covers the payer RPCs against a fake issuer and the
// issued-invoice listing.
func TestFetchPayAndList(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()
	node, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	msgr := &answeringMessenger{t: t, issuer: s.issuer, node: node}
	client, err := offerpay.New(offerpay.Config{
		Messenger: msgr, ChainHash: testChain, Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	s.cfg.Deps.Client = client
	var paid []*routing.LightningPayment
	s.cfg.Deps.PayInvoice = func(_ context.Context,
		p *routing.LightningPayment) ([32]byte, *route.Route, error) {

		paid = append(paid, p)

		return [32]byte{9, 9}, &route.Route{
			TotalAmount: p.Amount + 21,
			Hops: []*route.Hop{{
				AmtToForward: p.Amount, PubKeyBytes: p.Target,
			}},
		}, nil
	}

	// An offer of this node's own, used as the thing to fetch for: the
	// fake issuer signs with the same key.
	created, err := s.CreateOffer(ctx, &CreateOfferRequest{
		Description: "OCEAN Payouts for bc1qminer",
	})
	require.NoError(t, err)
	lno := created.Offer.Bolt12

	// Fetch.
	fetched, err := s.FetchInvoice(ctx, &FetchInvoiceRequest{
		Offer: lno, AmountMsat: 5000, PayerNote: "hi",
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(fetched.Bolt12, "lni1"))
	require.Equal(t, created.Offer.OfferId, fetched.OfferId)
	require.Equal(t, uint64(5000), fetched.Invoice.AmountMsat)
	require.True(t, fetched.Invoice.SignatureValid)
	require.Equal(t, []byte{4, 4}, fetched.Invoice.PaymentHash[:2])
	require.Equal(t, 1, msgr.sent)

	// The fetched invoice decodes.
	dec, err := s.DecodeBolt12(ctx, &DecodeBolt12Request{
		Bolt12: fetched.Bolt12,
	})
	require.NoError(t, err)
	require.Equal(t, "invoice", dec.Type)
	require.True(t, dec.ForThisChain)

	// Pay it, and pay the offer directly.
	payResp, err := s.PayOffer(ctx, &PayOfferRequest{
		Invoice: fetched.Bolt12,
	})
	require.NoError(t, err)
	require.Equal(t, fetched.Bolt12, payResp.Bolt12)
	require.Equal(t, uint64(5000), payResp.AmountMsat)
	require.Equal(t, uint64(21), payResp.FeeMsat)
	require.Equal(t, []byte{9, 9}, payResp.PaymentPreimage[:2])
	require.Len(t, paid, 1)
	require.Equal(t, lnwire.MilliSatoshi(5000), paid[0].Amount)
	require.Equal(t, lnwallet.DefaultRoutingFeeLimitForAmount(5000),
		paid[0].FeeLimit, "the wallet's default fee limit")
	require.Equal(t, uint32(DefaultMaxParts), paid[0].MaxParts)
	require.Equal(t, DefaultPayTimeout, paid[0].PayAttemptTimeout)

	payResp, err = s.PayOffer(ctx, &PayOfferRequest{
		Offer: lno, AmountMsat: 1_000_000, FeeLimitMsat: 500,
		MaxParts: 2, TimeoutSeconds: 30,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(1_000_000), payResp.AmountMsat)
	require.Len(t, paid, 2)
	require.Equal(t, lnwire.MilliSatoshi(500), paid[1].FeeLimit)
	require.Equal(t, uint32(2), paid[1].MaxParts)
	require.Equal(t, 30*time.Second, paid[1].PayAttemptTimeout)
	require.Equal(t, 2, msgr.sent)

	// Errors.
	_, err = s.PayOffer(ctx, &PayOfferRequest{})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.PayOffer(ctx, &PayOfferRequest{Offer: lno, Invoice: "x"})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.PayOffer(ctx, &PayOfferRequest{Invoice: lno})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.FetchInvoice(ctx, &FetchInvoiceRequest{Offer: fetched.Bolt12})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.FetchInvoice(ctx, &FetchInvoiceRequest{Offer: lno})
	requireCode(t, err, codes.InvalidArgument)
	_, err = s.FetchInvoice(ctx, &FetchInvoiceRequest{Offer: "junk"})
	requireCode(t, err, codes.InvalidArgument)
	msgr.refuse = "closed"
	_, err = s.FetchInvoice(ctx, &FetchInvoiceRequest{
		Offer: lno, AmountMsat: 1,
	})
	requireCode(t, err, codes.Aborted)
	msgr.refuse = ""
	s.cfg.Deps.PayInvoice = func(context.Context,
		*routing.LightningPayment) ([32]byte, *route.Route, error) {

		return [32]byte{}, nil, errors.New("no route")
	}
	_, err = s.PayOffer(ctx, &PayOfferRequest{Invoice: fetched.Bolt12})
	requireCode(t, err, codes.Aborted)
	require.ErrorContains(t, err, "payment hash 0404",
		"the hash to track is in the message")
	require.ErrorContains(t, err, fetched.Bolt12,
		"the invoice to retry with is in the message")

	// Paying an invoice that was already paid returns the first
	// payment's preimage; one in flight is reported as such.
	s.cfg.Deps.PayInvoice = func(context.Context,
		*routing.LightningPayment) ([32]byte, *route.Route, error) {

		return [32]byte{}, nil, paymentsdb.ErrAlreadyPaid
	}
	s.cfg.Deps.LookupPayment = func(_ context.Context,
		hash [32]byte) ([32]byte, bool, bool, error) {

		require.Equal(t, byte(4), hash[0])

		return [32]byte{8, 8}, true, false, nil
	}
	again, err := s.PayOffer(ctx, &PayOfferRequest{Invoice: fetched.Bolt12})
	require.NoError(t, err)
	require.Equal(t, []byte{8, 8}, again.PaymentPreimage[:2])
	require.Equal(t, fetched.Invoice.PaymentHash, again.PaymentHash)
	s.cfg.Deps.LookupPayment = func(context.Context,
		[32]byte) ([32]byte, bool, bool, error) {

		return [32]byte{}, false, true, nil
	}
	_, err = s.PayOffer(ctx, &PayOfferRequest{Invoice: fetched.Bolt12})
	requireCode(t, err, codes.AlreadyExists)
	s.cfg.Deps.PayInvoice = func(context.Context,
		*routing.LightningPayment) ([32]byte, *route.Route, error) {

		return [32]byte{}, nil, paymentsdb.ErrPaymentInFlight
	}
	_, err = s.PayOffer(ctx, &PayOfferRequest{Invoice: fetched.Bolt12})
	requireCode(t, err, codes.AlreadyExists)
	s.cfg.Deps.Client = nil
	_, err = s.FetchInvoice(ctx, &FetchInvoiceRequest{
		Offer: lno, AmountMsat: 1,
	})
	requireCode(t, err, codes.Unavailable)

	// Issued invoices, with the registry's state.
	var id offers.OfferID
	copy(id[:], created.Offer.OfferId)
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	require.NoError(t, s.cfg.Deps.Invoices.Put(&offers.IssuedInvoice{
		PaymentHash: [32]byte{1}, OfferID: id, PayerID: payer.PubKey(),
		AmountMsat: 5000, CreatedAt: time.Unix(1_800_000_000, 0).UTC(),
		Bolt12: "lni1x",
	}))
	require.NoError(t, s.cfg.Deps.Invoices.Put(&offers.IssuedInvoice{
		PaymentHash: [32]byte{2}, OfferID: offers.OfferID{7},
		PayerID: payer.PubKey(), AmountMsat: 1,
		CreatedAt: time.Unix(1_800_000_001, 0).UTC(), Bolt12: "lni1y",
	}))
	s.cfg.Deps.LookupInvoice = func(_ context.Context,
		hash [32]byte) (*invoices.Invoice, error) {

		if hash == ([32]byte{1}) {
			return &invoices.Invoice{
				State: invoices.ContractSettled, AmtPaid: 5000,
			}, nil
		}

		return nil, errors.New("not found")
	}
	list, err := s.ListOfferInvoices(ctx, &ListOfferInvoicesRequest{})
	require.NoError(t, err)
	require.Len(t, list.Invoices, 2)
	require.Equal(t, "SETTLED", list.Invoices[0].State)
	require.Equal(t, uint64(5000), list.Invoices[0].AmountPaidMsat)
	require.Equal(t, "UNKNOWN", list.Invoices[1].State)
	list, err = s.ListOfferInvoices(ctx, &ListOfferInvoicesRequest{
		OfferId: created.Offer.OfferId,
	})
	require.NoError(t, err)
	require.Len(t, list.Invoices, 1)
	require.Equal(t, "lni1x", list.Invoices[0].Bolt12)
	_, err = s.ListOfferInvoices(ctx, &ListOfferInvoicesRequest{
		OfferId: []byte{1},
	})
	requireCode(t, err, codes.InvalidArgument)
}
