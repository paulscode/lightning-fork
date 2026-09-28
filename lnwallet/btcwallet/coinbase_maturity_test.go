package btcwallet

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestWalletChainParams checks that btcwallet is handed the relay depth for
// coinbase maturity where the long rule is deployed, that the caller's params
// are left alone, and that networks without the rule are unchanged.
func TestWalletChainParams(t *testing.T) {
	t.Parallel()

	main := &chaincfg.MainNetParams
	require.True(t, main.CoinbaseMaturityLongDeployed(),
		"mainnet should carry the long coinbase maturity rule")

	before := main.CoinbaseMaturity
	got := walletChainParams(main)

	require.NotSame(t, main, got)
	require.Equal(t, main.RelayCoinbaseMaturity(),
		int32(got.CoinbaseMaturity))
	require.Equal(t, before, main.CoinbaseMaturity,
		"the caller's params were changed")
	require.Equal(t, main.Name, got.Name)
	require.Equal(t, main.GenesisHash, got.GenesisHash)

	// A network without the rule keeps its params as they are.
	reg := &chaincfg.RegressionNetParams
	if !reg.CoinbaseMaturityLongDeployed() {
		require.Same(t, reg, walletChainParams(reg))
	}

	require.Nil(t, walletChainParams(nil))
}
