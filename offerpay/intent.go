package offerpay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/tlv"
)

// IntroResolver turns a blinded path's introduction node into a node id
// when it is given as a channel and direction.
type IntroResolver func(ctx context.Context,
	node lnwire.IntroductionNode) (*btcec.PublicKey, error)

// IntentHooks is what building a payment needs from the node.
type IntentHooks struct {
	// ResolveIntro resolves an introduction node given as a channel and
	// direction.
	ResolveIntro IntroResolver
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

	payments := make([]*routing.BlindedPayment, 0, len(paths.Paths))
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
		payments = append(payments, payment)
	}
	// A path that starts at this node, because the node is the issuer's
	// channel peer, is the router's to handle: it processes our own hop
	// when the payment is made.
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
