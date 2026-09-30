package feature

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestNoUnifiedSigs checks that option_unified_sigs is advertised in init and
// node_announcement by default, and not at all when the node's own signatures
// do not opt into the unified signature hash.
func TestNoUnifiedSigs(t *testing.T) {
	t.Parallel()

	for _, off := range []bool{false, true} {
		m, err := NewManager(Config{NoUnifiedSigs: off})
		require.NoError(t, err)

		for _, set := range []Set{SetInit, SetNodeAnn} {
			fv := m.Get(set)
			require.Equal(t, !off,
				fv.IsSet(lnwire.UnifiedSigsOptional),
				"set %v, NoUnifiedSigs=%v", set, off)
			require.False(t, fv.IsSet(lnwire.UnifiedSigsRequired))
		}
	}
}

// TestUnifiedSigsDependsOnBlake2b pins BOLT-blake2b #9's dependency:
// option_unified_sigs without either form of option_blake2b is an invalid
// feature vector, which a peer's init fails on and pathfinding skips.
func TestUnifiedSigsDependsOnBlake2b(t *testing.T) {
	t.Parallel()

	vec := func(bits ...lnwire.FeatureBit) *lnwire.FeatureVector {
		return lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(bits...), lnwire.Features,
		)
	}

	for _, unified := range []lnwire.FeatureBit{
		lnwire.UnifiedSigsOptional, lnwire.UnifiedSigsRequired,
	} {
		err := ValidateDeps(vec(unified))
		require.ErrorContains(t, err, "missing feature dependency",
			"bit %d alone", unified)

		for _, b2b := range []lnwire.FeatureBit{
			lnwire.Blake2bRequired, lnwire.Blake2bOptional,
		} {
			require.NoError(t, ValidateDeps(vec(unified, b2b)),
				"bits %d and %d", unified, b2b)
		}
	}

	// option_blake2b alone needs nothing.
	require.NoError(t, ValidateDeps(vec(lnwire.Blake2bRequired)))

	// And the vectors this node advertises satisfy it.
	m, err := NewManager(Config{})
	require.NoError(t, err)
	for _, set := range []Set{SetInit, SetNodeAnn} {
		require.NoError(t, ValidateDeps(m.Get(set)), "set %v", set)
	}
}
