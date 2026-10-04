//go:build bridgerpc
// +build bridgerpc

package lnd

import (
	"fmt"
	"path/filepath"

	"github.com/lightningnetwork/lnd/lncfg"
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

	if sub.Supervised {
		if err := superviseSha256Node(cfg, sub, networkDir); err != nil {
			return err
		}
	}

	if sub.SHA256TLSCertPath != "" {
		sub.SHA256TLSCertPath = CleanAndExpandPath(
			sub.SHA256TLSCertPath,
		)
	}
	if sub.SHA256MacaroonPath != "" {
		sub.SHA256MacaroonPath = CleanAndExpandPath(
			sub.SHA256MacaroonPath,
		)
	}

	return sub.Validate()
}

// superviseSha256Node fills in where the supervised SHA256 node and this node's
// files for it are.
//
// The address, certificate and macaroon default to that node's own. The address
// and certificate may be named, for a platform that runs the node elsewhere
// (Umbrel gives it an address of its own); the macaroon may not: it is one this
// node bakes, and a path pointing elsewhere would have the bridge use a
// credential it did not make for a node it did not create.
func superviseSha256Node(cfg *Config, sub *bridgerpc.Config,
	networkDir string) error {

	if sub.SHA256MacaroonPath != "" {
		return fmt.Errorf("%w: bridgerpc.sha256.macaroonpath is for an "+
			"SHA256 node you already run; with "+
			"bridgerpc.sha256.supervised this node makes the "+
			"macaroon itself, so leave it out", bridgerpc.ErrConfig)
	}

	if sub.SHA256Dir == "" {
		sub.SHA256Dir = filepath.Join(
			cfg.LndDir, bridgerpc.DefaultSha256DirName,
		)
	}
	sub.SHA256Dir = CleanAndExpandPath(sub.SHA256Dir)

	if sub.SHA256RPCHost == "" {
		sub.SHA256RPCHost = bridgerpc.DefaultSupervisedRPCHost
	}
	if sub.SHA256TLSCertPath == "" {
		sub.SHA256TLSCertPath = filepath.Join(sub.SHA256Dir, "tls.cert")
	}

	secrets := filepath.Join(networkDir, bridgerpc.Sha256SecretsDir)
	sub.SHA256PasswordFile = filepath.Join(
		secrets, bridgerpc.Sha256PasswordName,
	)
	sub.SHA256MacaroonPath = filepath.Join(
		secrets, bridgerpc.Sha256MacaroonName,
	)
	sub.SHA256OperatorMacaroonPath = filepath.Join(
		secrets, bridgerpc.Sha256OperatorMacaroonName,
	)

	// The stock node keeps its macaroons under its own network directory,
	// named as lnd names networks, which is the same naming this node's
	// directories use.
	sub.SHA256AdminMacaroonPath = filepath.Join(
		sub.SHA256Dir, "data", "chain", "bitcoin",
		lncfg.NormalizeNetwork(cfg.ActiveNetParams.Name),
		"admin.macaroon",
	)

	return nil
}
