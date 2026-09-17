package lnd

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/stretchr/testify/require"
)

// TestCheckBackupChain pins that a channel backup written for another chain
// is refused before any of it is mapped into the database.
func TestCheckBackupChain(t *testing.T) {
	ours := chainreg.BitcoinMainNetParams.ChainHash

	// Since 2026-09-17 this chain advertises the genesis hash it shares
	// with the chain that did not upgrade, so a mainnet genesis hash is
	// ours rather than foreign. What keeps that chain's channels out is
	// option_unified_sigs in channel_type, not this comparison.
	require.Equal(t, *chaincfg.MainNetParams.GenesisHash, ours)

	// Another network is still foreign.
	foreign := *chaincfg.TestNet3Params.GenesisHash

	require.NoError(t, checkBackupChain(ours))
	require.NoError(t, checkBackupChain(ours,
		chanbackup.Single{ChainHash: ours},
		chanbackup.Single{ChainHash: ours},
	))

	err := checkBackupChain(ours,
		chanbackup.Single{ChainHash: ours},
		chanbackup.Single{ChainHash: foreign},
	)
	require.Error(t, err)
	var wrong *ErrBackupWrongChain
	require.True(t, errors.As(err, &wrong), err)
	require.Equal(t, foreign, wrong.Backup)
	require.Equal(t, ours, wrong.Ours)
	require.Contains(t, err.Error(), "BLAKE2b")

	// A zero chain hash (a backup from a build that never set one) is
	// refused too.
	require.Error(t, checkBackupChain(ours, chanbackup.Single{}))
	_ = chainhash.Hash{}
}
