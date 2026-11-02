package btcwallet

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestCoinbaseMaturity checks the depth the wallet holds a coinbase for: on
// mainnet 6480 blocks for one mined before 979919 and 25920 from it (Bitcoin
// Knots 29.4.3), and the ordinary 100 on a network without those rules.
func TestCoinbaseMaturity(t *testing.T) {
	t.Parallel()

	main := &chaincfg.MainNetParams
	require.EqualValues(t, 6480, coinbaseMaturity(main, 961640))
	require.EqualValues(t, 6480, coinbaseMaturity(main, 979918))
	require.EqualValues(t, 25920, coinbaseMaturity(main, 979919))
	require.EqualValues(t, 25920, coinbaseMaturity(main, 1_000_000))

	require.EqualValues(t, 100, coinbaseMaturity(
		&chaincfg.RegressionNetParams, 979919,
	))
}
