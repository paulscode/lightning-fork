//go:build bridgerpc
// +build bridgerpc

package lnd

import (
	"path/filepath"

	"github.com/lightningnetwork/lnd/lnrpc/bridgerpc"
)

// validateBridgeConfig fills in the bridge's derived defaults and refuses a
// configuration that would not serve swaps.
//
// It runs at startup rather than at the first quote, because the failure it
// catches is silent: a bridge whose numbers are collectively incoherent accepts
// its configuration, starts, and then refuses every swap for a reason that
// appears nowhere except in the arithmetic. An operator should learn that from
// the daemon refusing to start, with the knob to turn named.
func validateBridgeConfig(cfg *Config, networkDir string) error {
	sub := cfg.SubRPCServers.BridgeRPC
	if sub == nil {
		return nil
	}

	if sub.Journal == "" {
		// Under the network directory so that whatever backs this node
		// up takes the journal with it. It records swaps in flight,
		// including their preimages, and losing it while one is running
		// loses the record of an HTLC that is still out there.
		sub.Journal = filepath.Join(
			networkDir, bridgerpc.DefaultJournalName,
		)
	}
	sub.Journal = CleanAndExpandPath(sub.Journal)

	if sub.BitcoinTLSCertPath != "" {
		sub.BitcoinTLSCertPath = CleanAndExpandPath(
			sub.BitcoinTLSCertPath,
		)
	}
	if sub.BitcoinMacaroonPath != "" {
		sub.BitcoinMacaroonPath = CleanAndExpandPath(
			sub.BitcoinMacaroonPath,
		)
	}

	return sub.Validate()
}
