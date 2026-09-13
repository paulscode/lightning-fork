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
	bitcoin := *chaincfg.MainNetParams.GenesisHash

	require.NoError(t, checkBackupChain(ours))
	require.NoError(t, checkBackupChain(ours,
		chanbackup.Single{ChainHash: ours},
		chanbackup.Single{ChainHash: ours},
	))

	err := checkBackupChain(ours,
		chanbackup.Single{ChainHash: ours},
		chanbackup.Single{ChainHash: bitcoin},
	)
	require.Error(t, err)
	var wrong *ErrBackupWrongChain
	require.True(t, errors.As(err, &wrong), err)
	require.Equal(t, bitcoin, wrong.Backup)
	require.Equal(t, ours, wrong.Ours)
	require.Contains(t, err.Error(), "BLAKE2b")

	// A zero chain hash (a backup from a build that never set one) is
	// refused too.
	require.Error(t, checkBackupChain(ours, chanbackup.Single{}))
	_ = chainhash.Hash{}
}
