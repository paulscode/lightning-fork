package chainreg

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

// TestBlake2bChainIdentity pins the constants that identify the Bitcoin
// BLAKE2b chain at the Lightning layer.
func TestBlake2bChainIdentity(t *testing.T) {
	// The mainnet chain hash is the first BLAKE2b block, and it is not the
	// genesis hash.
	require.Equal(t,
		"0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb",
		BitcoinMainNetParams.ChainHash.String())
	require.Equal(t, *Blake2bMainnetActivationHash, BitcoinMainNetParams.ChainHash)
	require.NotEqual(t, *chaincfg.MainNetParams.GenesisHash,
		BitcoinMainNetParams.ChainHash)
	require.Equal(t, uint32(961640), BitcoinMainNetParams.Blake2bActivationHeight)
	require.NotNil(t, BitcoinMainNetParams.Blake2bActivationHash)

	// The genesis hash itself is untouched: the wallet backend still
	// identifies the node by it.
	require.Equal(t, *chaincfg.MainNetParams.GenesisHash,
		*BitcoinMainNetParams.Params.GenesisHash)
	require.Equal(t, chaincfg.MainNetParams.Bech32HRPSegwit,
		BitcoinMainNetParams.Params.Bech32HRPSegwit)

	all := []BitcoinNetParams{
		BitcoinMainNetParams, BitcoinTestNetParams, BitcoinTestNet4Params,
		BitcoinSimNetParams, BitcoinSigNetParams, BitcoinRegTestNetParams,
	}
	seen := map[chainhash.Hash]string{}
	for _, p := range all {
		// Every network has a chain hash that differs from its genesis
		// and from every other network's.
		require.NotEqual(t, *p.Params.GenesisHash, p.ChainHash, p.Name)
		require.NotEqual(t, chainhash.Hash{}, p.ChainHash, p.Name)
		if prev, dup := seen[p.ChainHash]; dup {
			t.Fatalf("%s and %s share a chain hash", prev, p.Name)
		}
		seen[p.ChainHash] = p.Name

		// Invoice prefixes are letters only (the amount begins at the
		// first digit), never a stock Bitcoin prefix, and never start
		// with "bc" (visually confusable with lnbc...).
		require.NotEmpty(t, p.InvoiceHRP, p.Name)
		for _, r := range p.InvoiceHRP {
			require.True(t, r >= 'a' && r <= 'z', "%s: %q", p.Name, p.InvoiceHRP)
		}
		for _, stock := range []string{"bc", "tb", "tbs", "bcrt", "sb"} {
			require.NotEqual(t, stock, p.InvoiceHRP, p.Name)
		}
		require.False(t, strings.HasPrefix(p.InvoiceHRP, "bc"), p.Name)
	}

	// Test networks are not pinned to an activation block.
	for _, p := range all[1:] {
		require.Nil(t, p.Blake2bActivationHash, p.Name)
		require.Zero(t, p.Blake2bActivationHeight, p.Name)
	}
}

func TestSyntheticChainHash(t *testing.T) {
	// Deterministic, tagged, and not a plain hash of the genesis.
	a := SyntheticChainHash(chaincfg.RegressionNetParams.GenesisHash)
	b := SyntheticChainHash(chaincfg.RegressionNetParams.GenesisHash)
	require.Equal(t, a, b)
	require.NotEqual(t, chainhash.HashH(chaincfg.RegressionNetParams.GenesisHash[:]), a)
	require.NotEqual(t, chainhash.DoubleHashH(chaincfg.RegressionNetParams.GenesisHash[:]), a)
	require.Equal(t,
		*chainhash.TaggedHash([]byte("Lightning Fork chain_hash"),
			chaincfg.RegressionNetParams.GenesisHash[:]),
		a)
}

func TestParseChainHashOverride(t *testing.T) {
	h, err := ParseChainHashOverride(
		"0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb")
	require.NoError(t, err)
	require.Equal(t, *Blake2bMainnetActivationHash, h)

	for _, bad := range []string{"", "abc", strings.Repeat("g", 64),
		strings.Repeat("0", 63)} {
		_, err := ParseChainHashOverride(bad)
		require.Error(t, err, bad)
	}
}
