package channeldb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/stretchr/testify/require"
)

// dbVersion returns the database's version.
func dbVersion(t *testing.T, db *DB) uint32 {
	t.Helper()

	meta, err := db.FetchMeta()
	require.NoError(t, err)

	return meta.DbVersionNumber
}

// TestIndependentSecretsDBVersion checks that the database moves to the
// independent secrets version when it first stores a channel with
// option_independent_secrets, new or restored, and only then; that a marked
// database still opens, with splicing on or off; and that a newer one is
// refused. An older release refuses a marked database by its own check: the
// version is past the newest one it knows.
func TestIndependentSecretsDBVersion(t *testing.T) {
	t.Parallel()

	require.Equal(t, uint32(independentSecretsDBVersion), LatestDBVersion())
	require.Equal(t, uint32(splicingDBVersion),
		getLatestDBVersion(targetVersions(true)))

	newChannel := func(t *testing.T, cdb *ChannelStateDB,
		independent bool) {

		t.Helper()

		channel := createTestChannelState(t, cdb)
		if independent {
			channel.ChanType |= IndependentSecretsBit
			channel.RevocationStore = shachain.NewIndependentStore()
		}
		require.NoError(t, channel.SyncPending(defaultAddr, 100))
	}

	t.Run("a shachain channel leaves the version", func(t *testing.T) {
		t.Parallel()

		db, err := MakeTestDB(t)
		require.NoError(t, err)
		before := dbVersion(t, db)
		require.Less(t, before, uint32(independentSecretsDBVersion))

		newChannel(t, db.ChannelStateDB(), false)
		require.Equal(t, before, dbVersion(t, db))
	})

	t.Run("an independent secrets channel marks", func(t *testing.T) {
		t.Parallel()

		db, err := MakeTestDB(t)
		require.NoError(t, err)

		newChannel(t, db.ChannelStateDB(), true)
		require.Equal(t, uint32(independentSecretsDBVersion),
			dbVersion(t, db))

		// A second one leaves it there.
		newChannel(t, db.ChannelStateDB(), true)
		require.Equal(t, uint32(independentSecretsDBVersion),
			dbVersion(t, db))
	})

	t.Run("a restored channel marks", func(t *testing.T) {
		t.Parallel()

		db, err := MakeTestDB(t)
		require.NoError(t, err)

		shell, err := genRandomChannelShell()
		require.NoError(t, err)
		shell.Chan.ChanType |= IndependentSecretsBit
		require.NoError(t, db.ChannelStateDB().RestoreChannelShells(
			shell,
		))
		require.Equal(t, uint32(independentSecretsDBVersion),
			dbVersion(t, db))
	})

	t.Run("marked opens, newer refused", func(t *testing.T) {
		t.Parallel()

		for _, splicing := range []bool{false, true} {
			backend, cleanup, err := kvdb.GetTestBackend(
				t.TempDir(), "cdb",
			)
			require.NoError(t, err)
			t.Cleanup(cleanup)

			db, err := CreateWithBackend(
				backend, OptionSplicing(splicing),
			)
			require.NoError(t, err)
			newChannel(t, db.ChannelStateDB(), true)

			// Opening again, with splicing either way, keeps the
			// mark and runs no migration.
			for _, again := range []bool{false, true} {
				v, err := openVersionTestDB(t, backend, again)
				require.NoError(t, err)
				require.Equal(t,
					uint32(independentSecretsDBVersion), v)
			}

			setVersionTestDB(t, backend,
				independentSecretsDBVersion+1)
			_, err = openVersionTestDB(t, backend, splicing)
			require.ErrorIs(t, err, ErrDBReversion)
		}
	})
}
