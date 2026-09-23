package feature

import (
	"fmt"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// Since the chain_hash reversal this bit is the only thing that says at init
// whether a node has upgraded. An upgraded node and one which has not carry
// the same genesis hash, so chain_hash cannot tell them apart there, in
// open_channel, or in a channel announcement. An even bit makes a peer that
// does not know it close the connection, per BOLT 1, which parts the two
// symmetrically and without the other side having to do anything.
//
// The odd form cannot do that job. An odd bit is ignored by a peer that cannot
// read it, which is precisely the peer that needs to go away. This daemon set
// the odd bit until the reversal, on the reasoning that chain_hash was the
// real check and the bit was a courtesy; that reasoning went with the
// reversal, and the bit had to change with it.
//
// Pinned here because reverting it would break nothing visible: peers that
// have upgraded would still connect and tests would still pass. When
// the odd bit was being sent, the separation had quietly fallen back to
// RequirePeerNetworks, a heuristic that drops any peer sending no networks
// TLV even though sending it is optional. That heuristic is now off by
// default, precisely because this bit does the job properly, so there is
// nothing left to fall back to.
func TestBlake2bIsAdvertisedAsRequired(t *testing.T) {
	t.Parallel()

	for _, set := range []Set{
		SetInit, SetNodeAnn, SetInvoice, SetInvoiceAmp,
	} {
		sets, ok := defaultSetDesc[lnwire.Blake2bRequired]
		require.True(t, ok, "the even bit is not advertised at all, so "+
			"nothing tells an upgraded node from one which did "+
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

	for _, set := range []Set{
		SetInit, SetNodeAnn, SetInvoice, SetInvoiceAmp,
	} {
		fv := m.Get(set)
		require.True(t, fv.IsSet(lnwire.Blake2bRequired),
			"bit %d missing from %v", lnwire.Blake2bRequired, set)
		require.False(t, fv.IsSet(lnwire.Blake2bOptional),
			"bit %d present in %v", lnwire.Blake2bOptional, set)

		// Required means required: if this reads as optional, a peer
		// that does not know the bit carries on instead of hanging up.
		require.True(t, fv.RequiresFeature(lnwire.Blake2bRequired),
			"bit %d is not being treated as required in %v",
			lnwire.Blake2bRequired, set)
	}
}

// The numbers themselves, which are a wire fact rather than an implementation
// detail: both ends have to agree on them or they refuse each other. Written
// down so a renumber cannot happen quietly on one side.
func TestTheAllocatedBitNumbers(t *testing.T) {
	t.Parallel()

	require.EqualValues(t, 512, lnwire.Blake2bRequired)
	require.EqualValues(t, 513, lnwire.Blake2bOptional)
	require.EqualValues(t, 514, lnwire.UnifiedSigsRequired)
	require.EqualValues(t, 515, lnwire.UnifiedSigsOptional)
}

// A peer refuses a required bit it does not know, and accepts one it does.
// That asymmetry is the whole mechanism. It is also why the move to the
// allocated numbers is a flag day rather than a rolling upgrade: a build which
// still names the withdrawn bit does not know this one, so the two refuse each
// other in both directions from the moment either moves.
func TestKnowingTheBitIsWhatDecides(t *testing.T) {
	t.Parallel()

	peer := lnwire.NewRawFeatureVector(lnwire.Blake2bRequired)

	// A node that knows the bit: no complaint.
	knows := lnwire.NewFeatureVector(peer, lnwire.Features)
	require.NoError(t, ValidateRequired(knows))

	// A node that does not, which is any build that has not upgraded.
	names := make(map[lnwire.FeatureBit]string, len(lnwire.Features))
	for bit, name := range lnwire.Features {
		if bit == lnwire.Blake2bRequired {
			continue
		}
		names[bit] = name
	}
	stranger := lnwire.NewFeatureVector(peer, names)

	err := ValidateRequired(stranger)
	require.Error(t, err, "a node which has not upgraded must refuse the "+
		"connection; without that it shares a network with nodes "+
		"following rules it cannot verify")
	require.Contains(t, err.Error(),
		fmt.Sprintf("%d", lnwire.Blake2bRequired))
}
