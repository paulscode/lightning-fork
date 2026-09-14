package routerrpc

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// selfHop processes this node's hop with a real sphinx router and a fixed
// channel table.
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

// TestQueryRoutesSelfIntro checks that a route query through a blinded path
// that starts at this node is answered with our hop processed: the query
// targets the node after us, with the expiry our hop no longer needs.
// Without a processor the query is refused as before.
func TestQueryRoutesSelfIntro(t *testing.T) {
	t.Parallel()

	us, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	recipient, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	scid := lnwire.NewShortChanIDFromInt(0x1234)

	router := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: us}, sphinx.NewNoOpReplayLog(),
	)
	require.NoError(t, router.Start())
	t.Cleanup(router.Stop)

	relay, err := record.EncodeBlindedRouteData(
		record.NewNonFinalBlindedRouteData(
			scid, nil, record.PaymentRelayInfo{
				CltvExpiryDelta: 40, FeeRate: 100, BaseFee: 1000,
			}, &record.PaymentConstraints{MaxCltvExpiry: 1000},
			nil,
		),
	)
	require.NoError(t, err)
	final, err := record.EncodeBlindedRouteData(
		record.NewFinalHopBlindedRouteData(nil, []byte{1}),
	)
	require.NoError(t, err)

	sessionKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	built, err := sphinx.BuildBlindedPath(sessionKey, []*sphinx.HopInfo{
		{NodePub: us.PubKey(), PlainText: relay},
		{NodePub: recipient.PubKey(), PlainText: final},
	})
	require.NoError(t, err)

	rpcPath := &lnrpc.BlindedPaymentPath{
		BlindedPath: &lnrpc.BlindedPath{
			IntroductionNode: us.PubKey().SerializeCompressed(),
			BlindingPoint: built.Path.BlindingPoint.
				SerializeCompressed(),
		},
		BaseFeeMsat:         2000,
		ProportionalFeeRate: 300,
		TotalCltvDelta:      100,
		HtlcMaxMsat:         1_000_000_000,
	}
	for _, hop := range built.Path.BlindedHops {
		rpcPath.BlindedPath.BlindedHops = append(
			rpcPath.BlindedPath.BlindedHops, &lnrpc.BlindedHop{
				BlindedNode: hop.BlindedNodePub.
					SerializeCompressed(),
				EncryptedData: hop.CipherText,
			},
		)
	}
	in := &lnrpc.QueryRoutesRequest{
		AmtMsat:             1_000_000,
		BlindedPaymentPaths: []*lnrpc.BlindedPaymentPath{rpcPath},
	}

	backend := &RouterBackend{
		SelfNode:         route.NewVertex(us.PubKey()),
		MaxTotalTimelock: 1000,
		SelfHop: &selfHop{
			router: router,
			peers: map[lnwire.ShortChannelID]*btcec.PublicKey{
				scid: recipient.PubKey(),
			},
		},
	}
	req, err := backend.parseQueryRoutesRequest(in)
	require.NoError(t, err)
	require.Equal(t, route.NewVertex(recipient.PubKey()), req.Target)
	require.Equal(t, uint16(60), req.FinalExpiry)
	require.False(t, req.BlindedPathSet.IsIntroNode(backend.SelfNode))

	backend.SelfHop = nil
	_, err = backend.parseQueryRoutesRequest(in)
	require.ErrorIs(t, err, routing.ErrSelfIntro)
}
