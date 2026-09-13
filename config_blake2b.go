package lnd

import (
	"fmt"
	"path/filepath"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcwallet/wallet/txauthor"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/zpay32"
)

// applyBlake2bChainConfig applies the Bitcoin BLAKE2b chain-identity options
// to the active network parameters once the network has been selected, and
// registers the invoice prefix for it. It returns an error for combinations
// that would let the daemon run against a chain it cannot identify.
func applyBlake2bChainConfig(cfg *Config) error {
	params := &cfg.ActiveNetParams

	if cfg.Bitcoin.Blake2bActivationHeight != 0 {
		if cfg.Bitcoin.MainNet {
			return fmt.Errorf("bitcoin.blake2b-activation-height cannot " +
				"be set on mainnet: the activation height is fixed " +
				"at 961640")
		}
		params.Blake2bActivationHeight = cfg.Bitcoin.Blake2bActivationHeight
	}

	if cfg.Bitcoin.ChainHashOverride != "" {
		if !cfg.Bitcoin.RegTest {
			return fmt.Errorf("bitcoin.chain-hash-override is only " +
				"accepted on regtest")
		}
		h, err := chainreg.ParseChainHashOverride(
			cfg.Bitcoin.ChainHashOverride,
		)
		if err != nil {
			return fmt.Errorf("bitcoin.chain-hash-override: %w", err)
		}
		params.ChainHash = h
	}

	// A local network chooses its activation height per run, so the daemon
	// has no way to know it unless told. Refuse to guess; the only
	// exception is the integration build, which may follow a SHA256d
	// regtest for upstream's own tests.
	if cfg.Bitcoin.Node == bitcoindBackendName &&
		cfg.Bitcoin.IsLocalNetwork() &&
		params.Blake2bActivationHeight == 0 &&
		!chainreg.AllowSHA256Regtest() {

		return fmt.Errorf("bitcoin.blake2b-activation-height is required "+
			"on %s: set it to the height the node activates BLAKE2b "+
			"at (its -testactivationheight=blake2b@N)",
			cfg.ActiveNetParams.Name)
	}

	// The status file is what a wrapper reads to show the outcome of the
	// chain-identity check, so its location must not depend on the
	// working directory the daemon happened to start in.
	if cfg.Bitcoin.ChainIdentityFile != "" {
		path := CleanAndExpandPath(cfg.Bitcoin.ChainIdentityFile)
		if !filepath.IsAbs(path) {
			return fmt.Errorf("bitcoin.chain-identity-file must be an "+
				"absolute path, got %q", cfg.Bitcoin.ChainIdentityFile)
		}
		cfg.Bitcoin.ChainIdentityFile = path
	}

	// Replay protection for everything this node signs alone. Mainnet
	// has no off switch: a sweep or on-chain send made without the opt-in
	// is a valid SHA256d transaction spending the same pre-fork coins.
	if cfg.Bitcoin.NoUnifiedSigHash && cfg.Bitcoin.MainNet {
		return fmt.Errorf("bitcoin.no-unified-sighash cannot be set on " +
			"mainnet")
	}
	// A development or integration build never opts in: it runs upstream's
	// tests against a stock SHA256d regtest.
	applyUnifiedSigHash(
		!cfg.Bitcoin.NoUnifiedSigHash && input.DefaultUnifiedSigHash(),
	)
	input.SetAllowLegacySigHash(cfg.Bitcoin.AllowLegacySigHash)

	zpay32.RegisterInvoiceHRP(params.Name, params.InvoiceHRP)

	return nil
}

// applyUnifiedSigHash switches the opt-in for every signing path the node
// uses alone: the sign descriptors it builds (through input.SoleSignerSigHash)
// and the wallet library's own on-chain sends, which choose their hash types
// from package-level settings.
func applyUnifiedSigHash(enabled bool) {
	input.SetUnifiedSigHash(enabled)
	if enabled {
		txauthor.WitnessSigHashType = txscript.SigHashAll |
			txscript.SigHashUnified
		txauthor.TaprootSigHashType = txscript.SigHashAll |
			txscript.SigHashUnified
		txauthor.LegacySigHashType = txscript.SigHashAll |
			txscript.SigHashUnified
		return
	}
	txauthor.WitnessSigHashType = txscript.SigHashAll
	txauthor.TaprootSigHashType = txscript.SigHashDefault
	txauthor.LegacySigHashType = txscript.SigHashAll
}
