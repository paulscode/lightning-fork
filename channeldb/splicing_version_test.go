package channeldb

import (
	"testing"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// openVersionTestDB opens the database on backend, with splicing on or off,
// and returns its version.
func openVersionTestDB(t *testing.T, backend kvdb.Backend,
	splicing bool) (uint32, error) {

	t.Helper()

	db, err := CreateWithBackend(backend, OptionSplicing(splicing))
	if err != nil {
		return 0, err
	}

	meta, err := db.FetchMeta()
	require.NoError(t, err)

	return meta.DbVersionNumber, nil
}

// setVersionTestDB writes version as the database's version.
func setVersionTestDB(t *testing.T, backend kvdb.Backend, version uint32) {
	t.Helper()

	require.NoError(t, kvdb.Update(backend, func(tx kvdb.RwTx) error {
		return putMeta(&Meta{DbVersionNumber: version}, tx)
	}, func() {}))
}

// TestSplicingDBVersion checks that the splicing mark is applied only when
// lnd starts with splicing on, that a marked database still opens with
// splicing off, and that a newer database is refused either way. An older
// release refuses a marked database by its own check: the version is past
// the newest one it knows.
func TestSplicingDBVersion(t *testing.T) {
	t.Parallel()

	require.Equal(t, uint32(splicingDBVersion),
		getLatestDBVersion(targetVersions(true)))
	before := uint32(splicingDBVersion - 1)

	newBackend := func(t *testing.T) kvdb.Backend {
		backend, cleanup, err := kvdb.GetTestBackend(
			t.TempDir(), "cdb",
		)
		require.NoError(t, err)
		t.Cleanup(cleanup)

		return backend
	}

	t.Run("fresh without splicing", func(t *testing.T) {
		t.Parallel()

		v, err := openVersionTestDB(t, newBackend(t), false)
		require.NoError(t, err)
		require.Equal(t, before, v)
	})

	t.Run("fresh with splicing", func(t *testing.T) {
		t.Parallel()

		v, err := openVersionTestDB(t, newBackend(t), true)
		require.NoError(t, err)
		require.Equal(t, uint32(splicingDBVersion), v)
	})

	t.Run("turning splicing on marks", func(t *testing.T) {
		t.Parallel()

		backend := newBackend(t)
		v, err := openVersionTestDB(t, backend, false)
		require.NoError(t, err)
		require.Equal(t, before, v)

		v, err = openVersionTestDB(t, backend, true)
		require.NoError(t, err)
		require.Equal(t, uint32(splicingDBVersion), v)
	})

	t.Run("marked opens with splicing off", func(t *testing.T) {
		t.Parallel()

		backend := newBackend(t)
		_, err := openVersionTestDB(t, backend, true)
		require.NoError(t, err)

		// The mark stays: turning splicing off doesn't make the
		// database safe for an older release again.
		v, err := openVersionTestDB(t, backend, false)
		require.NoError(t, err)
		require.Equal(t, uint32(splicingDBVersion), v)
	})

	t.Run("newer refused", func(t *testing.T) {
		t.Parallel()

		for _, splicing := range []bool{false, true} {
			backend := newBackend(t)
			_, err := openVersionTestDB(t, backend, splicing)
			require.NoError(t, err)

			setVersionTestDB(t, backend, LatestDBVersion()+1)
			_, err = openVersionTestDB(t, backend, splicing)
			require.ErrorIs(t, err, ErrDBReversion)
		}
	})

}
