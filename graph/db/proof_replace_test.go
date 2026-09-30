package graphdb

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestAddEdgeProofReplacesCachedProof checks that a proof added over an
// existing one is what later reads return, including the horizon query that
// serves gossip from the channel cache. The fork replaces a channel's proof
// when both sides re-sign an announcement first signed under the chain hash
// it has since withdrawn, and a stale cached copy would keep serving the old
// signatures.
func TestAddEdgeProofReplacesCachedProof(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	graph := NewVersionedGraph(MakeTestGraph(t), lnwire.GossipVersion1)

	node1 := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, graph.AddNode(ctx, node1))
	node2 := createTestVertex(t, lnwire.GossipVersion1)
	require.NoError(t, graph.AddNode(ctx, node2))

	channel, chanID := createEdge(
		lnwire.GossipVersion1, 100, 0, 0, 0, node1, node2,
	)
	require.NoError(t, graph.AddChannelEdge(ctx, channel))

	updated := time.Unix(1234, 0)
	for i, node := range []*models.Node{node2, node1} {
		policy := newEdgePolicy(
			lnwire.GossipVersion1, chanID.ToUint64(),
			updated.Unix(), i == 0,
		)
		policy.ChannelFlags = lnwire.ChanUpdateChanFlags(i)
		policy.ToNode = node.PubKeyBytes
		policy.SigBytes = testSig.Serialize()
		require.NoError(t, graph.UpdateEdgePolicy(ctx, policy))
	}

	horizonProof := func() *models.ChannelAuthProof {
		t.Helper()

		edges, err := fn.CollectErr(graph.ChanUpdatesInHorizon(
			ctx, ChanUpdateRange{
				StartTime: fn.Some(updated.Add(-time.Minute)),
				EndTime:   fn.Some(updated.Add(time.Minute)),
			},
		))
		require.NoError(t, err)
		require.Len(t, edges, 1)

		return edges[0].Info.AuthProof
	}

	// The first query loads the channel into the cache.
	first := horizonProof()
	require.NotNil(t, first)

	// Replace the proof with different signatures.
	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	sig := ecdsa.Sign(priv, make([]byte, 32)).Serialize()
	replacement := models.NewV1ChannelAuthProof(sig, sig, sig, sig)
	require.NotEqual(t, first.NodeSig1(), replacement.NodeSig1())

	require.NoError(t, graph.AddEdgeProof(ctx, chanID, replacement))

	got := horizonProof()
	require.Equal(t, replacement.NodeSig1(), got.NodeSig1(),
		"the horizon query served the cached proof")
	require.Equal(t, replacement.BitcoinSig2(), got.BitcoinSig2())

	dbEdge, _, _, err := graph.FetchChannelEdgesByID(
		ctx, chanID.ToUint64(),
	)
	require.NoError(t, err)
	require.Equal(t, replacement.NodeSig1(), dbEdge.AuthProof.NodeSig1())
}
