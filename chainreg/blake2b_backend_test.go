package chainreg

import (
	"path/filepath"
	"testing"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/stretchr/testify/require"
)

// TestChainIdentityStatusPathSelection: the status file lives next to
// channel.backup unless the operator moved it.
func TestChainIdentityStatusPathSelection(t *testing.T) {
	cfg := &Config{
		Bitcoin:         &lncfg.Chain{ChainDir: "/data/chain/bitcoin"},
		ActiveNetParams: BitcoinMainNetParams,
	}
	require.Equal(t,
		filepath.Join("/data/chain/bitcoin", "mainnet", ChainIdentityFileName),
		chainIdentityStatusPath(cfg))

	cfg.Bitcoin.ChainIdentityFile = "/status/chain-identity.json"
	require.Equal(t, "/status/chain-identity.json",
		chainIdentityStatusPath(cfg))

	// The network name is normalized the way the chain directory is, so
	// testnet3 aliases land in the same place as the wallet.
	cfg.Bitcoin.ChainIdentityFile = ""
	cfg.ActiveNetParams = BitcoinTestNetParams
	require.Equal(t,
		filepath.Join("/data/chain/bitcoin",
			lncfg.NormalizeNetwork(BitcoinTestNetParams.Name),
			ChainIdentityFileName),
		chainIdentityStatusPath(cfg))
}
