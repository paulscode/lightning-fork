package offerpay

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/feature"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/onionmsg"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var testChain = [32]byte{0xb2, 0xb2, 2}

type sent struct {
	dest    onionmsg.Destination
	payload []*lnwire.FinalHopTLV
	reply   *lnwire.BlindedPath
}

// fakeMessenger records sends and lets the test play the issuer.
type fakeMessenger struct {
	mu        sync.Mutex
	sent      []sent
	pathIDs   [][]byte
	onInvoice onionmsg.Handler
	onError   onionmsg.Handler
	sendErr   func(dest onionmsg.Destination) error
	replyErr  error
	nodeKey   *btcec.PrivateKey
}

func (f *fakeMessenger) Send(ctx context.Context, dest onionmsg.Destination,
	payload []*lnwire.FinalHopTLV, reply *lnwire.BlindedPath,
	opts ...onionmsg.SendOption) error {

	// The real messenger builds the reply path itself when asked to.
	if id := onionmsg.ReplyPathIDFromOptions(opts); id != nil {
		built, err := f.BuildReplyPath(ctx, id)
		if err != nil {
			return fmt.Errorf("reply path: %w", err)
		}
		reply = built
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		if err := f.sendErr(dest); err != nil {
			return err
		}
	}
	f.sent = append(f.sent, sent{dest: dest, payload: payload, reply: reply})

	return nil
}

func (f *fakeMessenger) BuildReplyPath(_ context.Context,
	pathID []byte) (*lnwire.BlindedPath, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replyErr != nil {
		return nil, f.replyErr
	}
	f.pathIDs = append(f.pathIDs, pathID)
	intro, err := lnwire.NewPubkeyIntro(f.nodeKey.PubKey())
	if err != nil {
		return nil, err
	}

	return &lnwire.BlindedPath{
		IntroductionNode: intro,
		BlindingPoint:    f.nodeKey.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: f.nodeKey.PubKey(),
			EncryptedData: pathID,
		}},
	}, nil
}

func (f *fakeMessenger) OnInvoice(h onionmsg.Handler)      { f.onInvoice = h }
func (f *fakeMessenger) OnInvoiceError(h onionmsg.Handler) { f.onError = h }

// lastRequest decodes the request last sent.
func (f *fakeMessenger) lastRequest(t *testing.T) (*bolt12.InvoiceRequest,
	[]byte) {

	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.sent)
	s := f.sent[len(f.sent)-1]
	require.Len(t, s.payload, 1)
	require.Equal(t, onionmsg.TypeInvoiceRequest, s.payload[0].TLVType)
	ir, err := bolt12.DecodeInvoiceRequest(s.payload[0].Value)
	require.NoError(t, err)
	require.NotNil(t, s.reply, "a reply path rides along")

	return ir, f.pathIDs[len(f.pathIDs)-1]
}

type env struct {
	t      *testing.T
	client *Client
	msgr   *fakeMessenger
	clock  *clock.TestClock
	issuer *btcec.PrivateKey
}

func newEnv(t *testing.T) *env {
	t.Helper()

	nodeKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	issuer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	e := &env{
		t:      t,
		msgr:   &fakeMessenger{nodeKey: nodeKey},
		clock:  clock.NewTestClock(time.Unix(1_800_000_000, 0)),
		issuer: issuer,
	}
	e.client, err = New(Config{
		Messenger: e.msgr,
		ChainHash: testChain,
		Clock:     e.clock,
		Timeout:   2 * time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, e.client.Start())

	return e
}

// offer builds an offer from the issuer, with the given amount and, when
// withPath is set, one blinded path.
func (e *env) offer(amount uint64, withPath bool,
	tweak func(*bolt12.Offer)) *bolt12.Offer {

	o := &bolt12.Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{testChain},
			}),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](
				tlv.Blob("OCEAN Payouts for bc1qminer"),
			),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](e.issuer.PubKey()),
		),

		// Written by a node which has upgraded, as every offer this
		// client will answer is. A test which wants the other case
		// clears this through tweak.
		OfferFeatures: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](
				*bolt12.Blake2bVector(),
			),
		),
	}
	if amount != 0 {
		o.OfferAmount = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType8](bolt12.TUint64(amount)),
		)
	}
	if withPath {
		intro, err := btcec.NewPrivateKey()
		require.NoError(e.t, err)
		node, err := lnwire.NewPubkeyIntro(intro.PubKey())
		require.NoError(e.t, err)
		o.OfferPaths = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType16](lnwire.BlindedPaths{
				Paths: []lnwire.BlindedPath{{
					IntroductionNode: node,
					BlindingPoint:    intro.PubKey(),
					Hops: []lnwire.BlindedHop{{
						BlindedNodeID: e.issuer.PubKey(),
						EncryptedData: []byte{1},
					}},
				}},
			}),
		)
	}
	if tweak != nil {
		tweak(o)
	}

	return o
}

// invoiceFor builds the issuer's invoice for a request.
func (e *env) invoiceFor(ir *bolt12.InvoiceRequest, amount uint64,
	tweak func(*bolt12.Invoice)) *bolt12.Invoice {

	inv := bolt12.NewInvoiceFromRequest(ir)
	intro, err := btcec.NewPrivateKey()
	require.NoError(e.t, err)
	node, err := lnwire.NewPubkeyIntro(intro.PubKey())
	require.NoError(e.t, err)
	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType160](lnwire.BlindedPaths{
			Paths: []lnwire.BlindedPath{{
				IntroductionNode: node,
				BlindingPoint:    intro.PubKey(),
				Hops: []lnwire.BlindedHop{
					{BlindedNodeID: intro.PubKey(),
						EncryptedData: []byte{1}},
					{BlindedNodeID: e.issuer.PubKey(),
						EncryptedData: []byte{2}},
				},
			}},
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
				Features:                  *lnwire.NewRawFeatureVector(),
			}},
		}),
	)
	inv.InvoiceCreatedAt = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType164](
			bolt12.TUint64(e.clock.Now().Unix()),
		),
	)
	inv.InvoiceRelativeExp = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType166](bolt12.TUint32(3600)),
	)
	inv.InvoicePaymentHash = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType168]([32]byte{7, 7}),
	)
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType170](bolt12.TUint64(amount)),
	)
	inv.InvoiceFeatures = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType174](
			*bolt12.Blake2bVector(lnwire.MPPOptional),
		),
	)
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](e.issuer.PubKey()),
	)
	if tweak != nil {
		tweak(inv)
	}
	sig, err := bolt12.SignInvoice(inv, e.issuer)
	require.NoError(e.t, err)
	inv.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)

	return inv
}

// reply delivers an invoice to the client as if it came over the reply
// path with the given path id.
func (e *env) reply(inv *bolt12.Invoice, pathID []byte) {
	// Encode from the records, without the writer checks, so invoices
	// the client must refuse can be delivered.
	stream, err := tlv.NewStream(inv.AllRecords()...)
	require.NoError(e.t, err)
	var buf bytes.Buffer
	require.NoError(e.t, stream.Encode(&buf))
	raw := buf.Bytes()
	e.msgr.onInvoice(context.Background(), &onionmsg.Inbound{
		PathID:  pathID,
		Invoice: inv,
		Records: record.CustomSet{uint64(onionmsg.TypeInvoice): raw},
	})
}

// fetch runs FetchInvoice while the issuer answers as told.
func (e *env) fetch(params FetchParams,
	answer func(ir *bolt12.InvoiceRequest, pathID []byte)) (*Fetched,
	error) {

	e.msgr.mu.Lock()
	before := len(e.msgr.sent)
	e.msgr.mu.Unlock()
	done := make(chan struct{})
	var (
		got *Fetched
		err error
	)
	go func() {
		defer close(done)
		got, err = e.client.FetchInvoice(context.Background(), params)
	}()
	if answer != nil {
		require.Eventually(e.t, func() bool {
			e.msgr.mu.Lock()
			defer e.msgr.mu.Unlock()

			return len(e.msgr.sent) > before
		}, 2*time.Second, 5*time.Millisecond)
		ir, pathID := e.msgr.lastRequest(e.t)
		answer(ir, pathID)
	}
	<-done

	return got, err
}

// TestFetchPayout fetches an invoice for an amount-less offer, the payout
// case, over the offer's path.
func TestFetchPayout(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	offer := e.offer(0, true, nil)
	got, err := e.fetch(FetchParams{
		Offer: offer, AmountMsat: 250_000_000, PayerNote: "block 1",
	}, func(ir *bolt12.InvoiceRequest, pathID []byte) {
		// The request is what the issuer expects.
		require.NoError(t, bolt12.ValidateInvoiceRequestRead(
			ir, testChain, bolt12.Blake2bFeatures,
		))
		require.Equal(t, uint64(250_000_000),
			uint64(ir.InvreqAmount.ValOpt().UnwrapOr(0)))
		note := ir.InvreqPayerNote.ValOpt().UnwrapOr(nil)
		require.Equal(t, "block 1", string(note))
		id, err := bolt12.RequestOfferID(ir)
		require.NoError(t, err)
		offerID, err := bolt12.OfferID(offer)
		require.NoError(t, err)
		require.Equal(t, offerID, id)
		require.Len(t, pathID, 32)

		// Sent over the offer's path, not the node id.
		require.NotNil(t, e.msgr.sent[0].dest.Path)
		require.Nil(t, e.msgr.sent[0].dest.NodeID)

		e.reply(e.invoiceFor(ir, 250_000_000, nil), pathID)
	})
	require.NoError(t, err)
	require.Equal(t, uint64(250_000_000), got.AmountMsat)
	require.Equal(t, [32]byte{7, 7}, got.PaymentHash)
	require.Contains(t, got.Bolt12, "lni1")
	require.NotNil(t, got.PayerKey)
	require.NoError(t, bolt12.ValidateInvoiceAgainstRequest(
		got.Invoice, got.Request,
	))
	require.Empty(t, e.client.pending, "nothing left waiting")

	// A late duplicate reply is dropped, not delivered anywhere.
	_, pathID := e.msgr.lastRequest(t)
	e.reply(e.invoiceFor(got.Request, 250_000_000, nil), pathID)
}

// TestFetchNodeIDAndFallback sends to the node id when the offer has no
// path, and falls back to it when the paths cannot be used.
func TestFetchNodeIDAndFallback(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	offer := e.offer(5000, false, nil)
	got, err := e.fetch(FetchParams{Offer: offer},
		func(ir *bolt12.InvoiceRequest, pathID []byte) {
			require.Nil(t, e.msgr.sent[0].dest.Path)
			require.True(t, e.msgr.sent[0].dest.NodeID.IsEqual(
				e.issuer.PubKey(),
			))
			require.False(t, ir.InvreqAmount.IsSome(),
				"the offer's amount is not repeated")
			e.reply(e.invoiceFor(ir, 5000, nil), pathID)
		})
	require.NoError(t, err)
	require.Equal(t, uint64(5000), got.AmountMsat)

	// Paths fail: the node id is not tried, since the issuer would
	// ignore a request for a pathed offer that came another way.
	e.msgr.sent = nil
	e.msgr.sendErr = func(dest onionmsg.Destination) error {
		if dest.Path != nil {
			return errors.New("no route")
		}

		return nil
	}
	pathed := e.offer(5000, true, nil)
	_, err = e.fetch(FetchParams{Offer: pathed, AmountMsat: 6000}, nil)
	require.ErrorContains(t, err, "over the offer's paths")
	require.Empty(t, e.msgr.sent)

	// A tip over the offer's amount is allowed.
	e.msgr.sendErr = nil
	got, err = e.fetch(FetchParams{Offer: pathed, AmountMsat: 6000},
		func(ir *bolt12.InvoiceRequest, pathID []byte) {
			require.NotNil(t, e.msgr.sent[0].dest.Path)
			e.reply(e.invoiceFor(ir, 6000, nil), pathID)
		})
	require.NoError(t, err)
	require.Equal(t, uint64(6000), got.AmountMsat, "a tip is allowed")

	// Nothing works: the send error comes back.
	e.msgr.sendErr = func(onionmsg.Destination) error {
		return errors.New("no route")
	}
	_, err = e.fetch(FetchParams{Offer: pathed, AmountMsat: 6000}, nil)
	require.ErrorContains(t, err, "no route")

	// An offer with paths and no issuer id: the invoice must be signed
	// by the path's final blinded node, which is the issuer here.
	e.msgr.sendErr = nil
	anonymous := e.offer(0, true, func(o *bolt12.Offer) {
		o.OfferIssuerID = tlv.OptionalRecordT[
			tlv.TlvType22, *btcec.PublicKey,
		]{}
	})
	got, err = e.fetch(FetchParams{Offer: anonymous, AmountMsat: 700},
		func(ir *bolt12.InvoiceRequest, pathID []byte) {
			e.reply(e.invoiceFor(ir, 700, nil), pathID)
		})
	require.NoError(t, err)
	require.Equal(t, uint64(700), got.AmountMsat)
	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	_, err = e.fetch(FetchParams{Offer: anonymous, AmountMsat: 700},
		func(ir *bolt12.InvoiceRequest, pathID []byte) {
			inv := e.invoiceFor(ir, 700, func(inv *bolt12.Invoice) {
				inv.InvoiceNodeID = tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType176](
						other.PubKey(),
					),
				)
			})
			sig, err := bolt12.SignInvoice(inv, other)
			require.NoError(t, err)
			inv.Signature = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
			)
			e.reply(inv, pathID)
		})
	require.ErrorContains(t, err, "final blinded node")

	// No node id and no path: unreachable.
	e.msgr.sendErr = nil
	pathOnly := e.offer(5000, true, func(o *bolt12.Offer) {
		o.OfferIssuerID = tlv.OptionalRecordT[
			tlv.TlvType22, *btcec.PublicKey,
		]{}
	})
	pathOnly.OfferPaths = tlv.OptionalRecordT[
		tlv.TlvType16, lnwire.BlindedPaths,
	]{}
	_, err = e.fetch(FetchParams{Offer: pathOnly}, nil)
	require.Error(t, err, "an offer with neither is invalid")
}

// TestFetchParamsChecks covers the amount and quantity rules.
func TestFetchParamsChecks(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()
	free := e.offer(0, false, nil)
	priced := e.offer(5000, false, nil)
	byQuantity := e.offer(5000, false, func(o *bolt12.Offer) {
		o.OfferQuantityMax = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType20](bolt12.TUint64(3)),
		)
	})
	anyQuantity := e.offer(5000, false, func(o *bolt12.Offer) {
		o.OfferQuantityMax = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType20](bolt12.TUint64(0)),
		)
	})

	_, err := e.client.FetchInvoice(ctx, FetchParams{Offer: free})
	require.ErrorIs(t, err, ErrAmountRequired)
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: priced, AmountMsat: 4999,
	})
	require.ErrorIs(t, err, ErrAmountBelowOffer)
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: priced, Quantity: 2,
	})
	require.ErrorIs(t, err, ErrQuantityNotOffered)
	_, err = e.client.FetchInvoice(ctx, FetchParams{Offer: byQuantity})
	require.ErrorIs(t, err, ErrQuantityRequired)
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: byQuantity, Quantity: 4,
	})
	require.ErrorContains(t, err, "maximum")
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: byQuantity, Quantity: 2, AmountMsat: 9999,
	})
	require.ErrorIs(t, err, ErrAmountBelowOffer)
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: free, AmountMsat: 1,
		PayerNote: string(make([]byte, MaxPayerNoteBytes+1)),
	})
	require.ErrorContains(t, err, "note")
	_, err = e.client.FetchInvoice(ctx, FetchParams{})
	require.Error(t, err)

	// An offer for another chain, and an expired one.
	other := e.offer(0, false, func(o *bolt12.Offer) {
		o.OfferChains = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{{0xaa}},
			}),
		)
	})
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: other, AmountMsat: 1,
	})
	require.ErrorIs(t, err, bolt12.ErrUnsupportedChain)
	expired := e.offer(0, false, func(o *bolt12.Offer) {
		o.OfferAbsoluteExpiry = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType14](
				bolt12.TUint64(e.clock.Now().Unix() - 1),
			),
		)
	})
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: expired, AmountMsat: 1,
	})
	require.ErrorIs(t, err, bolt12.ErrOfferExpired)
	require.Empty(t, e.msgr.sent, "nothing was sent")

	// Any quantity, with the amount covering it.
	got, err := e.fetch(FetchParams{Offer: anyQuantity, Quantity: 100},
		func(ir *bolt12.InvoiceRequest, pathID []byte) {
			require.Equal(t, uint64(100),
				uint64(ir.InvreqQuantity.ValOpt().UnwrapOr(0)))
			e.reply(e.invoiceFor(ir, 500_000, nil), pathID)
		})
	require.NoError(t, err)
	require.Equal(t, uint64(500_000), got.AmountMsat)
}

// TestFetchRefusesBadInvoices covers the checks on what comes back.
func TestFetchRefusesBadInvoices(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	offer := e.offer(0, false, nil)
	params := FetchParams{Offer: offer, AmountMsat: 1000}

	// Wrong amount.
	_, err := e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		e.reply(e.invoiceFor(ir, 999, nil), id)
	})
	require.ErrorContains(t, err, "999")

	// Signed by someone else.
	_, err = e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		inv := e.invoiceFor(ir, 1000, nil)
		other, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		inv.InvoiceNodeID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType176](other.PubKey()),
		)
		sig, err := bolt12.SignInvoice(inv, other)
		require.NoError(t, err)
		inv.Signature = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
		)
		e.reply(inv, id)
	})
	require.Error(t, err)

	// Tampered after signing.
	_, err = e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		inv := e.invoiceFor(ir, 1000, nil)
		inv.InvoiceAmount = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType170](bolt12.TUint64(1000)),
		)
		inv.InvoiceRelativeExp = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType166](bolt12.TUint32(1)),
		)
		e.reply(inv, id)
	})
	require.ErrorIs(t, err, bolt12.ErrInvalidSignature)

	// Already expired.
	_, err = e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		e.reply(e.invoiceFor(ir, 1000, func(inv *bolt12.Invoice) {
			inv.InvoiceCreatedAt = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType164](
					bolt12.TUint64(e.clock.Now().Unix() - 7200),
				),
			)
		}), id)
	})
	require.ErrorContains(t, err, "expired")

	// Answering a different request.
	_, err = e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		otherPayer, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		other, err := bolt12.NewInvoiceRequestFromOffer(
			offer, otherPayer.PubKey(), []byte{5}, testChain,
		)
		require.NoError(t, err)
		other.InvreqAmount = ir.InvreqAmount
		e.reply(e.invoiceFor(other, 1000, nil), id)
	})
	require.Error(t, err)

	// The issuer says no.
	_, err = e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		e.msgr.onError(context.Background(), &onionmsg.Inbound{
			PathID:       id,
			InvoiceError: &onionmsg.InvoiceError{Message: "closed"},
		})
	})
	require.ErrorIs(t, err, ErrInvoiceError)
	require.ErrorContains(t, err, "closed")

	// A reply over the wrong path is ignored, and the fetch times out.
	e.client.cfg.Timeout = 300 * time.Millisecond
	_, err = e.fetch(params, func(ir *bolt12.InvoiceRequest, id []byte) {
		wrong := append([]byte{}, id...)
		wrong[0] ^= 1
		e.reply(e.invoiceFor(ir, 1000, nil), wrong)
		e.reply(e.invoiceFor(ir, 1000, nil), []byte{1})
	})
	require.ErrorIs(t, err, ErrTimeout)

	// Cancelled by the caller.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = e.client.FetchInvoice(ctx, params)
	require.ErrorIs(t, err, context.Canceled)

	// No reply path can be built.
	e.msgr.replyErr = errors.New("no peers")
	_, err = e.client.FetchInvoice(context.Background(), params)
	require.ErrorContains(t, err, "reply path")
}

// TestPaymentIntent turns an invoice into a payment for the router.
func TestPaymentIntent(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	offer := e.offer(0, false, nil)
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{1}, testChain,
	)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(123_000)),
	)

	// A second path named by channel, resolved through the hook.
	chanIntro, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	var scid [8]byte
	binary.BigEndian.PutUint64(scid[:], 0x0102030405060708)
	sciddir, err := lnwire.NewSciddirIntro(1, scid)
	require.NoError(t, err)
	inv := e.invoiceFor(ir, 123_000, func(inv *bolt12.Invoice) {
		var paths lnwire.BlindedPaths
		inv.InvoicePaths.WhenSome(
			func(r tlv.RecordT[tlv.TlvType160, lnwire.BlindedPaths]) {
				paths = r.Val
			},
		)
		second := paths.Paths[0]
		second.IntroductionNode = sciddir
		paths.Paths = append(paths.Paths, second)
		inv.InvoicePaths = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType160](paths),
		)
		var infos bolt12.BlindedPayInfos
		inv.InvoiceBlindedPay.WhenSome(
			func(r tlv.RecordT[tlv.TlvType162, bolt12.BlindedPayInfos]) {
				infos = r.Val
			},
		)
		info := infos.Infos[0]
		info.CltvExpiryDelta = 200
		infos.Infos = append(infos.Infos, info)
		inv.InvoiceBlindedPay = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType162](infos),
		)
	})
	resolved := 0
	resolve := func(_ context.Context,
		node lnwire.IntroductionNode) (*btcec.PublicKey, error) {

		resolved++
		require.Equal(t, sciddir, node)

		return chanIntro.PubKey(), nil
	}
	intent, err := PaymentIntent(context.Background(), inv, PaymentParams{
		FeeLimitMsat: 500, Timeout: time.Minute, MaxParts: 4,
		CltvLimit: 1000,
	}, IntentHooks{ResolveIntro: resolve})
	require.NoError(t, err)
	require.Equal(t, 1, resolved)
	require.Equal(t, lnwire.MilliSatoshi(123_000), intent.Amount)
	require.Equal(t, lnwire.MilliSatoshi(500), intent.FeeLimit)
	require.Equal(t, time.Minute, intent.PayAttemptTimeout)
	require.Equal(t, uint32(4), intent.MaxParts, "MPP allowed")
	require.Equal(t, uint32(1000), intent.CltvLimit)
	require.NotNil(t, intent.BlindedPathSet)
	require.Equal(t, intent.BlindedPathSet.TargetPubKey().SerializeCompressed(),
		intent.Target[:])
	hash := intent.Identifier()
	require.Equal(t, [32]byte{7, 7}, [32]byte(hash))
	require.True(t, intent.DestFeatures.HasFeature(lnwire.MPPOptional))
	require.True(t, intent.DestFeatures.HasFeature(lnwire.PaymentAddrOptional))
	require.True(t, intent.DestFeatures.HasFeature(
		lnwire.TLVOnionPayloadOptional,
	))
	require.NoError(t, feature.ValidateDeps(intent.DestFeatures),
		"the router checks feature dependencies")
	require.Equal(t, intent.BlindedPathSet.FinalCLTVDelta(),
		intent.FinalCLTVDelta)

	// Without MPP the payment is a single part.
	single := e.invoiceFor(ir, 123_000, func(inv *bolt12.Invoice) {
		inv.InvoiceFeatures = tlv.OptionalRecordT[
			tlv.TlvType174, lnwire.RawFeatureVector,
		]{}
	})
	intent, err = PaymentIntent(context.Background(), single,
		PaymentParams{MaxParts: 4}, IntentHooks{})
	require.NoError(t, err)
	require.Equal(t, uint32(1), intent.MaxParts)
	require.False(t, intent.DestFeatures.HasFeature(lnwire.MPPOptional))
	require.NoError(t, feature.ValidateDeps(intent.DestFeatures))

	// A channel-named intro with no resolver, and mismatched paths.
	_, err = PaymentIntent(context.Background(), inv, PaymentParams{},
		IntentHooks{})
	require.Error(t, err)
	broken := e.invoiceFor(ir, 123_000, func(inv *bolt12.Invoice) {
		inv.InvoiceBlindedPay = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType162](bolt12.BlindedPayInfos{}),
		)
	})
	_, err = PaymentIntent(context.Background(), broken, PaymentParams{},
		IntentHooks{})
	require.Error(t, err)
}

// TestNewRequirements checks the configuration checks and defaults.
func TestNewRequirements(t *testing.T) {
	t.Parallel()

	_, err := New(Config{ChainHash: testChain})
	require.Error(t, err)
	_, err = New(Config{Messenger: &fakeMessenger{}})
	require.Error(t, err)
	c, err := New(Config{Messenger: &fakeMessenger{}, ChainHash: testChain})
	require.NoError(t, err)
	require.Equal(t, DefaultTimeout, c.cfg.Timeout)
	require.NotNil(t, c.cfg.Clock)
	require.NoError(t, c.Stop())
}

// An offer or an invoice written by a node which has not upgraded is refused,
// in both of the places a payer meets one. Neither is visible from chain_hash,
// which is the same on both sides, so without these rules a payer would ask
// for and then try to settle a payment that cannot be made.
//
// Written as a refusal rather than as a happy path on purpose: the rule sits
// behind checks that pass anyway, so a version of it that never ran would look
// exactly like this one until something went unpaid.
func TestFetchRefusesArtifactsWithoutBlake2b(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	ctx := context.Background()

	// An offer with no feature vector at all, which is what a node that
	// has not upgraded writes.
	bare := e.offer(1000, false, func(o *bolt12.Offer) {
		o.OfferFeatures = tlv.OptionalRecordT[
			tlv.TlvType12, lnwire.RawFeatureVector,
		]{}
	})
	_, err := e.client.FetchInvoice(ctx, FetchParams{
		Offer: bare, AmountMsat: 1000,
	})
	require.ErrorIs(t, err, bolt12.ErrMissingBlake2b)

	// One that carries features, but not this bit: an offer for the
	// network we are no longer part of.
	other := e.offer(1000, false, func(o *bolt12.Offer) {
		o.OfferFeatures = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](
				*lnwire.NewRawFeatureVector(lnwire.MPPOptional),
			),
		)
	})
	_, err = e.client.FetchInvoice(ctx, FetchParams{
		Offer: other, AmountMsat: 1000,
	})
	require.ErrorIs(t, err, bolt12.ErrMissingBlake2b)

	// And an invoice that comes back without it, for a request that was
	// properly made. The offer here is ours, so everything else about the
	// exchange is in order.
	good := e.offer(0, false, nil)
	_, err = e.fetch(FetchParams{Offer: good, AmountMsat: 1000},
		func(ir *bolt12.InvoiceRequest, id []byte) {
			e.reply(e.invoiceFor(ir, 1000,
				func(inv *bolt12.Invoice) {
					inv.InvoiceFeatures = tlv.SomeRecordT(
						tlv.NewRecordT[tlv.TlvType174](
							*lnwire.NewRawFeatureVector(
								lnwire.MPPOptional,
							),
						),
					)
				},
			), id)
		},
	)
	require.ErrorIs(t, err, bolt12.ErrMissingBlake2b)
}

// The other direction: what this node writes says so, so that a reader which
// has not upgraded refuses it on the unknown even bit.
func TestRequestsWeWriteSayBlake2b(t *testing.T) {
	t.Parallel()

	e := newEnv(t)
	offer := e.offer(1000, false, nil)
	_, err := e.fetch(FetchParams{Offer: offer, AmountMsat: 1000},
		func(ir *bolt12.InvoiceRequest, id []byte) {
			require.NoError(t, bolt12.CheckBlake2b(
				ir.InvreqFeatures, "invoice request",
			))
			e.reply(e.invoiceFor(ir, 1000, nil), id)
		},
	)
	require.NoError(t, err)
}
