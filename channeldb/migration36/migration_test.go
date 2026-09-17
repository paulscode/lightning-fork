package migration36

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// chainHashTag is the tag the withdrawn design used to derive a chain_hash for
// the networks that have no fixed activation block. Copied rather than
// imported for the reason the migration gives about its constants.
const chainHashTag = "Lightning Fork chain_hash"

// The values this migration matches against a user's database are written out
// rather than derived, so that they cannot drift with a helper and so that
// they can be read. This re-derives every one of them and fails if a digit
// ever moves.
func TestChangesAreTheKnownDerivations(t *testing.T) {
	t.Parallel()

	// mainnet took the id of the first BLAKE2b block; everything else took
	// a tagged hash of its own genesis.
	const mainnetActivation = "0000000000000050c1e5f69672f459293be14f46" +
		"e5a494e7a8c8541396f18eeb"

	want := []struct {
		name   string
		params *chaincfg.Params
		legacy func(*chaincfg.Params) chainhash.Hash
	}{
		{"mainnet", &chaincfg.MainNetParams, func(*chaincfg.Params) chainhash.Hash {
			h, err := chainhash.NewHashFromStr(mainnetActivation)
			require.NoError(t, err)

			return *h
		}},
		{"testnet4", &chaincfg.TestNet4Params, tagged},
		{"signet", &chaincfg.SigNetParams, tagged},
		{"simnet", &chaincfg.SimNetParams, tagged},
		{"regtest", &chaincfg.RegressionNetParams, tagged},
	}

	require.Len(t, changes, len(want), "a network was added or removed "+
		"without updating this test")

	for i, w := range want {
		require.Equal(t, w.legacy(w.params), changes[i].legacy,
			"%v: the legacy chain hash is not the one the "+
				"withdrawn design produced", w.name)
		require.Equal(t, *w.params.GenesisHash, changes[i].current,
			"%v: the current chain hash is not the genesis hash",
			w.name)
	}
}

func tagged(p *chaincfg.Params) chainhash.Hash {
	return *chainhash.TaggedHash([]byte(chainHashTag), p.GenesisHash[:])
}

// chanInfo builds the start of a chan-info record: the channel type as a
// variable length integer, the chain hash, and some trailing bytes standing in
// for the rest of the record, which this migration must leave alone.
func chanInfo(t *testing.T, chanType uint64, chain chainhash.Hash,
	tail string) []byte {

	t.Helper()

	var w bytes.Buffer
	var buf [8]byte
	require.NoError(t, tlv.WriteVarInt(&w, chanType, &buf))
	_, err := w.Write(chain[:])
	require.NoError(t, err)
	_, err = w.WriteString(tail)
	require.NoError(t, err)

	return w.Bytes()
}

// regtest is the network the fixtures below use.
var regtest = changes[4]

// A node with channels under the withdrawn chain hash must come out with the
// same channels under the current one, with nothing else touched.
func TestChannelsMoveToTheCurrentChainHash(t *testing.T) {
	t.Parallel()

	node := []byte("node-pubkey")
	chanPoint := []byte("channel-point")
	const tail = "the rest of the record"

	before := func(tx kvdb.RwTx) error {
		open, err := tx.CreateTopLevelBucket(openChannelBucket)
		if err != nil {
			return err
		}
		nodeB, err := open.CreateBucket(node)
		if err != nil {
			return err
		}
		chainB, err := nodeB.CreateBucket(regtest.legacy[:])
		if err != nil {
			return err
		}
		chanB, err := chainB.CreateBucket(chanPoint)
		if err != nil {
			return err
		}
		err = chanB.Put(chanInfoKey, chanInfo(
			t, 1<<13, regtest.legacy, tail,
		))
		if err != nil {
			return err
		}

		// A nested bucket inside the channel, as the real layout has,
		// to prove the copy goes all the way down.
		sub, err := chanB.CreateBucket([]byte("revocation-log"))
		if err != nil {
			return err
		}

		return sub.Put([]byte("k"), []byte("v"))
	}

	after := func(tx kvdb.RwTx) error {
		open := tx.ReadWriteBucket(openChannelBucket)
		require.NotNil(t, open)

		nodeB := open.NestedReadWriteBucket(node)
		require.NotNil(t, nodeB)

		require.Nil(t, nodeB.NestedReadBucket(regtest.legacy[:]),
			"the old chain hash bucket is still there")

		chainB := nodeB.NestedReadWriteBucket(regtest.current[:])
		require.NotNil(t, chainB, "the channel was not moved to the "+
			"current chain hash")

		chanB := chainB.NestedReadWriteBucket(chanPoint)
		require.NotNil(t, chanB)

		// The recorded chain hash is the new one, the channel type in
		// front of it is untouched, and so is everything after it.
		require.Equal(t, chanInfo(t, 1<<13, regtest.current, tail),
			chanB.Get(chanInfoKey))

		sub := chanB.NestedReadBucket([]byte("revocation-log"))
		require.NotNil(t, sub, "a nested bucket was lost")
		require.Equal(t, []byte("v"), sub.Get([]byte("k")))

		return nil
	}

	migtest.ApplyMigration(t, before, after, MigrateChainHash, false)
}

// Every database that never held such a channel must come out untouched, which
// is every node created since the change and every node that never opened a
// channel before it.
func TestNothingToDoIsLeftAlone(t *testing.T) {
	t.Parallel()

	node := []byte("node-pubkey")
	current := chanInfo(t, 1, regtest.current, "untouched")

	before := func(tx kvdb.RwTx) error {
		open, err := tx.CreateTopLevelBucket(openChannelBucket)
		if err != nil {
			return err
		}
		nodeB, err := open.CreateBucket(node)
		if err != nil {
			return err
		}
		chainB, err := nodeB.CreateBucket(regtest.current[:])
		if err != nil {
			return err
		}
		chanB, err := chainB.CreateBucket([]byte("cp"))
		if err != nil {
			return err
		}

		return chanB.Put(chanInfoKey, current)
	}

	after := func(tx kvdb.RwTx) error {
		chanB := tx.ReadWriteBucket(openChannelBucket).
			NestedReadWriteBucket(node).
			NestedReadWriteBucket(regtest.current[:]).
			NestedReadWriteBucket([]byte("cp"))
		require.NotNil(t, chanB)
		require.Equal(t, current, chanB.Get(chanInfoKey))

		return nil
	}

	migtest.ApplyMigration(t, before, after, MigrateChainHash, false)
}

// An empty database is the commonest case of all and must not error.
func TestEmptyDatabase(t *testing.T) {
	t.Parallel()

	migtest.ApplyMigration(t,
		func(kvdb.RwTx) error { return nil },
		func(kvdb.RwTx) error { return nil },
		MigrateChainHash, false,
	)
}

// The resolver reports of closed channels are keyed by chain hash one level
// higher up and hold no copy of it, so they move without being rewritten.
func TestCloseSummariesMove(t *testing.T) {
	t.Parallel()

	before := func(tx kvdb.RwTx) error {
		closed, err := tx.CreateTopLevelBucket(closeSummaryBucket)
		if err != nil {
			return err
		}
		chainB, err := closed.CreateBucket(regtest.legacy[:])
		if err != nil {
			return err
		}
		chanB, err := chainB.CreateBucket([]byte("cp"))
		if err != nil {
			return err
		}

		return chanB.Put([]byte("report"), []byte("body"))
	}

	after := func(tx kvdb.RwTx) error {
		closed := tx.ReadWriteBucket(closeSummaryBucket)
		require.Nil(t, closed.NestedReadBucket(regtest.legacy[:]))

		chanB := closed.NestedReadWriteBucket(regtest.current[:]).
			NestedReadWriteBucket([]byte("cp"))
		require.NotNil(t, chanB)
		require.Equal(t, []byte("body"), chanB.Get([]byte("report")))

		return nil
	}

	migtest.ApplyMigration(t, before, after, MigrateChainHash, false)
}

// A channel whose recorded chain hash is not the one the bucket says it is
// under is not the record this migration believes it is. Stopping leaves the
// database as it was, because the whole migration is one transaction; carrying
// on would write 32 bytes into the middle of something unknown.
func TestARecordThatDoesNotMatchStopsTheMigration(t *testing.T) {
	t.Parallel()

	before := func(tx kvdb.RwTx) error {
		open, err := tx.CreateTopLevelBucket(openChannelBucket)
		if err != nil {
			return err
		}
		nodeB, err := open.CreateBucket([]byte("node"))
		if err != nil {
			return err
		}
		chainB, err := nodeB.CreateBucket(regtest.legacy[:])
		if err != nil {
			return err
		}
		chanB, err := chainB.CreateBucket([]byte("cp"))
		if err != nil {
			return err
		}

		// Stored under the legacy hash, but recording a different one.
		return chanB.Put(chanInfoKey, chanInfo(
			t, 1, changes[0].legacy, "tail",
		))
	}

	migtest.ApplyMigration(t, before,
		func(kvdb.RwTx) error { return nil },
		MigrateChainHash, true,
	)
}

// Both buckets existing at once is a database this migration has not seen
// before. Merging one over the other could lose a channel, so it refuses.
func TestBothBucketsPresentIsRefused(t *testing.T) {
	t.Parallel()

	before := func(tx kvdb.RwTx) error {
		open, err := tx.CreateTopLevelBucket(openChannelBucket)
		if err != nil {
			return err
		}
		nodeB, err := open.CreateBucket([]byte("node"))
		if err != nil {
			return err
		}

		for _, h := range [][]byte{
			regtest.legacy[:], regtest.current[:],
		} {
			chainB, err := nodeB.CreateBucket(h)
			if err != nil {
				return err
			}
			chanB, err := chainB.CreateBucket([]byte("same-point"))
			if err != nil {
				return err
			}
			err = chanB.Put(chanInfoKey, chanInfo(
				t, 1, regtest.legacy, "tail",
			))
			if err != nil {
				return err
			}
		}

		return nil
	}

	// Asserted on the message rather than only on "it failed". Removing
	// the explicit collision check still fails this, because CreateBucket
	// refuses an existing bucket on its own, and a test that cannot tell
	// those apart would pass while the error an operator sees became an
	// unexplained "bucket already exists".
	cdb, err := migtest.MakeDB(t)
	require.NoError(t, err)
	require.NoError(t, kvdb.Update(cdb, before, func() {}))

	err = kvdb.Update(cdb, MigrateChainHash, func() {})
	require.Error(t, err)
	require.Contains(t, err.Error(),
		"exists under both the old and the new chain hash")
}

// A peer with channels on more than one legacy network in the same database is
// not a real deployment, but the migration should carry each to its own
// destination rather than confusing them.
func TestEachNetworkGoesToItsOwnHash(t *testing.T) {
	t.Parallel()

	node := []byte("node")
	mainnet := changes[0]

	before := func(tx kvdb.RwTx) error {
		open, err := tx.CreateTopLevelBucket(openChannelBucket)
		if err != nil {
			return err
		}
		nodeB, err := open.CreateBucket(node)
		if err != nil {
			return err
		}

		for _, c := range []chainHashChange{regtest, mainnet} {
			chainB, err := nodeB.CreateBucket(c.legacy[:])
			if err != nil {
				return err
			}
			chanB, err := chainB.CreateBucket([]byte("cp"))
			if err != nil {
				return err
			}
			err = chanB.Put(chanInfoKey, chanInfo(t, 1, c.legacy, "t"))
			if err != nil {
				return err
			}
		}

		return nil
	}

	after := func(tx kvdb.RwTx) error {
		nodeB := tx.ReadWriteBucket(openChannelBucket).
			NestedReadWriteBucket(node)

		for _, c := range []chainHashChange{regtest, mainnet} {
			require.Nil(t, nodeB.NestedReadBucket(c.legacy[:]))

			chanB := nodeB.NestedReadWriteBucket(c.current[:]).
				NestedReadWriteBucket([]byte("cp"))
			require.NotNil(t, chanB)
			require.Equal(t, chanInfo(t, 1, c.current, "t"),
				chanB.Get(chanInfoKey))
		}

		return nil
	}

	migtest.ApplyMigration(t, before, after, MigrateChainHash, false)
}

// Running it twice must be the same as running it once. A migration that fails
// half way is re-run against a database it has already partly changed.
func TestRunningItTwiceChangesNothingMore(t *testing.T) {
	t.Parallel()

	node := []byte("node")

	before := func(tx kvdb.RwTx) error {
		open, err := tx.CreateTopLevelBucket(openChannelBucket)
		if err != nil {
			return err
		}
		nodeB, err := open.CreateBucket(node)
		if err != nil {
			return err
		}
		chainB, err := nodeB.CreateBucket(regtest.legacy[:])
		if err != nil {
			return err
		}
		chanB, err := chainB.CreateBucket([]byte("cp"))
		if err != nil {
			return err
		}

		return chanB.Put(chanInfoKey, chanInfo(t, 1, regtest.legacy, "t"))
	}

	twice := func(tx kvdb.RwTx) error {
		if err := MigrateChainHash(tx); err != nil {
			return err
		}

		return MigrateChainHash(tx)
	}

	after := func(tx kvdb.RwTx) error {
		chanB := tx.ReadWriteBucket(openChannelBucket).
			NestedReadWriteBucket(node).
			NestedReadWriteBucket(regtest.current[:]).
			NestedReadWriteBucket([]byte("cp"))
		require.NotNil(t, chanB)
		require.Equal(t, chanInfo(t, 1, regtest.current, "t"),
			chanB.Get(chanInfoKey))

		return nil
	}

	migtest.ApplyMigration(t, before, after, twice, false)
}
