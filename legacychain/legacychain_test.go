package legacychain_test

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/legacychain"
	"github.com/stretchr/testify/require"
)

// TestValuesAreTheKnownDerivations re-derives every legacy value the way the
// withdrawn design did, so a typo in the table cannot pass unnoticed.
func TestValuesAreTheKnownDerivations(t *testing.T) {
	t.Parallel()

	nets := []chainreg.BitcoinNetParams{
		chainreg.BitcoinMainNetParams,
		chainreg.BitcoinTestNet4Params,
		chainreg.BitcoinSigNetParams,
		chainreg.BitcoinSimNetParams,
		chainreg.BitcoinRegTestNetParams,
	}

	for _, net := range nets {
		want := chainreg.SyntheticChainHash(net.GenesisHash)
		if net.Name == chainreg.BitcoinMainNetParams.Name {
			want = *chainreg.Blake2bMainnetActivationHash
		}

		require.Equal(t, *net.GenesisHash, net.ChainHash, net.Name)

		got, ok := legacychain.For(net.ChainHash)
		require.True(t, ok, net.Name)
		require.Equal(t, want, got, net.Name)
	}
}

// TestNoneForOthers pins that nothing else maps, including a legacy value
// itself: the table goes one way.
func TestNoneForOthers(t *testing.T) {
	t.Parallel()

	legacy, ok := legacychain.For(chainreg.BitcoinMainNetParams.ChainHash)
	require.True(t, ok)

	_, ok = legacychain.For(legacy)
	require.False(t, ok)

	_, ok = legacychain.For(chainhash.Hash{})
	require.False(t, ok)
}
