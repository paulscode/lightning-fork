package migration37

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/migtest"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// regtest is the network the fixtures below use.
var regtest = changes[4]

// arbKey is an arbitrator log's bucket key under the given chain hash.
func arbKey(chain chainhash.Hash, op string) []byte {
	key := make([]byte, arbLogKeyLen)
	copy(key, chain[:])
	copy(key[chainhash.HashSize:], op)

	return key
}

// nurseryKey is the nursery's root bucket key under the given chain hash.
func nurseryKey(chain chainhash.Hash) []byte {
	return append(append([]byte(nil), nurseryPrefix...), chain[:]...)
}

// putLog fills a bucket the way the stores do: values, and a nested bucket.
func putLog(t *testing.T, b kvdb.RwBucket, tag string) {
	t.Helper()

	require.NoError(t, b.Put([]byte("state"), []byte(tag)))
	sub, err := b.CreateBucket([]byte("resolvers"))
	require.NoError(t, err)
	require.NoError(t, sub.Put([]byte("r1"), []byte(tag+"-resolver")))
}

// checkLog asserts a bucket holds what putLog wrote.
func checkLog(t *testing.T, b kvdb.RBucket, tag string) {
	t.Helper()

	require.NotNil(t, b)
	require.Equal(t, []byte(tag), b.Get([]byte("state")))
	sub := b.NestedReadBucket([]byte("resolvers"))
	require.NotNil(t, sub, "a nested bucket was lost")
	require.Equal(t, []byte(tag+"-resolver"), sub.Get([]byte("r1")))
}

// A channel mid force close keeps its arbitrator log and its nursery outputs,
// now under the current chain hash, with nothing else touched.
func TestClosingStateMoves(t *testing.T) {
	t.Parallel()

	const op = "a closing channel's outpoint.."
	// A log already under the current hash, for another channel, which
	// must not be touched.
	const other = "another channel's outpoint...."

	before := func(tx kvdb.RwTx) error {
		b, err := tx.CreateTopLevelBucket(arbKey(regtest.legacy, op))
		require.NoError(t, err)
		putLog(t, b, "closing")

		b, err = tx.CreateTopLevelBucket(arbKey(regtest.current, other))
		require.NoError(t, err)
		putLog(t, b, "other")

		b, err = tx.CreateTopLevelBucket(nurseryKey(regtest.legacy))
		require.NoError(t, err)
		putLog(t, b, "nursery")

		// A top-level bucket that happens to be the same length as a
		// log but is not one.
		b, err = tx.CreateTopLevelBucket(make([]byte, arbLogKeyLen))
		require.NoError(t, err)
		putLog(t, b, "unrelated")

		return nil
	}

	after := func(tx kvdb.RwTx) error {
		require.Nil(t, tx.ReadBucket(arbKey(regtest.legacy, op)),
			"the old arbitrator log is still there")
		checkLog(t, tx.ReadBucket(arbKey(regtest.current, op)),
			"closing")
		checkLog(t, tx.ReadBucket(arbKey(regtest.current, other)),
			"other")

		require.Nil(t, tx.ReadBucket(nurseryKey(regtest.legacy)))
		checkLog(t, tx.ReadBucket(nurseryKey(regtest.current)),
			"nursery")

		checkLog(t, tx.ReadBucket(make([]byte, arbLogKeyLen)),
			"unrelated")

		return nil
	}

	migtest.ApplyMigration(t, before, after, MigrateChainHashState, false)
}

// A build with migration 36 but not this one may have started a fresh log
// for the same channel. Without overlap the two are merged; with overlap the
// old one is left, and the migration still succeeds.
func TestExistingDestination(t *testing.T) {
	t.Parallel()

	const merge = "merged channel's outpoint....."
	const clash = "clashing channel's outpoint..."

	before := func(tx kvdb.RwTx) error {
		old, err := tx.CreateTopLevelBucket(arbKey(regtest.legacy, merge))
		require.NoError(t, err)
		putLog(t, old, "old")
		cur, err := tx.CreateTopLevelBucket(arbKey(regtest.current, merge))
		require.NoError(t, err)
		require.NoError(t, cur.Put([]byte("fresh"), []byte("1")))

		old, err = tx.CreateTopLevelBucket(arbKey(regtest.legacy, clash))
		require.NoError(t, err)
		putLog(t, old, "old")
		cur, err = tx.CreateTopLevelBucket(arbKey(regtest.current, clash))
		require.NoError(t, err)
		require.NoError(t, cur.Put([]byte("state"), []byte("new")))

		return nil
	}

	after := func(tx kvdb.RwTx) error {
		require.Nil(t, tx.ReadBucket(arbKey(regtest.legacy, merge)))
		cur := tx.ReadBucket(arbKey(regtest.current, merge))
		checkLog(t, cur, "old")
		require.Equal(t, []byte("1"), cur.Get([]byte("fresh")))

		checkLog(t, tx.ReadBucket(arbKey(regtest.legacy, clash)), "old")
		require.Equal(t, []byte("new"),
			tx.ReadBucket(arbKey(regtest.current, clash)).
				Get([]byte("state")))

		return nil
	}

	migtest.ApplyMigration(t, before, after, MigrateChainHashState, false)
}

// An empty database, or one with nothing under an old value, is left alone.
func TestNothingToDo(t *testing.T) {
	t.Parallel()

	migtest.ApplyMigration(t,
		func(kvdb.RwTx) error { return nil },
		func(kvdb.RwTx) error { return nil },
		MigrateChainHashState, false,
	)
}

// The edge rewrite is checked against the graph store's own encoding rather
// than a hand-built record: the store writes an edge under the old chain
// hash, the migration runs over the same database, and the store reads it back
// with the current hash and every other field as it was.
func TestGraphEdgeRewrite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "cdb")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	store, err := graphdb.NewKVStore(backend)
	require.NoError(t, err)

	vertex := func() route.Vertex {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		v, err := route.NewVertexFromBytes(
			key.PubKey().SerializeCompressed(),
		)
		require.NoError(t, err)

		return v
	}

	// Signatures of different lengths and features, so that the
	// variable-length parts in front of the chain hash are exercised.
	proof := models.NewV1ChannelAuthProof(
		make([]byte, 71), make([]byte, 72), make([]byte, 70),
		make([]byte, 71),
	)
	features := lnwire.NewRawFeatureVector(lnwire.Blake2bRequired)

	edge := func(id uint64, chain chainhash.Hash) *models.ChannelEdgeInfo {
		e, err := models.NewV1Channel(
			id, chain, vertex(), vertex(),
			&models.ChannelV1Fields{
				BitcoinKey1Bytes: vertex(),
				BitcoinKey2Bytes: vertex(),
				ExtraOpaqueData:  []byte{0x01, 0x02, 0x03},
			},
			models.WithChannelPoint(wire.OutPoint{Index: 1}),
			models.WithCapacity(1_000_000),
			models.WithChanProof(proof),
			models.WithFeatures(features),
		)
		require.NoError(t, err)

		return e
	}

	oldEdge := edge(1<<40, regtest.legacy)
	curEdge := edge(2<<40, regtest.current)
	require.NoError(t, store.AddChannelEdge(ctx, oldEdge))
	require.NoError(t, store.AddChannelEdge(ctx, curEdge))

	fetch := func(id uint64) *models.ChannelEdgeInfo {
		info, _, _, err := store.FetchChannelEdgesByID(
			ctx, lnwire.GossipVersion1, id,
		)
		require.NoError(t, err)

		return info
	}
	before := fetch(oldEdge.ChannelID)
	require.Equal(t, regtest.legacy, before.ChainHash)
	untouched := fetch(curEdge.ChannelID)

	migrate := func() {
		require.NoError(t, kvdb.Update(
			backend, MigrateChainHashState, func() {},
		))
	}
	migrate()

	after := fetch(oldEdge.ChannelID)
	require.Equal(t, regtest.current, after.ChainHash)

	after.ChainHash = before.ChainHash
	require.Equal(t, before, after, "a field other than the chain hash "+
		"changed")

	require.Equal(t, untouched, fetch(curEdge.ChannelID))

	// Running again finds nothing to do and changes nothing.
	migrate()
	after = fetch(oldEdge.ChannelID)
	require.Equal(t, regtest.current, after.ChainHash)
}
