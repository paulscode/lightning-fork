package feature

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// Since the chain_hash reversal this bit is the only thing that separates the
// two chains at init. Both carry the genesis hash they share, so chain_hash
// cannot tell them apart there, in open_channel, or in a channel
// announcement. An even bit makes a peer that does not know it close the
// connection, per BOLT 1, which separates the chains symmetrically and without
// the other side having to do anything.
//
// The odd form cannot do that job. An odd bit is ignored by a peer that cannot
// read it, which is precisely the peer that needs to go away. This daemon set
// the odd bit until the reversal, on the reasoning that chain_hash was the
// real check and the bit was a courtesy; that reasoning went with the
// reversal, and the bit had to change with it.
//
// Pinned here because reverting it would break nothing visible: peers that
// understand this chain would still connect and tests would still pass. When
// the odd bit was being sent, the separation had quietly fallen back to
// RequirePeerNetworks, a heuristic that drops any peer sending no networks
// TLV even though sending it is optional. That heuristic is now off by
// default, precisely because this bit does the job properly, so there is
// nothing left to fall back to.
func TestBlake2bIsAdvertisedAsRequired(t *testing.T) {
	t.Parallel()

	for _, set := range []Set{SetInit, SetNodeAnn} {
		sets, ok := defaultSetDesc[lnwire.Blake2bRequired]
		require.True(t, ok, "the even bit is not advertised at all, so "+
			"nothing separates this chain from the one that did "+
			"not upgrade at init")

		_, ok = sets[set]
		require.True(t, ok, "the even bit is missing from %v", set)
	}

	// The odd form must not be sent alongside it. A peer reading both would
	// have no way to tell which this node meant, and BOLT 9 gives the pair
	// one meaning between them.
	_, odd := defaultSetDesc[lnwire.Blake2bOptional]
	require.False(t, odd, "the odd form is advertised; a peer that cannot "+
		"read it ignores it, which is the peer this bit exists to "+
		"disconnect")
}

// The manager has to carry the bit through to the vectors it hands out, not
// merely list it in the descriptor.
func TestBlake2bReachesTheInitVector(t *testing.T) {
	t.Parallel()

	m, err := NewManager(Config{})
	require.NoError(t, err)

	for _, set := range []Set{SetInit, SetNodeAnn} {
		fv := m.Get(set)
		require.True(t, fv.IsSet(lnwire.Blake2bRequired),
			"bit 68 missing from %v", set)
		require.False(t, fv.IsSet(lnwire.Blake2bOptional),
			"bit 69 present in %v", set)

		// Required means required: if this reads as optional, a peer
		// that does not know the bit carries on instead of hanging up.
		require.True(t, fv.RequiresFeature(lnwire.Blake2bRequired),
			"bit 68 is not being treated as required in %v", set)
	}
}

// A peer refuses a required bit it does not know, and accepts one it does.
// That asymmetry is the whole mechanism, and it is also what makes this change
// safe for nodes already in the field: an older build of this daemon has named
// bit 68 since before it set it, so it accepts a newer peer rather than
// hanging up on one.
func TestKnowingTheBitIsWhatDecides(t *testing.T) {
	t.Parallel()

	peer := lnwire.NewRawFeatureVector(lnwire.Blake2bRequired)

	// A node that knows the bit: no complaint.
	knows := lnwire.NewFeatureVector(peer, lnwire.Features)
	require.NoError(t, ValidateRequired(knows))

	// A node that does not, which is any build not updated for this chain.
	names := make(map[lnwire.FeatureBit]string, len(lnwire.Features))
	for bit, name := range lnwire.Features {
		if bit == lnwire.Blake2bRequired {
			continue
		}
		names[bit] = name
	}
	stranger := lnwire.NewFeatureVector(peer, names)

	err := ValidateRequired(stranger)
	require.Error(t, err, "a node that does not know bit 68 must refuse "+
		"the connection; without that the two chains share a network")
	require.Contains(t, err.Error(), "68")
}
