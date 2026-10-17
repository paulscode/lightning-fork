package migration30

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/shachain"
)

// TestMigrateRevocationLogIndependentStore checks that the revocation log
// migration, which can also be run later as an optional migration, still
// succeeds when the database holds a channel with option_independent_secrets.
// Such a channel has no legacy revocation logs, but the migration decodes the
// revocation state of every channel, and its store isn't a shachain.
func TestMigrateRevocationLogIndependentStore(t *testing.T) {
	legacyChan, independentChan := createTwoChannels()

	store := shachain.NewIndependentStore()
	if err := store.AddNextEntry(&chainhash.Hash{1}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddNextEntry(&chainhash.Hash{2}); err != nil {
		t.Fatal(err)
	}
	independentChan.RevocationStore = store

	beforeMigration := func(db kvdb.Backend) error {
		if err := createNotStarted(db, legacyChan, true); err != nil {
			return err
		}

		return setupTestLogs(db, independentChan, nil, nil)
	}

	afterMigration := func(db kvdb.Backend) error {
		assertOldLogBucketDeleted(t, db, legacyChan)
		return nil
	}

	migtest.ApplyMigrationWithDB(
		t, beforeMigration, afterMigration,
		func(db kvdb.Backend) error {
			return MigrateRevocationLog(
				db, &MigrateRevLogConfigImpl{},
			)
		},
		false,
	)
}
