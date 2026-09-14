package offerpay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
)

// IntroResolver turns a blinded path's introduction node into a node id
// when it is given as a channel and direction.
type IntroResolver func(ctx context.Context,
	node lnwire.IntroductionNode) (*btcec.PublicKey, error)

// IntentHooks is what building a payment needs from the node.
type IntentHooks struct {
	// NodeKey is this node's id, for noticing a path that starts at us.
	NodeKey *btcec.PublicKey

	// ResolveIntro resolves an introduction node given as a channel and
	// direction.
	ResolveIntro IntroResolver

	// PeerOverChannel returns the node at the other end of one of our
	// channels, for a path whose hop after ours names the channel.
	PeerOverChannel func(ctx context.Context,
		scid lnwire.ShortChannelID) (*btcec.PublicKey, error)

	// DecryptBlindedData decrypts the data a blinded path put on our
	// hop, with our node key and the path's blinding point.
	DecryptBlindedData func(blindingPoint *btcec.PublicKey,
		data []byte) ([]byte, error)

	// NextPathKey derives the blinding point for the hop after ours.
	NextPathKey func(blindingPoint *btcec.PublicKey) (*btcec.PublicKey,
		error)
}

// PaymentParams is how to pay a fetched invoice.
type PaymentParams struct {
	// FeeLimitMsat bounds the routing fee.
	FeeLimitMsat uint64

	// Timeout bounds the payment attempt.
	Timeout time.Duration

	// MaxParts bounds how many shards the payment may be split into;
	// zero means the router's default, one when the invoice does not
	// allow multi-part payments.
	MaxParts uint32

	// CltvLimit bounds the total time lock; zero means the router's
	// default.
	CltvLimit uint32
}

// PaymentIntent turns a BOLT 12 invoice into the payment the router makes:
// its blinded payment paths become the blinded path set, and the invoice's
// amount, payment hash and features are the payment's.
func PaymentIntent(ctx context.Context, inv *bolt12.Invoice,
	params PaymentParams, hooks IntentHooks) (*routing.LightningPayment,
	error) {

	var paths lnwire.BlindedPaths
	inv.InvoicePaths.WhenSome(
		func(r tlv.RecordT[tlv.TlvType160, lnwire.BlindedPaths]) {
			paths = r.Val
		},
	)
	var infos bolt12.BlindedPayInfos
	inv.InvoiceBlindedPay.WhenSome(
		func(r tlv.RecordT[tlv.TlvType162, bolt12.BlindedPayInfos]) {
			infos = r.Val
		},
	)
	if len(paths.Paths) == 0 || len(paths.Paths) != len(infos.Infos) {
		return nil, errors.New("invoice paths and payment info do " +
			"not match")
	}

	var hash [32]byte
	inv.InvoicePaymentHash.WhenSome(
		func(r tlv.RecordT[tlv.TlvType168, [32]byte]) { hash = r.Val },
	)
	amount := uint64(inv.InvoiceAmount.ValOpt().UnwrapOr(0))
	if amount == 0 {
		return nil, errors.New("invoice without an amount")
	}

	var (
		payments = make([]*routing.BlindedPayment, 0, len(paths.Paths))
		peeled   bool
		peelErr  error
	)
	for i := range paths.Paths {
		path, info := paths.Paths[i], infos.Infos[i]
		intro, err := resolveIntro(
			ctx, path.IntroductionNode, hooks.ResolveIntro,
		)
		if err != nil {
			return nil, err
		}
		sphinxPath := &sphinx.BlindedPath{
			IntroductionPoint: intro,
			BlindingPoint:     path.BlindingPoint,
		}
		for _, h := range path.Hops {
			sphinxPath.BlindedHops = append(
				sphinxPath.BlindedHops, &sphinx.BlindedHopInfo{
					BlindedNodePub: h.BlindedNodeID,
					CipherText:     h.EncryptedData,
				},
			)
		}
		features := info.Features
		payment := &routing.BlindedPayment{
			BlindedPath:         sphinxPath,
			BaseFee:             info.FeeBaseMsat,
			ProportionalFeeRate: info.FeeProportionalMillionths,
			CltvExpiryDelta:     info.CltvExpiryDelta,
			HtlcMinimum:         info.HtlcMinimumMsat,
			HtlcMaximum:         info.HtlcMaximumMsat,
			Features: lnwire.NewFeatureVector(
				&features, lnwire.Features,
			),
		}
		if err := payment.Validate(); err != nil {
			return nil, fmt.Errorf("invoice path %d: %w", i, err)
		}

		// A path that starts at this node: the router cannot pay
		// through a path it is the introduction node of, so our own
		// hop is processed here and the path continues from the next
		// node, which is a peer over one of our channels.
		if hooks.NodeKey != nil && intro.IsEqual(hooks.NodeKey) {
			payment, err = peelSelfIntro(ctx, payment, amount, hooks)
			if err != nil {
				peelErr = fmt.Errorf("invoice path %d starts "+
					"at this node: %w", i, err)
				continue
			}
			peeled = true
		}
		payments = append(payments, payment)
	}
	if len(payments) == 0 && peelErr != nil {
		return nil, peelErr
	}
	pathSet, err := routing.NewBlindedPaymentPathSet(payments)
	if err != nil {
		return nil, fmt.Errorf("invoice paths: %w", err)
	}

	intent := &routing.LightningPayment{
		Amount:            lnwire.MilliSatoshi(amount),
		FeeLimit:          lnwire.MilliSatoshi(params.FeeLimitMsat),
		PayAttemptTimeout: params.Timeout,
		MaxParts:          params.MaxParts,
		CltvLimit:         params.CltvLimit,
		BlindedPathSet:    pathSet,
		FinalCLTVDelta:    pathSet.FinalCLTVDelta(),
	}
	if err := intent.SetPaymentHash(hash); err != nil {
		return nil, err
	}
	copy(intent.Target[:], pathSet.TargetPubKey().SerializeCompressed())

	// The invoice's features say whether it takes a multi-part payment.
	// The router wants the destination's features to be consistent by
	// BOLT 9, where multi-part payments depend on payment secrets and
	// those on TLV onion payloads; a blinded path carries the secret's
	// role in the path_id, so those bits are set as understood.
	var invFeatures lnwire.RawFeatureVector
	inv.InvoiceFeatures.WhenSome(
		func(r tlv.RecordT[tlv.TlvType174, lnwire.RawFeatureVector]) {
			invFeatures = r.Val
		},
	)
	dest := lnwire.NewRawFeatureVector(
		lnwire.TLVOnionPayloadOptional, lnwire.PaymentAddrOptional,
	)
	mpp := invFeatures.IsSet(lnwire.MPPOptional) ||
		invFeatures.IsSet(lnwire.MPPRequired)
	if mpp {
		dest.Set(lnwire.MPPOptional)
	} else {
		intent.MaxParts = 1
	}
	// A peeled path's fee is exact for the invoice amount only, so the
	// payment is not split.
	if peeled {
		intent.MaxParts = 1
	}
	intent.DestFeatures = lnwire.NewFeatureVector(dest, lnwire.Features)
	if pathFeatures := pathSet.Features(); !pathFeatures.IsEmpty() {
		intent.DestFeatures = pathFeatures.Clone()
	}

	return intent, nil
}

// resolveIntro turns an introduction node into a node id.
func resolveIntro(ctx context.Context, node lnwire.IntroductionNode,
	resolve IntroResolver) (*btcec.PublicKey, error) {

	if pk, ok := node.(lnwire.PubkeyIntro); ok {
		return pk.Pubkey, nil
	}
	if resolve == nil {
		return nil, errors.New("introduction node given as a channel " +
			"cannot be resolved")
	}

	return resolve(ctx, node)
}

// peelSelfIntro processes our own hop of a blinded payment path that has
// this node as its introduction node, and returns the path from the next
// hop on. The next hop is the peer on the channel our hop's data names,
// under the blinding point that hop expects. The fee and expiry the path
// aggregates over all its hops are reduced by our hop's own relay policy,
// the way our hop would apply it when forwarding, so the amount handed to
// the next hop is what the rest of the path expects for the invoice
// amount.
func peelSelfIntro(ctx context.Context, payment *routing.BlindedPayment,
	amount uint64, hooks IntentHooks) (*routing.BlindedPayment, error) {

	if hooks.DecryptBlindedData == nil || hooks.NextPathKey == nil {
		return nil, errors.New("no way to process our own hop")
	}
	path := payment.BlindedPath
	if len(path.BlindedHops) < 2 {
		return nil, errors.New("the path ends at this node")
	}
	plain, err := hooks.DecryptBlindedData(
		path.BlindingPoint, bytes.Clone(path.BlindedHops[0].CipherText),
	)
	if err != nil {
		return nil, fmt.Errorf("own hop data: %w", err)
	}
	data, err := record.DecodeBlindedRouteData(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("own hop data: %w", err)
	}

	// Where our hop forwards to.
	var next *btcec.PublicKey
	data.NextNodeID.WhenSome(
		func(r tlv.RecordT[tlv.TlvType4, *btcec.PublicKey]) {
			next = r.Val
		},
	)
	if next == nil {
		var scid lnwire.ShortChannelID
		data.ShortChannelID.WhenSome(
			func(r tlv.RecordT[tlv.TlvType2, lnwire.ShortChannelID]) {
				scid = r.Val
			},
		)
		if scid.ToUint64() == 0 || hooks.PeerOverChannel == nil {
			return nil, errors.New("own hop names no next node")
		}
		next, err = hooks.PeerOverChannel(ctx, scid)
		if err != nil {
			return nil, fmt.Errorf("own hop's channel: %w", err)
		}
	}
	if next.IsEqual(hooks.NodeKey) {
		return nil, errors.New("own hop leads back to this node")
	}

	// The blinding point the next hop expects.
	nextKey, err := hooks.NextPathKey(path.BlindingPoint)
	if err != nil {
		return nil, fmt.Errorf("next blinding point: %w", err)
	}
	data.NextBlindingOverride.WhenSome(
		func(r tlv.RecordT[tlv.TlvType8, *btcec.PublicKey]) {
			nextKey = r.Val
		},
	)

	// What our hop would forward: the incoming amount under the path's
	// aggregate fee, less our own relay fee, rounded the way BOLT 4 has
	// a forwarding node round; the expiry less our delta.
	var (
		relay    record.PaymentRelayInfo
		hasRelay bool
	)
	data.RelayInfo.WhenSome(
		func(r tlv.RecordT[tlv.TlvType10, record.PaymentRelayInfo]) {
			relay, hasRelay = r.Val, true
		},
	)
	if !hasRelay {
		return nil, errors.New("own hop carries no payment_relay")
	}
	in, err := amountIn(amount, payment.BaseFee, payment.ProportionalFeeRate)
	if err != nil {
		return nil, err
	}
	if in < uint64(relay.BaseFee) {
		return nil, errors.New("own hop's fee exceeds the path's")
	}
	out, err := amountToForward(in-uint64(relay.BaseFee), relay.FeeRate)
	if err != nil {
		return nil, err
	}
	if out < amount {
		return nil, errors.New("own hop's fee exceeds the path's")
	}
	if out-amount > math.MaxUint32 {
		return nil, errors.New("remaining fee does not fit")
	}
	if payment.CltvExpiryDelta < relay.CltvExpiryDelta {
		return nil, errors.New("own hop's expiry delta exceeds the " +
			"path's")
	}

	return &routing.BlindedPayment{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: next,
			BlindingPoint:     nextKey,
			BlindedHops:       path.BlindedHops[1:],
		},
		// The remaining fee, exact for this amount.
		BaseFee:             uint32(out - amount),
		ProportionalFeeRate: 0,
		CltvExpiryDelta:     payment.CltvExpiryDelta - relay.CltvExpiryDelta,
		HtlcMinimum:         payment.HtlcMinimum,
		HtlcMaximum:         payment.HtlcMaximum,
		Features:            payment.Features,
	}, nil
}

// amountIn is what a path's introduction node is sent for an amount to
// arrive: the amount plus the path's aggregate fee.
func amountIn(amount uint64, baseFee, feeRate uint32) (uint64, error) {
	hi, prop := bits.Mul64(amount, uint64(feeRate))
	if hi != 0 {
		return 0, errors.New("fee overflows")
	}
	in := amount + uint64(baseFee) + prop/1_000_000
	if in < amount {
		return 0, errors.New("fee overflows")
	}

	return in, nil
}

// amountToForward is BOLT 4's forwarding amount for a hop with the given
// proportional fee, once the base fee is taken off: the division rounds up,
// so the hop after gets no less than it expects.
func amountToForward(afterBase uint64, feeRate uint32) (uint64, error) {
	denominator := 1_000_000 + uint64(feeRate)
	hi, lo := bits.Mul64(afterBase, 1_000_000)
	var carry uint64
	lo, carry = bits.Add64(lo, denominator-1, 0)
	hi += carry
	if hi >= denominator {
		return 0, errors.New("amount overflows")
	}
	out, _ := bits.Div64(hi, lo, denominator)

	return out, nil
}
