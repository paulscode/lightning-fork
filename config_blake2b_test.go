package lnd

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

func blake2bTestConfig(t *testing.T, params chainreg.BitcoinNetParams,
	set func(*Config)) *Config {

	t.Helper()
	cfg := DefaultConfig()
	cfg.ActiveNetParams = params
	cfg.Bitcoin.MainNet = params.Name == chaincfg.MainNetParams.Name
	cfg.Bitcoin.RegTest = params.Name == chaincfg.RegressionNetParams.Name
	cfg.Bitcoin.SimNet = params.Name == chaincfg.SimNetParams.Name
	cfg.Bitcoin.TestNet4 = params.Name == chaincfg.TestNet4Params.Name
	cfg.Bitcoin.Node = bitcoindBackendName
	if set != nil {
		set(&cfg)
	}
	t.Cleanup(func() { zpay32.RegisterInvoiceHRP(params.Name, "") })
	return &cfg
}

// TestApplyBlake2bChainConfig covers the network/option combinations the
// chain-identity configuration accepts and refuses.
func TestApplyBlake2bChainConfig(t *testing.T) {
	t.Run("mainnet defaults", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams, nil)
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.Equal(t, uint32(961640),
			cfg.ActiveNetParams.Blake2bActivationHeight)
		// chain_hash is the genesis hash shared with the chain that
		// did not upgrade; the activation hash is what the startup
		// check reads, not what is advertised.
		require.Equal(t, *chaincfg.MainNetParams.GenesisHash,
			cfg.ActiveNetParams.ChainHash)
		require.NotEqual(t, *chainreg.Blake2bMainnetActivationHash,
			cfg.ActiveNetParams.ChainHash)
		require.Equal(t, "blake", zpay32.InvoiceHRP(&chaincfg.MainNetParams))
	})

	t.Run("mainnet refuses height override", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) { c.Bitcoin.Blake2bActivationHeight = 1 })
		err := applyBlake2bChainConfig(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "mainnet")
	})

	t.Run("mainnet refuses chain hash override", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) {
				c.Bitcoin.ChainHashOverride = chainreg.BitcoinMainNetParams.
					ChainHash.String()
			})
		err := applyBlake2bChainConfig(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "regtest")
	})

	t.Run("regtest requires activation height", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinRegTestNetParams, nil)
		err := applyBlake2bChainConfig(cfg)
		if chainreg.AllowSHA256Regtest() {
			require.NoError(t, err)
			return
		}
		require.Error(t, err)
		require.Contains(t, err.Error(), "blake2b-activation-height")
	})

	t.Run("regtest with height and override", func(t *testing.T) {
		override := chainreg.SyntheticChainHash(
			chaincfg.MainNetParams.GenesisHash,
		)
		cfg := blake2bTestConfig(t, chainreg.BitcoinRegTestNetParams,
			func(c *Config) {
				c.Bitcoin.Blake2bActivationHeight = 20
				c.Bitcoin.ChainHashOverride = override.String()
			})
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.Equal(t, uint32(20),
			cfg.ActiveNetParams.Blake2bActivationHeight)
		require.Equal(t, override, cfg.ActiveNetParams.ChainHash)
		require.Equal(t, "blakert",
			zpay32.InvoiceHRP(&chaincfg.RegressionNetParams))
	})

	t.Run("regtest rejects malformed override", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinRegTestNetParams,
			func(c *Config) {
				c.Bitcoin.Blake2bActivationHeight = 20
				c.Bitcoin.ChainHashOverride = "nothex"
			})
		require.Error(t, applyBlake2bChainConfig(cfg))
	})

	t.Run("regtest without chain backend needs no height", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinRegTestNetParams,
			func(c *Config) { c.Bitcoin.Node = "nochainbackend" })
		require.NoError(t, applyBlake2bChainConfig(cfg))
	})

	t.Run("testnet4 height override accepted", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinTestNet4Params,
			func(c *Config) { c.Bitcoin.Blake2bActivationHeight = 150308 })
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.Equal(t, uint32(150308),
			cfg.ActiveNetParams.Blake2bActivationHeight)
		require.Equal(t, "tblake", zpay32.InvoiceHRP(&chaincfg.TestNet4Params))
	})

	t.Run("peers without networks are refused by default", func(t *testing.T) {
		require.False(t, DefaultConfig().AllowPeersWithoutNetworks)
	})
}

// TestBackendChoicesRefused pins that the btcd and neutrino backends are
// rejected at configuration time with a message naming the reason, and that
// bitcoind on mainnet gets past that check.
func TestBackendChoicesRefused(t *testing.T) {
	validate := func(t *testing.T, node string) error {
		t.Helper()
		cfg := DefaultConfig()
		cfg.LndDir = t.TempDir()
		cfg.Bitcoin.Node = node
		cfg.Bitcoin.MainNet = true
		fileParser := flags.NewParser(&cfg, flags.Default)
		flagParser := flags.NewParser(&cfg, flags.Default)
		_, err := ValidateConfig(
			cfg, signal.Interceptor{}, fileParser, flagParser,
		)
		return err
	}

	for _, node := range []string{btcdBackendName, neutrinoBackendName} {
		err := validate(t, node)
		require.Error(t, err, node)
		require.Contains(t, err.Error(), "not supported", node)
		require.Contains(t, err.Error(), "BLAKE2b", node)
	}

	// bitcoind is accepted by the backend switch; whatever fails later
	// (RPC credentials are not configured here) must not be that message.
	if err := validate(t, bitcoindBackendName); err != nil {
		require.NotContains(t, err.Error(), "not supported")
	}
}

// TestBlake2bChainIdentityFileOption: the status-file override is expanded
// and must be absolute, so a wrapper always finds it where it was told.
func TestBlake2bChainIdentityFileOption(t *testing.T) {
	t.Run("absent leaves the default", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams, nil)
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.Empty(t, cfg.Bitcoin.ChainIdentityFile)
	})

	t.Run("absolute path kept", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) {
				c.Bitcoin.ChainIdentityFile = "/status//chain-identity.json"
			})
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.Equal(t, "/status/chain-identity.json",
			cfg.Bitcoin.ChainIdentityFile)
	})

	t.Run("home expanded", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) {
				c.Bitcoin.ChainIdentityFile = "~/status.json"
			})
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.True(t, filepath.IsAbs(cfg.Bitcoin.ChainIdentityFile))
		require.NotContains(t, cfg.Bitcoin.ChainIdentityFile, "~")
	})

	t.Run("relative path refused", func(t *testing.T) {
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) {
				c.Bitcoin.ChainIdentityFile = "status/chain-identity.json"
			})
		err := applyBlake2bChainConfig(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "absolute")
	})
}

// Silent peers are kept by default. The chains are separated by
// option_blake2b, an even feature bit a node on the other chain must
// disconnect on, and refusing every peer that omits an optional BOLT 1 field
// as well costs more than it protects: it drops client applications that speak
// the wire protocol only to reach a node's RPC.
//
// This used to be the other way round, when the odd form of option_blake2b
// separated nothing and this heuristic was carrying the separation on its own.
func TestSilentPeersAreKeptByDefault(t *testing.T) {
	t.Parallel()

	cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams, nil)
	require.NoError(t, applyBlake2bChainConfig(cfg))
	require.False(t, cfg.RequirePeerNetworks,
		"a peer that sends no networks list is refused by default; "+
			"that is a heuristic standing in for option_blake2b")
}

// An operator who wants the old behaviour can still have it, and asking for
// both at once is asking for opposite things.
func TestPeerNetworksOptionsThatContradict(t *testing.T) {
	t.Parallel()

	t.Run("the strict option alone is accepted", func(t *testing.T) {
		t.Parallel()

		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) { c.RequirePeerNetworks = true })
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.True(t, cfg.RequirePeerNetworks)
	})

	t.Run("the deprecated option alone still starts", func(t *testing.T) {
		t.Parallel()

		// A configuration written before the change must not stop the
		// node from booting. It now asks for what already happens.
		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) { c.AllowPeersWithoutNetworks = true })
		require.NoError(t, applyBlake2bChainConfig(cfg))
		require.False(t, cfg.RequirePeerNetworks)
	})

	t.Run("both together are refused", func(t *testing.T) {
		t.Parallel()

		cfg := blake2bTestConfig(t, chainreg.BitcoinMainNetParams,
			func(c *Config) {
				c.AllowPeersWithoutNetworks = true
				c.RequirePeerNetworks = true
			})
		err := applyBlake2bChainConfig(cfg)
		require.Error(t, err, "opposite requests were accepted, and "+
			"one of them was silently ignored")
		require.Contains(t, err.Error(), "contradict")
	})
}
