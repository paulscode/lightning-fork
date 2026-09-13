package chainreg

import (
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightningnetwork/lnd/lncfg"
)

// chainIdentityStatusPath returns where the chain-identity status file is
// written: the configured path when there is one, otherwise next to
// channel.backup in the network's chain directory.
func chainIdentityStatusPath(cfg *Config) string {
	if cfg.Bitcoin.ChainIdentityFile != "" {
		return cfg.Bitcoin.ChainIdentityFile
	}

	return ChainIdentityStatusPath(
		cfg.Bitcoin.ChainDir,
		lncfg.NormalizeNetwork(cfg.ActiveNetParams.Name),
	)
}

// verifyBlake2bBackend runs the chain-identity check against the bitcoind
// backend before it is used, then keeps re-checking in the background until
// quit is closed. It returns an error when the node is not on the Bitcoin
// BLAKE2b chain, or cannot be shown to be.
func verifyBlake2bBackend(rpc *rpcclient.Client, cfg *Config,
	quit <-chan struct{}) error {

	params := cfg.ActiveNetParams
	statusPath := chainIdentityStatusPath(cfg)

	// Development builds may follow a SHA256d regtest so that upstream's
	// integration tests keep running. Say so loudly; a release build
	// never takes this path.
	if params.Blake2bActivationHeight == 0 && cfg.Bitcoin.IsLocalNetwork() &&
		AllowSHA256Regtest() {

		log.Warnf("No BLAKE2b activation height configured on %s and "+
			"this is an integration build: skipping the chain-identity "+
			"check and following whatever chain the node serves",
			params.Name)
		if err := WriteChainIdentityStatus(statusPath, ChainIdentityStatus{
			State:     ChainIdentitySkipped,
			Reason:    "integration build on a local network without an activation height",
			Network:   params.Name,
			ChainHash: params.ChainHash.String(),
		}); err != nil {
			log.Warnf("Unable to write %s: %v", statusPath, err)
		}
		return nil
	}

	checker := newChainIdentityChecker(rpc, params, statusPath, log)
	height, err := checker.run(quit)
	if err != nil {
		return err
	}

	onMismatch := cfg.OnChainMismatch
	if onMismatch == nil {
		onMismatch = func(err error) {
			log.Criticalf("No shutdown hook registered for a chain "+
				"mismatch: %v", err)
		}
	}
	go checker.watch(
		height, DefaultChainIdentityRecheckInterval, quit, onMismatch,
	)

	return nil
}
