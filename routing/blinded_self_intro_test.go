package routing

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// forwardedByHop is what a forwarding node sends on for an incoming amount
// under its policy, as BOLT 4 has it: the division rounds up.
func forwardedByHop(in uint64, base, rate uint32) uint64 {
	if in < uint64(base) {
		return 0
	}
	n := (in - uint64(base)) * 1_000_000
	d := 1_000_000 + uint64(rate)

	return (n + d - 1) / d
}

// TestFeeAfterOwnHop checks, over many policies and amounts, that paying
// the remaining path with the fee taken back out sends the next hop at
// least what our hop would have forwarded, and not much more.
func TestFeeAfterOwnHop(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20_000; i++ {
		// Our hop's policy and the rest of the path's, aggregated the
		// way a recipient aggregates them (fees compound).
		ownBase := uint32(rng.Intn(5_000))
		ownRate := uint32(rng.Intn(5_000))
		restBase := uint32(rng.Intn(5_000))
		restRate := uint32(rng.Intn(5_000))
		pathBase := ownBase + restBase +
			uint32((uint64(restBase)*uint64(ownRate)+999_999)/1_000_000)
		pathRate := ownRate + restRate +
			uint32((uint64(restRate)*uint64(ownRate)+999_999)/1_000_000)

		base, rate, err := feeAfterOwnHop(
			pathBase, pathRate, ownBase, ownRate,
		)
		require.NoError(t, err)

		amount := uint64(1 + rng.Intn(1_000_000_000))
		if i%3 == 0 {
			amount = uint64(1 + rng.Intn(20_000))
		}
		in := amount + uint64(pathBase) + amount*uint64(pathRate)/1_000_000
		want := forwardedByHop(in, ownBase, ownRate)
		got := amount + uint64(base) + amount*uint64(rate)/1_000_000
		require.GreaterOrEqual(t, got, want,
			"amount %d own %d/%d path %d/%d", amount, ownBase,
			ownRate, pathBase, pathRate)
		require.LessOrEqual(t, got-want, uint64(3)+amount/1_000_000,
			"overpays by more than a few msat")
	}

	_, _, err := feeAfterOwnHop(10, 10, 11, 10)
	require.Error(t, err, "own base above the path's")
	_, _, err = feeAfterOwnHop(10, 10, 10, 11)
	require.Error(t, err, "own rate above the path's")
	base, rate, err := feeAfterOwnHop(1000, 1000, 1000, 1000)
	require.NoError(t, err)
	require.Equal(t, uint32(2), base, "only the slack remains")
	require.Equal(t, uint32(0), rate)
}

// selfHop is a SelfHopProcessor over a real sphinx router with a node key.
type selfHop struct {
	router *sphinx.Router
	peers  map[lnwire.ShortChannelID]*btcec.PublicKey
}

func (s *selfHop) DecryptBlindedData(blindingPoint *btcec.PublicKey,
	data []byte) ([]byte, error) {

	return s.router.DecryptBlindedHopData(blindingPoint, data)
}

func (s *selfHop) NextBlindingPoint(
	blindingPoint *btcec.PublicKey) (*btcec.PublicKey, error) {

	return s.router.NextEphemeral(blindingPoint)
}

func (s *selfHop) PeerOverChannel(
	scid lnwire.ShortChannelID) (*btcec.PublicKey, error) {

	if pub, ok := s.peers[scid]; ok {
		return pub, nil
	}

	return nil, errors.New("no such channel")
}

func newRouter(t *testing.T, key *btcec.PrivateKey) *sphinx.Router {
	t.Helper()

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: key}, sphinx.NewNoOpReplayLog(),
	)
	require.NoError(t, router.Start())
	t.Cleanup(router.Stop)

	return router
}

// TestPeelSelfIntro builds a real blinded path that starts at this node,
// peels our hop, and checks the next hop can process what is left.
func TestPeelSelfIntro(t *testing.T) {
	t.Parallel()

	us, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipient, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	self := route.NewVertex(us.PubKey())

	scid := lnwire.NewShortChanIDFromInt(0x1234)
	ourHop := record.NewNonFinalBlindedRouteData(
		scid, nil, record.PaymentRelayInfo{
			CltvExpiryDelta: 40, FeeRate: 1000, BaseFee: 1000,
		}, &record.PaymentConstraints{MaxCltvExpiry: 1000}, nil,
	)
	ourPlain, err := record.EncodeBlindedRouteData(ourHop)
	require.NoError(t, err)
	peerHop := record.NewNonFinalBlindedRouteData(
		lnwire.NewShortChanIDFromInt(0x5678), nil,
		record.PaymentRelayInfo{
			CltvExpiryDelta: 80, FeeRate: 500, BaseFee: 500,
		}, &record.PaymentConstraints{MaxCltvExpiry: 1000}, nil,
	)
	peerPlain, err := record.EncodeBlindedRouteData(peerHop)
	require.NoError(t, err)
	finalPlain, err := record.EncodeBlindedRouteData(
		record.NewFinalHopBlindedRouteData(nil, []byte{1, 2, 3}),
	)
	require.NoError(t, err)
	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	built, err := sphinx.BuildBlindedPath(sessionKey, []*sphinx.HopInfo{
		{NodePub: us.PubKey(), PlainText: ourPlain},
		{NodePub: peer.PubKey(), PlainText: peerPlain},
		{NodePub: recipient.PubKey(), PlainText: finalPlain},
	})
	require.NoError(t, err)

	// The aggregate over both relay hops, as the recipient computes it.
	payment := &BlindedPayment{
		BlindedPath:         built.Path,
		BaseFee:             1501,
		ProportionalFeeRate: 1501,
		CltvExpiryDelta:     120,
		HtlcMinimum:         1,
		HtlcMaximum:         1_000_000_000,
		Features:            lnwire.EmptyFeatureVector(),
	}
	set, err := NewBlindedPaymentPathSet([]*BlindedPayment{payment})
	require.NoError(t, err)
	require.True(t, set.IsIntroNode(self))

	proc := &selfHop{
		router: newRouter(t, us),
		peers:  map[lnwire.ShortChannelID]*btcec.PublicKey{scid: peer.PubKey()},
	}
	peeled, err := PeelSelfIntro(set, self, proc)
	require.NoError(t, err)
	require.False(t, peeled.IsIntroNode(self))
	require.True(t, peeled.IsIntroNode(route.NewVertex(peer.PubKey())))
	require.Len(t, peeled.paths, 1)
	got := peeled.paths[0]

	// Our hop is gone, the marker hop is back once, and the fee and
	// expiry have our policy taken out.
	require.Len(t, got.BlindedPath.BlindedHops, 3)
	require.Equal(t, built.Path.BlindedHops[1].CipherText,
		got.BlindedPath.BlindedHops[0].CipherText)
	require.True(t, IsBlindedRouteNUMSTargetKey(
		got.BlindedPath.BlindedHops[2].BlindedNodePub.SerializeCompressed(),
	))
	require.Equal(t, uint16(80), got.CltvExpiryDelta)
	require.Equal(t, uint32(503), got.BaseFee, "ceil(501e6/1001e3)+2")
	require.Equal(t, uint32(501), got.ProportionalFeeRate)
	expectedKey, err := proc.router.NextEphemeral(built.Path.BlindingPoint)
	require.NoError(t, err)
	require.True(t, got.BlindedPath.BlindingPoint.IsEqual(expectedKey))

	// The next hop can process its hop under the derived key.
	peerRouter := newRouter(t, peer)
	plain, err := peerRouter.DecryptBlindedHopData(
		got.BlindedPath.BlindingPoint,
		bytes.Clone(got.BlindedPath.BlindedHops[0].CipherText),
	)
	require.NoError(t, err)
	data, err := record.DecodeBlindedRouteData(bytes.NewReader(plain))
	require.NoError(t, err)
	require.True(t, data.ShortChannelID.IsSome())

	// A set not starting at us is returned as is; with no processor the
	// old refusal stands; a path ending at us is unpayable; an unknown
	// channel is unpayable.
	same, err := PeelSelfIntro(set, route.NewVertex(peer.PubKey()), proc)
	require.NoError(t, err)
	require.Equal(t, set, same)
	_, err = PeelSelfIntro(set, self, nil)
	require.ErrorIs(t, err, ErrSelfIntro)
	short, err := NewBlindedPaymentPathSet([]*BlindedPayment{{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: us.PubKey(),
			BlindingPoint:     built.Path.BlindingPoint,
			BlindedHops:       built.Path.BlindedHops[:1],
		},
		Features: lnwire.EmptyFeatureVector(),
	}})
	require.NoError(t, err)
	_, err = PeelSelfIntro(short, self, proc)
	require.ErrorIs(t, err, ErrSelfIntroUnpayable)
	noChan := &selfHop{router: proc.router, peers: nil}
	_, err = PeelSelfIntro(set, self, noChan)
	require.ErrorIs(t, err, ErrSelfIntroUnpayable)

	// A mixed set keeps the path through another node and peels ours.
	other, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	viaOther := payment.deepCopy()
	viaOther.BlindedPath.IntroductionPoint = other.PubKey()
	mixed, err := NewBlindedPaymentPathSet([]*BlindedPayment{
		payment, viaOther,
	})
	require.NoError(t, err)
	peeled, err = PeelSelfIntro(mixed, self, proc)
	require.NoError(t, err)
	require.Len(t, peeled.paths, 2)
	require.False(t, peeled.IsIntroNode(self))
	require.True(t, peeled.IsIntroNode(route.NewVertex(other.PubKey())))
}

// TestPeelPaymentForSelf checks the payment the router pathfinds with after
// peeling: the target and final expiry follow the peeled set, a path that
// ends right after our hop makes the recipient's real key the target, a
// path that leads back to us is refused, and a huge own base fee is refused
// rather than truncated.
func TestPeelPaymentForSelf(t *testing.T) {
	t.Parallel()

	us, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	peer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipient, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	self := route.NewVertex(us.PubKey())
	scid := lnwire.NewShortChanIDFromInt(0x1234)
	scidToRecipient := lnwire.NewShortChanIDFromInt(0x4321)
	proc := &selfHop{
		router: newRouter(t, us),
		peers: map[lnwire.ShortChannelID]*btcec.PublicKey{
			scid:            peer.PubKey(),
			scidToRecipient: recipient.PubKey(),
		},
	}

	build := func(hops []*sphinx.HopInfo) *sphinx.BlindedPath {
		sessionKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		built, err := sphinx.BuildBlindedPath(sessionKey, hops)
		require.NoError(t, err)

		return built.Path
	}
	relay := func(scid lnwire.ShortChannelID, base uint64) []byte {
		data := record.NewNonFinalBlindedRouteData(
			scid, nil, record.PaymentRelayInfo{
				CltvExpiryDelta: 40, FeeRate: 100,
				BaseFee: lnwire.MilliSatoshi(base),
			}, &record.PaymentConstraints{MaxCltvExpiry: 1000}, nil,
		)
		plain, err := record.EncodeBlindedRouteData(data)
		require.NoError(t, err)

		return plain
	}
	finalPlain, err := record.EncodeBlindedRouteData(
		record.NewFinalHopBlindedRouteData(nil, []byte{1}),
	)
	require.NoError(t, err)
	payment := func(path *sphinx.BlindedPath) *LightningPayment {
		set, err := NewBlindedPaymentPathSet([]*BlindedPayment{{
			BlindedPath:         path,
			BaseFee:             2000,
			ProportionalFeeRate: 300,
			CltvExpiryDelta:     100,
			HtlcMaximum:         1_000_000_000,
			Features:            lnwire.EmptyFeatureVector(),
		}})
		require.NoError(t, err)
		p := &LightningPayment{
			Amount:         1_000_000,
			BlindedPathSet: set,
			FinalCLTVDelta: set.FinalCLTVDelta(),
		}
		copy(p.Target[:], set.TargetPubKey().SerializeCompressed())

		return p
	}

	// Three hops: after ours, a two-hop path with the marker target.
	long := payment(build([]*sphinx.HopInfo{
		{NodePub: us.PubKey(), PlainText: relay(scid, 1000)},
		{NodePub: peer.PubKey(), PlainText: relay(
			lnwire.NewShortChanIDFromInt(9), 500,
		)},
		{NodePub: recipient.PubKey(), PlainText: finalPlain},
	}))
	peeled, err := PeelPaymentForSelf(long, self, proc)
	require.NoError(t, err)
	require.NotSame(t, long, peeled, "the caller's payment is left alone")
	require.True(t, long.BlindedPathSet.IsIntroNode(self))
	require.False(t, peeled.BlindedPathSet.IsIntroNode(self))
	require.True(t, IsBlindedRouteNUMSTargetKey(peeled.Target[:]))
	// A multi-hop set carries its expiry inside the blinded path, so
	// the final delta the router adds is the set's, which is zero.
	require.Equal(t, peeled.BlindedPathSet.FinalCLTVDelta(),
		peeled.FinalCLTVDelta)
	require.Equal(t, uint16(0), peeled.FinalCLTVDelta)

	// Two hops: after ours the recipient is the only hop, so it becomes
	// the target itself, with the remaining expiry as the final delta.
	short := payment(build([]*sphinx.HopInfo{
		{NodePub: us.PubKey(), PlainText: relay(scidToRecipient, 1000)},
		{NodePub: recipient.PubKey(), PlainText: finalPlain},
	}))
	require.True(t, IsBlindedRouteNUMSTargetKey(short.Target[:]))
	peeled, err = PeelPaymentForSelf(short, self, proc)
	require.NoError(t, err)
	require.Equal(t, route.NewVertex(recipient.PubKey()), peeled.Target)
	require.Equal(t, uint16(60), peeled.FinalCLTVDelta)
	hints, err := peeled.BlindedPathSet.ToRouteHints()
	require.NoError(t, err)
	require.Empty(t, hints, "a single-hop path needs no hint")

	// A path that leads back to us is refused.
	loop := payment(build([]*sphinx.HopInfo{
		{NodePub: us.PubKey(), PlainText: relay(
			lnwire.NewShortChanIDFromInt(7), 1000,
		)},
		{NodePub: us.PubKey(), PlainText: relay(scid, 1000)},
		{NodePub: recipient.PubKey(), PlainText: finalPlain},
	}))
	loopProc := &selfHop{
		router: proc.router,
		peers: map[lnwire.ShortChannelID]*btcec.PublicKey{
			lnwire.NewShortChanIDFromInt(7): us.PubKey(),
			scid:                            peer.PubKey(),
			scidToRecipient:                 recipient.PubKey(),
		},
	}
	_, err = PeelPaymentForSelf(loop, self, loopProc)
	require.ErrorIs(t, err, ErrSelfIntroUnpayable)

	// An own base fee above the path's aggregate is refused: the
	// recipient folded every hop's fee into the aggregate, so ours can
	// never exceed it.
	greedy := payment(build([]*sphinx.HopInfo{
		{NodePub: us.PubKey(), PlainText: relay(scidToRecipient, 5000)},
		{NodePub: recipient.PubKey(), PlainText: finalPlain},
	}))
	_, err = PeelPaymentForSelf(greedy, self, proc)
	require.ErrorIs(t, err, ErrSelfIntroUnpayable)

	// Not ours to peel: returned as is.
	other := payment(build([]*sphinx.HopInfo{
		{NodePub: peer.PubKey(), PlainText: relay(scid, 1000)},
		{NodePub: recipient.PubKey(), PlainText: finalPlain},
	}))
	same, err := PeelPaymentForSelf(other, self, proc)
	require.NoError(t, err)
	require.Same(t, other, same)
}
