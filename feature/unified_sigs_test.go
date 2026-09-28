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
