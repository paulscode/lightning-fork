package discovery

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/legacychain"
	"github.com/stretchr/testify/require"
)

// TestNormaliseLegacyChainHash pins that only this network's own withdrawn
// chain_hash is taken as ours.
func TestNormaliseLegacyChainHash(t *testing.T) {
	t.Parallel()

	current := *chaincfg.MainNetParams.GenesisHash
	legacy, ok := legacychain.For(current)
	require.True(t, ok)

	h := legacy
	require.True(t, normaliseLegacyChainHash(&h, current))
	require.Equal(t, current, h)

	h = current
	require.False(t, normaliseLegacyChainHash(&h, current))
	require.Equal(t, current, h)

	regtestLegacy, ok := legacychain.For(
		*chaincfg.RegressionNetParams.GenesisHash,
	)
	require.True(t, ok)
	for _, other := range []chainhash.Hash{
		{0x01}, regtestLegacy, *chaincfg.TestNet3Params.GenesisHash,
	} {
		h = other
		require.False(t, normaliseLegacyChainHash(&h, current))
		require.Equal(t, other, h)
	}
}
