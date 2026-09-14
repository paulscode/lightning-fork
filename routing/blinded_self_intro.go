package routing

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
)

// SelfHopProcessor lets the router process this node's own hop of a blinded
// payment path that starts at it. A recipient that has only this node as a
// peer builds every path with this node as the introduction node; paying
// such a path means doing here what the introduction node would do:
// decrypt our hop's data, find the next node, derive the next blinding
// point, and forward what our relay policy says.
type SelfHopProcessor interface {
	// DecryptBlindedData decrypts the data a blinded path put on our
	// hop, with our node key and the path's blinding point.
	DecryptBlindedData(blindingPoint *btcec.PublicKey,
		data []byte) ([]byte, error)

	// NextBlindingPoint derives the blinding point for the hop after
	// ours from the one at ours.
	NextBlindingPoint(blindingPoint *btcec.PublicKey) (*btcec.PublicKey,
		error)

	// PeerOverChannel returns the node at the other end of one of our
	// channels, for a hop whose data names the channel.
	PeerOverChannel(scid lnwire.ShortChannelID) (*btcec.PublicKey, error)
}

// ErrSelfIntroUnpayable is returned when every path in a set starts at this
// node and none of them can be processed.
var ErrSelfIntroUnpayable = errors.New("no payable path: every path starts " +
	"at this node and none can be processed")

// PeelSelfIntro returns a path set in which every path that starts at self
// has had our own hop processed, so that the router can pay through it as
// through any other path. Paths that do not start at self are kept as they
// are; paths that start at self but cannot be processed are dropped. When
// no path is left, an error says why.
func PeelSelfIntro(set *BlindedPaymentPathSet, self route.Vertex,
	proc SelfHopProcessor) (*BlindedPaymentPathSet, error) {

	if set == nil || !set.IsIntroNode(self) {
		return set, nil
	}
	if proc == nil {
		return nil, ErrSelfIntro
	}

	var (
		kept    []*BlindedPayment
		lastErr error
	)
	for _, path := range set.paths {
		intro := route.NewVertex(path.BlindedPath.IntroductionPoint)
		if intro != self {
			kept = append(kept, stripNUMSHop(path))
			continue
		}
		peeled, err := peelOwnHop(stripNUMSHop(path), self, proc)
		if err != nil {
			log.Warnf("Blinded path starting at this node cannot "+
				"be processed: %v", err)
			lastErr = err
			continue
		}
		kept = append(kept, peeled)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("%w: %v", ErrSelfIntroUnpayable, lastErr)
	}
	peeled, err := NewBlindedPaymentPathSet(kept)
	if err != nil {
		return nil, err
	}
	// Our hop is processed once; a path that still starts at us after
	// that names us twice, which no honest recipient does.
	if peeled.IsIntroNode(self) {
		return nil, fmt.Errorf("%w: a path names this node twice",
			ErrSelfIntroUnpayable)
	}

	return peeled, nil
}

// PeelPaymentForSelf returns a copy of a payment whose blinded path set has
// had our own hop processed, with the target and final expiry the router
// pathfinds with taken from the new set. The caller's payment is left as it
// was. A payment with no path set, or none starting at self, is returned
// unchanged.
func PeelPaymentForSelf(p *LightningPayment, self route.Vertex,
	proc SelfHopProcessor) (*LightningPayment, error) {

	if p.BlindedPathSet == nil || !p.BlindedPathSet.IsIntroNode(self) {
		return p, nil
	}
	peeled, err := PeelSelfIntro(p.BlindedPathSet, self, proc)
	if err != nil {
		return nil, err
	}

	// The target is the set's, which changes when the remaining path is
	// a single hop and the recipient's real key becomes the target; so
	// does the final expiry.
	pp := *p
	pp.BlindedPathSet = peeled
	copy(pp.Target[:], peeled.TargetPubKey().SerializeCompressed())
	pp.FinalCLTVDelta = peeled.FinalCLTVDelta()

	return &pp, nil
}

// stripNUMSHop returns a copy of the payment without the final hop the path
// set adds for path finding, so the set can be built again.
func stripNUMSHop(b *BlindedPayment) *BlindedPayment {
	c := b.deepCopy()
	hops := c.BlindedPath.BlindedHops
	if len(hops) > 1 && IsBlindedRouteNUMSTargetKey(
		hops[len(hops)-1].BlindedNodePub.SerializeCompressed(),
	) {
		c.BlindedPath.BlindedHops = hops[:len(hops)-1]
	}

	return c
}

// peelOwnHop processes our own hop of a path that starts at this node and
// returns the path from the next hop on, under the blinding point that hop
// expects, with the path's aggregate fee and expiry reduced by our hop's
// relay policy.
func peelOwnHop(payment *BlindedPayment, self route.Vertex,
	proc SelfHopProcessor) (*BlindedPayment, error) {

	path := payment.BlindedPath
	if len(path.BlindedHops) < 2 {
		return nil, errors.New("the path ends at this node")
	}
	plain, err := proc.DecryptBlindedData(
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
		if scid.ToUint64() == 0 {
			return nil, errors.New("own hop names no next node")
		}
		next, err = proc.PeerOverChannel(scid)
		if err != nil {
			return nil, fmt.Errorf("own hop's channel %v: %w", scid,
				err)
		}
	}

	if route.NewVertex(next) == self {
		return nil, errors.New("own hop leads back to this node")
	}

	// The blinding point the next hop expects.
	nextKey, err := proc.NextBlindingPoint(path.BlindingPoint)
	if err != nil {
		return nil, fmt.Errorf("next blinding point: %w", err)
	}
	data.NextBlindingOverride.WhenSome(
		func(r tlv.RecordT[tlv.TlvType8, *btcec.PublicKey]) {
			nextKey = r.Val
		},
	)

	// Our hop's relay policy, which the recipient folded into the path's
	// aggregate fee and expiry.
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
	baseFee, feeRate, err := feeAfterOwnHop(
		payment.BaseFee, payment.ProportionalFeeRate,
		uint32(relay.BaseFee), relay.FeeRate,
	)
	if err != nil {
		return nil, err
	}
	if payment.CltvExpiryDelta < relay.CltvExpiryDelta {
		return nil, errors.New("own hop's expiry delta exceeds the " +
			"path's")
	}

	return &BlindedPayment{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: next,
			BlindingPoint:     nextKey,
			BlindedHops:       path.BlindedHops[1:],
		},
		BaseFee:             baseFee,
		ProportionalFeeRate: feeRate,
		CltvExpiryDelta:     payment.CltvExpiryDelta - relay.CltvExpiryDelta,
		HtlcMinimum:         payment.HtlcMinimum,
		HtlcMaximum:         payment.HtlcMaximum,
		Features:            payment.Features,
	}, nil
}

// feeAfterOwnHop turns a path's aggregate fee (base and proportional, as the
// recipient computed them over every hop including ours) into the fee for
// the path from the next hop on, by taking our own hop's policy back out.
//
// A forwarding node sends on amt_to_forward = ceil((amt_in - base) * 1e6 /
// (1e6 + rate)), and amt_in for the introduction node is amount + B +
// floor(amount * P / 1e6). What our hop would forward is then at most
// amount * (1e6 + P) / (1e6 + p) + (B - b) * 1e6 / (1e6 + p) + 1, so a base of
// ceil((B - b) * 1e6 / (1e6 + p)) + 2 and a rate of ceil((P - p) * 1e6 /
// (1e6 + p)) send the next hop at least that for every amount, and at most
// three millisatoshi plus one millionth of the amount more, which the
// recipient keeps.
func feeAfterOwnHop(pathBase, pathRate, ownBase, ownRate uint32) (uint32,
	uint32, error) {

	if pathBase < ownBase || pathRate < ownRate {
		return 0, 0, errors.New("own hop's fee exceeds the path's")
	}
	denominator := 1_000_000 + uint64(ownRate)
	base := ceilDiv(uint64(pathBase-ownBase)*1_000_000, denominator) + 2
	rate := ceilDiv(uint64(pathRate-ownRate)*1_000_000, denominator)
	if base > uint64(^uint32(0)) || rate > uint64(^uint32(0)) {
		return 0, 0, errors.New("fee after own hop does not fit")
	}

	return uint32(base), uint32(rate), nil
}

// ceilDiv divides, rounding up.
func ceilDiv(n, d uint64) uint64 {
	q, r := bits.Div64(0, n, d)
	if r != 0 {
		q++
	}

	return q
}
