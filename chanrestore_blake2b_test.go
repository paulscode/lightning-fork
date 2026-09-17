package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/stretchr/testify/require"
)

// Until 2026-09-17 this chain advertised a chain_hash of its own, and backups
// written by that build carry it. Refusing those after the change would mean
// an operator who upgrades cannot restore from a backup taken the day before,
// which is the moment a backup is most likely to be wanted.
func TestBackupsFromBeforeTheChainHashChangeAreAccepted(t *testing.T) {
	t.Parallel()

	mainnet := chainreg.BitcoinMainNetParams.ChainHash
	regtest := chainreg.BitcoinRegTestNetParams.ChainHash

	tests := []struct {
		name   string
		ours   chainhash.Hash
		backup chainhash.Hash
		ok     bool
	}{
		{
			name: "what this build writes",
			ours: mainnet, backup: mainnet, ok: true,
		},
		{
			// Mainnet's old chain_hash was the id of the first
			// BLAKE2b block.
			name:   "mainnet, written before the change",
			ours:   mainnet,
			backup: *chainreg.Blake2bMainnetActivationHash,
			ok:     true,
		},
		{
			// Every other network used a tagged genesis instead,
			// so the two legacy forms are different values and
			// both have to be recognised.
			name: "regtest, written before the change",
			ours: regtest,
			backup: chainreg.SyntheticChainHash(
				chaincfg.RegressionNetParams.GenesisHash,
			),
			ok: true,
		},
		{
			name:   "a backup for another network entirely",
			ours:   mainnet,
			backup: *chaincfg.TestNet3Params.GenesisHash,
		},
		{
			// The legacy form is per network. Accepting mainnet's
			// everywhere would be laxer than the rule it replaces,
			// and would restore a mainnet channel onto a regtest
			// node.
			name:   "mainnet's legacy hash is refused on regtest",
			ours:   regtest,
			backup: *chainreg.Blake2bMainnetActivationHash,
		},
		{
			// The converse: the tagged form was never what mainnet
			// advertised, so no mainnet backup carries it.
			name: "the tagged form is refused on mainnet",
			ours: mainnet,
			backup: chainreg.SyntheticChainHash(
				chaincfg.MainNetParams.GenesisHash,
			),
		},
		{
			name: "a backup naming nothing",
			ours: mainnet, backup: chainhash.Hash{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			err := checkBackupChain(test.ours, chanbackup.Single{
				ChainHash: test.backup,
			})
			if test.ok {
				require.NoError(t, err, "a restorable backup "+
					"was refused, which loses the channels "+
					"in it")

				return
			}
			require.Error(t, err)
		})
	}
}

// The legacy forms must not be a way in for the chain that did not upgrade.
// That chain never advertised either of them, but the check is worth pinning:
// its backups carry the genesis hash, which is now also ours, and what keeps
// those channels out is option_unified_sigs rather than this comparison.
func TestTheSharedGenesisIsNotTreatedAsForeign(t *testing.T) {
	t.Parallel()

	ours := chainreg.BitcoinMainNetParams.ChainHash

	require.Equal(t, *chaincfg.MainNetParams.GenesisHash, ours,
		"this chain advertises the genesis hash it shares with the "+
			"chain that did not upgrade; if that stops being true "+
			"the reasoning in checkBackupChain needs revisiting")

	require.NoError(t, checkBackupChain(ours, chanbackup.Single{
		ChainHash: *chaincfg.MainNetParams.GenesisHash,
	}))
}
