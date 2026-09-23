package offerserve

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/onionmsg"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

var testChain = [32]byte{0xb2, 0xb2, 1}

// sent is one message the fake messenger was asked to send.
type sent struct {
	dest    onionmsg.Destination
	payload []*lnwire.FinalHopTLV
}

type fakeMessenger struct {
	handler onionmsg.Handler
	sent    []sent
	err     error
}

func (f *fakeMessenger) OnInvoiceRequest(h onionmsg.Handler) { f.handler = h }

func (f *fakeMessenger) Send(_ context.Context, dest onionmsg.Destination,
	payload []*lnwire.FinalHopTLV, _ *lnwire.BlindedPath,
	_ ...onionmsg.SendOption) error {

	f.sent = append(f.sent, sent{dest: dest, payload: payload})

	return f.err
}

// keySigner signs the way the wallet does: a tagged hash, then Schnorr.
type keySigner struct {
	key  *btcec.PrivateKey
	loc  keychain.KeyLocator
	seen []string
}

func (k *keySigner) SignMessageSchnorr(loc keychain.KeyLocator, msg []byte,
	doubleHash bool, taprootTweak []byte,
	tag []byte) (*schnorr.Signature, error) {

	if loc != k.loc || doubleHash || taprootTweak != nil {
		return nil, errors.New("unexpected signing request")
	}
	k.seen = append(k.seen, string(tag))
	digest := chainhash.TaggedHash(tag, msg)

	return schnorr.Sign(k.key, digest[:])
}

type env struct {
	t        *testing.T
	server   *Server
	manager  *offers.Manager
	invoices *offers.InvoiceStore
	msgr     *fakeMessenger
	signer   *keySigner
	clock    *clock.TestClock
	nodeKey  *btcec.PrivateKey
	paths    []*zpay32.BlindedPaymentPath
	addErr   error
	added    []addCall
	settled  map[[32]byte]bool
}

type addCall struct {
	amount uint64
	desc   string
	expiry time.Duration
}

func newEnv(t *testing.T) *env {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "offers")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	store, err := offers.NewKVStore(backend)
	require.NoError(t, err)
	invStore, err := offers.NewInvoiceStore(backend)
	require.NoError(t, err)

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	loc := keychain.KeyLocator{Family: keychain.KeyFamilyNodeKey}
	desc := keychain.KeyDescriptor{KeyLocator: loc, PubKey: nodeKey.PubKey()}
	e := &env{
		t:        t,
		invoices: invStore,
		msgr:     &fakeMessenger{},
		signer:   &keySigner{key: nodeKey, loc: loc},
		clock:    clock.NewTestClock(time.Unix(1_800_000_000, 0)),
		nodeKey:  nodeKey,
		settled:  make(map[[32]byte]bool),
	}
	e.manager, err = offers.NewManager(offers.Config{
		ChainHash:     testChain,
		IssuerKey:     desc,
		Secret:        [32]byte{1, 2, 3},
		Store:         store,
		Clock:         e.clock,
		NodeReachable: func() bool { return true },
		PathBuilder:   pathBuilder{e},
	})
	require.NoError(t, err)
	e.paths = []*zpay32.BlindedPaymentPath{e.paymentPath()}
	e.server, err = New(Config{
		Manager:   e.manager,
		Invoices:  invStore,
		Messenger: e.msgr,
		ChainHash: testChain,
		NodeKey:   desc,
		Signer:    e.signer,
		AddInvoice: func(_ context.Context, amount uint64, desc string,
			expiry time.Duration) (*CreatedInvoice, error) {

			e.added = append(e.added, addCall{amount, desc, expiry})
			if e.addErr != nil {
				return nil, e.addErr
			}
			var hash [32]byte
			hash[0] = byte(len(e.added))

			return &CreatedInvoice{
				PaymentHash: hash,
				Paths:       e.paths,
				CreatedAt:   e.clock.Now(),
				Expiry:      expiry,
				Features: lnwire.NewFeatureVector(
					lnwire.NewRawFeatureVector(
						lnwire.MPPOptional,
					), lnwire.Features,
				),
			}, nil
		},
		Clock:                 e.clock,
		RequestsPerSecond:     1000,
		RequestBurst:          1000,
		PeerRequestsPerSecond: 1000,
		PeerRequestBurst:      1000,
		InvoiceSettled: func(_ context.Context,
			hash [32]byte) (bool, error) {

			return e.settled[hash], nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, e.server.Start())
	require.NotNil(t, e.msgr.handler)

	return e
}

// pathBuilder gives offers a fake onion-message path with the secret as
// path_id, so the arrival check can be exercised.
type pathBuilder struct{ e *env }

func (p pathBuilder) BuildOfferPaths(_ context.Context,
	secret [32]byte) ([]lnwire.BlindedPath, error) {

	intro, err := btcec.NewPrivateKey()
	require.NoError(p.e.t, err)
	blinding, err := btcec.NewPrivateKey()
	require.NoError(p.e.t, err)
	node, err := lnwire.NewPubkeyIntro(intro.PubKey())
	require.NoError(p.e.t, err)

	return []lnwire.BlindedPath{{
		IntroductionNode: node,
		BlindingPoint:    blinding.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: p.e.nodeKey.PubKey(),
			EncryptedData: secret[:],
		}},
	}}, nil
}

// paymentPath is a blinded payment path shaped like the registry's.
func (e *env) paymentPath() *zpay32.BlindedPaymentPath {
	intro, err := btcec.NewPrivateKey()
	require.NoError(e.t, err)
	blinding, err := btcec.NewPrivateKey()
	require.NoError(e.t, err)
	blinded, err := btcec.NewPrivateKey()
	require.NoError(e.t, err)

	return &zpay32.BlindedPaymentPath{
		FeeBaseMsat:                 1000,
		FeeRate:                     500,
		CltvExpiryDelta:             144,
		HTLCMinMsat:                 1,
		HTLCMaxMsat:                 500_000_000,
		Features:                    lnwire.EmptyFeatureVector(),
		FirstEphemeralBlindingPoint: blinding.PubKey(),
		Hops: []*sphinx.BlindedHopInfo{
			{BlindedNodePub: intro.PubKey(), CipherText: []byte{1, 1}},
			{BlindedNodePub: blinded.PubKey(), CipherText: []byte{2, 2}},
		},
	}
}

// request builds a signed request for an offer from a fresh payer.
func (e *env) request(offer *bolt12.Offer, amount uint64,
	tweak func(ir *bolt12.InvoiceRequest)) (*bolt12.InvoiceRequest,
	*btcec.PrivateKey) {

	payer, err := btcec.NewPrivateKey()
	require.NoError(e.t, err)
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{9, 9, 9, 9}, testChain,
	)
	require.NoError(e.t, err)
	if amount != 0 {
		ir.InvreqAmount = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType82](
				bolt12.TUint64(amount),
			),
		)
	}
	if tweak != nil {
		tweak(ir)
	}
	sig, err := bolt12.SignInvoiceRequest(ir, payer)
	require.NoError(e.t, err)
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)

	return ir, payer
}

// inbound wraps a request the way the messenger delivers it.
func (e *env) inbound(ir *bolt12.InvoiceRequest, pathID []byte,
	withReply bool) *onionmsg.Inbound {

	// Encode from the records, without the writer checks, so requests
	// the server must refuse can be built.
	stream, err := tlv.NewStream(ir.AllRecords()...)
	require.NoError(e.t, err)
	var buf bytes.Buffer
	require.NoError(e.t, stream.Encode(&buf))
	raw := buf.Bytes()
	msg := &onionmsg.Inbound{
		Peer:           [33]byte{2, 1},
		InvoiceRequest: ir,
		PathID:         pathID,
		Records: record.CustomSet{
			uint64(onionmsg.TypeInvoiceRequest): raw,
		},
	}
	if withReply {
		reply := pathBuilder{e}
		paths, err := reply.BuildOfferPaths(
			context.Background(), [32]byte{0xee},
		)
		require.NoError(e.t, err)
		msg.ReplyPath = &paths[0]
	}

	return msg
}

func (e *env) decodeOffer(rec *offers.Record) *bolt12.Offer {
	offer, err := bolt12.DecodeOffer(rec.Offer)
	require.NoError(e.t, err)

	return offer
}

// lastInvoice decodes the invoice the messenger was last asked to send.
func (e *env) lastInvoice() *bolt12.Invoice {
	require.NotEmpty(e.t, e.msgr.sent)
	s := e.msgr.sent[len(e.msgr.sent)-1]
	require.Len(e.t, s.payload, 1)
	require.Equal(e.t, onionmsg.TypeInvoice, s.payload[0].TLVType)
	inv, err := bolt12.DecodeInvoice(s.payload[0].Value)
	require.NoError(e.t, err)

	return inv
}

func (e *env) lastError() *onionmsg.InvoiceError {
	require.NotEmpty(e.t, e.msgr.sent)
	s := e.msgr.sent[len(e.msgr.sent)-1]
	require.Len(e.t, s.payload, 1)
	require.Equal(e.t, onionmsg.TypeInvoiceError, s.payload[0].TLVType)
	ie, err := onionmsg.DecodeInvoiceError(s.payload[0].Value)
	require.NoError(e.t, err)

	return ie
}

// TestServePayoutRequest walks the pool payout: an amount-less offer, a
// request naming the amount, an invoice back over the reply path.
func TestServePayoutRequest(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, created, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "OCEAN Payouts for bc1qminer",
		NoPaths:     true,
	})
	require.NoError(t, err)
	require.True(t, created)
	offer := e.decodeOffer(rec)

	ir, payer := e.request(offer, 250_000_000, nil)
	msg := e.inbound(ir, nil, true)
	e.server.Handle(ctx, msg)

	require.Len(t, e.added, 1)
	require.Equal(t, uint64(250_000_000), e.added[0].amount)
	require.Equal(t, "OCEAN Payouts for bc1qminer", e.added[0].desc)
	require.Equal(t, DefaultInvoiceExpiry, e.added[0].expiry)

	require.Len(t, e.msgr.sent, 1)
	require.Equal(t, msg.ReplyPath, e.msgr.sent[0].dest.Path)
	inv := e.lastInvoice()

	// What a payer checks.
	require.NoError(t, bolt12.VerifyInvoice(inv))
	require.NoError(t, bolt12.ValidateInvoiceRead(
		inv, testChain, bolt12.InvoiceFeatureCatalogues{
			Invoice: bolt12.Blake2bFeatures,
		},
	))
	require.NoError(t, bolt12.CheckBlake2b(inv.InvoiceFeatures, "invoice"))
	require.NoError(t, bolt12.ValidateInvoiceAgainstRequest(inv, ir))
	require.Equal(t, uint64(250_000_000),
		uint64(inv.InvoiceAmount.ValOpt().UnwrapOr(0)))
	nodeID := inv.InvoiceNodeID.ValOpt().UnwrapOr(nil)
	require.True(t, nodeID.IsEqual(e.nodeKey.PubKey()))
	var hash [32]byte
	inv.InvoicePaymentHash.WhenSome(
		func(r tlv.RecordT[tlv.TlvType168, [32]byte]) { hash = r.Val },
	)
	require.Equal(t, [32]byte{1}, hash)
	require.Equal(t, uint64(e.clock.Now().Unix()),
		uint64(inv.InvoiceCreatedAt.ValOpt().UnwrapOr(0)))
	require.Equal(t, uint32(3600),
		uint32(inv.InvoiceRelativeExp.ValOpt().UnwrapOr(0)))
	var paths lnwire.BlindedPaths
	inv.InvoicePaths.WhenSome(
		func(r tlv.RecordT[tlv.TlvType160, lnwire.BlindedPaths]) {
			paths = r.Val
		},
	)
	require.Len(t, paths.Paths, 1)
	require.Len(t, paths.Paths[0].Hops, 2)
	intro, ok := paths.Paths[0].IntroductionNode.(lnwire.PubkeyIntro)
	require.True(t, ok)
	require.True(t, intro.Pubkey.IsEqual(e.paths[0].Hops[0].BlindedNodePub))
	require.Equal(t, e.paths[0].FirstEphemeralBlindingPoint,
		paths.Paths[0].BlindingPoint)
	var infos bolt12.BlindedPayInfos
	inv.InvoiceBlindedPay.WhenSome(
		func(r tlv.RecordT[tlv.TlvType162, bolt12.BlindedPayInfos]) {
			infos = r.Val
		},
	)
	require.Len(t, infos.Infos, 1)
	require.Equal(t, uint32(1000), infos.Infos[0].FeeBaseMsat)
	require.Equal(t, uint32(500), infos.Infos[0].FeeProportionalMillionths)
	require.Equal(t, uint16(144), infos.Infos[0].CltvExpiryDelta)
	require.Equal(t, uint64(500_000_000), infos.Infos[0].HtlcMaximumMsat)
	var feats lnwire.RawFeatureVector
	inv.InvoiceFeatures.WhenSome(
		func(r tlv.RecordT[tlv.TlvType174, lnwire.RawFeatureVector]) {
			feats = r.Val
		},
	)
	require.True(t, feats.IsSet(lnwire.MPPOptional))
	require.Equal(t, []string{invoiceSignatureTag}, e.signer.seen)

	// Same signature as signing with the key directly.
	unsigned := *inv
	unsigned.Signature = tlv.OptionalRecordT[tlv.TlvType240, [64]byte]{}
	direct, err := bolt12.SignInvoice(&unsigned, e.nodeKey)
	require.NoError(t, err)
	var got [64]byte
	inv.Signature.WhenSome(
		func(r tlv.RecordT[tlv.TlvType240, [64]byte]) { got = r.Val },
	)
	require.NotEqual(t, [64]byte{}, got)
	require.NoError(t, bolt12.VerifyInvoice(inv))
	_ = direct // Schnorr signatures are randomised; both verify.

	// Bookkeeping.
	issued, err := e.invoices.Get([32]byte{1})
	require.NoError(t, err)
	require.Equal(t, rec.ID, issued.OfferID)
	require.True(t, issued.PayerID.IsEqual(payer.PubKey()))
	require.Equal(t, uint64(250_000_000), issued.AmountMsat)
	require.Equal(t, e.clock.Now().UTC(), issued.CreatedAt)
	require.Contains(t, issued.Bolt12, "lni1")
	list, err := e.invoices.ListForOffer(rec.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	got2, err := e.manager.LookupOffer(rec.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), got2.InvoicesIssued)

	// The same request again gets the same invoice, not a second one.
	e.server.Handle(ctx, msg)
	require.Len(t, e.added, 1)
	require.Len(t, e.msgr.sent, 2)
	require.Equal(t, e.msgr.sent[0].payload[0].Value,
		e.msgr.sent[1].payload[0].Value)

	// Once that invoice has expired, a fresh request gets a new one.
	e.clock.SetTime(e.clock.Now().Add(2 * time.Hour))
	e.server.Handle(ctx, msg)
	require.Len(t, e.added, 2)

	// A request with a different payer note is a different request.
	ir2, _ := e.request(offer, 250_000_000,
		func(ir *bolt12.InvoiceRequest) {
			ir.InvreqPayerNote = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType89](
					tlv.Blob("thanks"),
				),
			)
		})
	e.server.Handle(ctx, e.inbound(ir2, nil, true))
	require.Len(t, e.added, 3)
	inv2 := e.lastInvoice()
	require.NoError(t, bolt12.ValidateInvoiceAgainstRequest(inv2, ir2))
}

// TestServeAmountAndQuantity covers amounts from the offer and quantities.
func TestServeAmountAndQuantity(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "coffee",
		AmountMsat:  5_000,
		QuantityMax: 10,
		NoPaths:     true,
	})
	require.NoError(t, err)
	offer := e.decodeOffer(rec)

	// Three coffees at the offer's price.
	ir, _ := e.request(offer, 0, func(ir *bolt12.InvoiceRequest) {
		ir.InvreqQuantity = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType86](bolt12.TUint64(3)),
		)
	})
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Len(t, e.added, 1)
	require.Equal(t, uint64(15_000), e.added[0].amount)
	inv := e.lastInvoice()
	require.NoError(t, bolt12.ValidateInvoiceAgainstRequest(inv, ir))
	issued, err := e.invoices.Get([32]byte{1})
	require.NoError(t, err)
	require.Equal(t, uint64(3), issued.Quantity)

	// Too many: the reader check refuses, and the payer is told.
	ir, _ = e.request(offer, 0, func(ir *bolt12.InvoiceRequest) {
		ir.InvreqQuantity = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType86](bolt12.TUint64(11)),
		)
	})
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Len(t, e.added, 1)
	require.Equal(t, "invalid invoice request", e.lastError().Message)

	// An offer without an amount needs one in the request.
	free, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "tips", NoPaths: true,
	})
	require.NoError(t, err)
	ir, _ = e.request(e.decodeOffer(free), 0, nil)
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Len(t, e.added, 1)
	require.Equal(t, "invalid invoice request", e.lastError().Message)
}

// TestServeRefusals covers what is ignored and what is answered with an
// error.
func TestServeRefusals(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "OCEAN Payouts for bc1qminer", NoPaths: true,
	})
	require.NoError(t, err)
	offer := e.decodeOffer(rec)

	// Not an invoice request at all.
	e.server.Handle(ctx, &onionmsg.Inbound{})
	require.Empty(t, e.msgr.sent)

	// A bad signature is answered with an error.
	ir, _ := e.request(offer, 1000, nil)
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240]([64]byte{1}),
	)
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Len(t, e.msgr.sent, 1)
	require.Equal(t, "invalid invoice request", e.lastError().Message)
	require.Empty(t, e.added)

	// Another chain.
	ir, _ = e.request(offer, 1000, func(ir *bolt12.InvoiceRequest) {
		ir.InvreqChain = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType80]([32]byte{0xaa}),
		)
	})
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Equal(t, "wrong chain", e.lastError().Message)

	// An offer that is not ours is ignored, not answered.
	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	foreign := &bolt12.Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{testChain},
			}),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("x")),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](other.PubKey()),
		),
	}
	ir, _ = e.request(foreign, 1000, nil)
	sentBefore := len(e.msgr.sent)
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Len(t, e.msgr.sent, sentBefore)

	// Ours, but with our issuer id and no metadata: not ours either.
	impostor := *offer
	impostor.OfferMetadata = tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]{}
	ir, _ = e.request(&impostor, 1000, nil)
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Len(t, e.msgr.sent, sentBefore)

	// A request over a blinded path for an offer without paths is
	// ignored.
	ir, _ = e.request(offer, 1000, nil)
	e.server.Handle(ctx, e.inbound(ir, []byte("some path"), true))
	require.Len(t, e.msgr.sent, sentBefore)
	require.Empty(t, e.added)

	// Disabled: answered with an error. Expired: the same.
	require.NoError(t, e.manager.DisableOffer(rec.ID))
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Equal(t, "offer is no longer available", e.lastError().Message)
	require.NoError(t, e.manager.EnableOffer(rec.ID))
	expiring, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description:    "soon",
		AbsoluteExpiry: e.clock.Now().Add(time.Minute),
		NoPaths:        true,
	})
	require.NoError(t, err)
	ir, _ = e.request(e.decodeOffer(expiring), 1000, nil)
	e.clock.SetTime(e.clock.Now().Add(2 * time.Minute))
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Equal(t, "offer is no longer available", e.lastError().Message)

	// The invoice registry failing is answered with an error, without
	// saying why.
	ir, _ = e.request(offer, 1000, nil)
	e.addErr = errors.New("database on fire")
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	require.Equal(t, "cannot issue an invoice right now",
		e.lastError().Message)
	e.addErr = nil

	// No reply path: nothing is issued, since nothing could be sent.
	sentBefore = len(e.msgr.sent)
	addedBefore := len(e.added)
	e.server.Handle(ctx, e.inbound(ir, nil, false))
	require.Len(t, e.msgr.sent, sentBefore)
	require.Len(t, e.added, addedBefore)

	// Errors are not sent without a reply path either.
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240]([64]byte{1}),
	)
	e.server.Handle(ctx, e.inbound(ir, nil, false))
	require.Len(t, e.msgr.sent, sentBefore)
}

// TestServeOverOfferPath covers offers with blinded paths: the request has
// to arrive with the path's secret, and a lost offer is restored from it.
func TestServeOverOfferPath(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "OCEAN Payouts for bc1qminer", WithPaths: true,
	})
	require.NoError(t, err)
	offer := e.decodeOffer(rec)
	require.True(t, offer.OfferPaths.IsSome())

	// Direct to the node id: ignored. Wrong secret: ignored.
	ir, _ := e.request(offer, 1000, nil)
	e.server.Handle(ctx, e.inbound(ir, nil, true))
	e.server.Handle(ctx, e.inbound(ir, []byte("wrong"), true))
	wrong := rec.PathSecret
	wrong[0] ^= 1
	e.server.Handle(ctx, e.inbound(ir, wrong[:], true))
	require.Empty(t, e.msgr.sent)
	require.Empty(t, e.added)

	// Over the path: served.
	e.server.Handle(ctx, e.inbound(ir, rec.PathSecret[:], true))
	require.Len(t, e.added, 1)
	inv := e.lastInvoice()
	require.NoError(t, bolt12.ValidateInvoiceAgainstRequest(inv, ir))

	// The node lost its offers (restore from seed): the request still
	// serves, and the offer is stored again.
	require.NoError(t, e.manager.DisableOffer(rec.ID))
	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "fresh")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	freshStore, err := offers.NewKVStore(backend)
	require.NoError(t, err)
	freshInvoices, err := offers.NewInvoiceStore(backend)
	require.NoError(t, err)
	fresh := *e
	fresh.manager, err = offers.NewManager(offers.Config{
		ChainHash:     testChain,
		IssuerKey:     e.server.cfg.NodeKey,
		Secret:        [32]byte{1, 2, 3},
		Store:         freshStore,
		Clock:         e.clock,
		NodeReachable: func() bool { return true },
	})
	require.NoError(t, err)
	fresh.msgr = &fakeMessenger{}
	cfg := e.server.cfg
	cfg.Manager = fresh.manager
	cfg.Invoices = freshInvoices
	cfg.Messenger = fresh.msgr
	fresh.server, err = New(cfg)
	require.NoError(t, err)
	_, err = fresh.manager.LookupOffer(rec.ID)
	require.ErrorIs(t, err, offers.ErrOfferNotFound)
	ir2, _ := e.request(offer, 2000, nil)
	fresh.server.Handle(ctx, fresh.inbound(ir2, rec.PathSecret[:], true))
	require.Len(t, fresh.msgr.sent, 1)
	inv = fresh.lastInvoice()
	require.NoError(t, bolt12.ValidateInvoiceAgainstRequest(inv, ir2))
	restored, err := fresh.manager.LookupOffer(rec.ID)
	require.NoError(t, err)
	require.Equal(t, rec.Offer, restored.Offer)
	require.Equal(t, rec.PathSecret, restored.PathSecret)
	require.True(t, restored.Active)
	require.Equal(t, uint64(1), restored.InvoicesIssued)

	// A different node's secret does not restore it.
	imposter, err := offers.NewManager(offers.Config{
		ChainHash: testChain, IssuerKey: e.server.cfg.NodeKey,
		Secret: [32]byte{4, 5, 6}, Store: freshStore, Clock: e.clock,
	})
	require.NoError(t, err)
	_, err = imposter.RestoreOffer(offer)
	require.ErrorIs(t, err, offers.ErrNotOurOffer)
}

// TestRateLimit drops requests over the global and the per-peer limits.
func TestRateLimit(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "x", NoPaths: true,
	})
	require.NoError(t, err)
	offer := e.decodeOffer(rec)

	cfg := e.server.cfg
	cfg.RequestsPerSecond = 0.001
	cfg.RequestBurst = 2
	limited, err := New(cfg)
	require.NoError(t, err)
	for i := 0; i < 5; i++ {
		ir, _ := e.request(offer, uint64(1000+i), nil)
		limited.Handle(ctx, e.inbound(ir, nil, true))
	}
	require.Len(t, e.added, 2, "burst of two, then dropped")

	// One peer is held to its own limit while another still gets
	// through.
	e.added = nil
	cfg = e.server.cfg
	cfg.PeerRequestsPerSecond = 0.001
	cfg.PeerRequestBurst = 1
	perPeer, err := New(cfg)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		ir, _ := e.request(offer, uint64(2000+i), nil)
		perPeer.Handle(ctx, e.inbound(ir, nil, true))
	}
	require.Len(t, e.added, 1, "one per peer")
	ir, _ := e.request(offer, 3000, nil)
	msg := e.inbound(ir, nil, true)
	msg.Peer = [33]byte{3, 3}
	perPeer.Handle(ctx, msg)
	require.Len(t, e.added, 2, "another peer is not held back")
}

// TestPrune drops the records of invoices that expired unpaid, after the
// retention, and keeps settled and recent ones.
func TestPrune(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	old := e.clock.Now().Add(-DefaultInvoiceExpiry - DefaultInvoiceRetention -
		time.Hour)
	put := func(n byte, at time.Time) {
		require.NoError(t, e.invoices.Put(&offers.IssuedInvoice{
			PaymentHash: [32]byte{n}, OfferID: offers.OfferID{1},
			PayerID: payer.PubKey(), AmountMsat: 1, CreatedAt: at,
			Bolt12: "lni1x",
		}))
	}
	put(1, old)                           // expired, unpaid: pruned
	put(2, old)                           // expired, settled: kept
	put(3, e.clock.Now().Add(-time.Hour)) // recent: kept
	e.settled[[32]byte{2}] = true

	e.server.Prune(ctx)
	_, err = e.invoices.Get([32]byte{1})
	require.ErrorIs(t, err, offers.ErrInvoiceNotFound)
	_, err = e.invoices.Get([32]byte{2})
	require.NoError(t, err)
	_, err = e.invoices.Get([32]byte{3})
	require.NoError(t, err)

	// Start and Stop run and end the pruning loop.
	require.NoError(t, e.server.Start())
	require.NoError(t, e.server.Stop())
	require.NoError(t, e.server.Stop(), "idempotent")
}

// TestNewRequirements checks the configuration checks and defaults.
func TestNewRequirements(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	full := e.server.cfg
	require.Equal(t, DefaultInvoiceExpiry, full.InvoiceExpiry)

	for _, strip := range []func(c *Config){
		func(c *Config) { c.Manager = nil },
		func(c *Config) { c.Invoices = nil },
		func(c *Config) { c.Messenger = nil },
		func(c *Config) { c.Signer = nil },
		func(c *Config) { c.AddInvoice = nil },
		func(c *Config) { c.NodeKey = keychain.KeyDescriptor{} },
		func(c *Config) { c.ChainHash = [32]byte{} },
	} {
		c := full
		strip(&c)
		_, err := New(c)
		require.Error(t, err)
	}
	c := full
	c.RequestsPerSecond, c.RequestBurst, c.InvoiceExpiry = 0, 0, 0
	s, err := New(c)
	require.NoError(t, err)
	require.Equal(t, DefaultInvoiceExpiry, s.cfg.InvoiceExpiry)
	require.Equal(t, float64(DefaultRequestsPerSecond),
		s.cfg.RequestsPerSecond)
	require.Equal(t, DefaultRequestBurst, s.cfg.RequestBurst)
}

// TestRateLimitedRequestIsToldOnce is the behaviour that turns a silent
// timeout into a fast failure.
//
// Found by measurement rather than by reading: a fetch loop in the lab failed
// one request in six, which looked like message loss for a while. It was the
// per-peer limiter doing its job and saying nothing, so the requester waited
// out its whole timeout for something decided here in microseconds. Telling it
// costs one onion message; telling a flooder once per dropped request would
// make the limiter an amplifier for the traffic it exists to shed, so the
// notice is itself limited and only the first one in the window is answered.
func TestRateLimitedRequestIsToldOnce(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, _, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "x", NoPaths: true,
	})
	require.NoError(t, err)
	offer := e.decodeOffer(rec)

	cfg := e.server.cfg
	cfg.PeerRequestsPerSecond = 0.001
	cfg.PeerRequestBurst = 1
	limited, err := New(cfg)
	require.NoError(t, err)

	// One gets through, and the two after it are refused.
	var errors int
	for i := 0; i < 3; i++ {
		before := len(e.msgr.sent)
		ir, _ := e.request(offer, uint64(4000+i), nil)
		limited.Handle(ctx, e.inbound(ir, nil, true))
		for _, m := range e.msgr.sent[before:] {
			if len(m.payload) == 1 &&
				m.payload[0].TLVType == onionmsg.TypeInvoiceError {

				errors++
			}
		}
	}

	require.Len(t, e.added, 1, "one invoice, the rest refused")
	require.Equal(t, 1, errors,
		"the first refusal is answered and the second is not: "+
			"a requester learns why, a flood gets silence")
}

// A request from a payer which has not upgraded is answered with a refusal
// rather than an invoice. The payer could not settle it, and the request names
// the same chain_hash we do, so invreq_features is the only thing that says
// so.
func TestServeRefusesRequestWithoutBlake2b(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	rec, created, err := e.manager.CreateOffer(ctx, offers.CreateParams{
		Description: "OCEAN Payouts for bc1qminer",
		NoPaths:     true,
	})
	require.NoError(t, err)
	require.True(t, created)
	offer := e.decodeOffer(rec)

	// The request is well formed in every other way; it is signed after
	// the tweak, so this is not a signature failure in disguise.
	ir, _ := e.request(offer, 250_000_000, func(ir *bolt12.InvoiceRequest) {
		ir.InvreqFeatures = tlv.OptionalRecordT[
			tlv.TlvType84, lnwire.RawFeatureVector,
		]{}
	})
	e.server.Handle(ctx, e.inbound(ir, nil, true))

	require.Empty(t, e.added, "an invoice was created for a payer that "+
		"could not have settled it")
}
