// Package migration37 moves the rest of what was stored under the chain_hash
// this daemon used to advertise.
//
// Migration 36 moved the channels themselves: openChannelBucket and
// close-summaries. It missed three other places that key or record the chain
// hash, all of which a node upgrading from v0.21.3-beta-blake2b.9 or earlier
// still holds under the old value:
//
//   - The chain arbitrator's log, one top-level bucket per channel named
//     chainHash || chanPoint. It is where a channel that is closing keeps its
//     contract resolutions. Left behind, a channel that was mid force close at
//     the upgrade is found with no resolutions, and nothing sweeps its outputs
//     or claims its HTLCs: an incoming HTLC whose preimage this node holds
//     times out back to the peer.
//   - The UTXO nursery's root bucket, "utxn" || chainHash, which holds outputs
//     still waiting out a timelock.
//   - Each edge in the channel graph, whose record repeats the chain hash.
//     Every channel_update this node signs is built from it, and every
//     upgraded node, this one included, drops an update that names the old
//     value. Our own channels could then never be updated or disabled.
//
// It is a separate migration rather than a fix to 36 because 36 has shipped:
// a database a build with 36 has opened is at version 36 and would never run
// a changed 36.
//
// As in 36, nothing is guessed. A bucket moves only when its key is built
// from one of the specific values below, and a record is rewritten only when
// the bytes being replaced are already the old value.
package migration37

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// nurseryPrefix prefixes the chain hash to name the nursery's root
	// bucket.
	nurseryPrefix = []byte("utxn")

	// graphEdgeBucket holds the edge index.
	graphEdgeBucket = []byte("graph-edge")

	// edgeIndexBucket maps a short channel id to the edge's record.
	edgeIndexBucket = []byte("edge-index")
)

const (
	// arbLogKeyLen is the length of an arbitrator log's bucket key: a
	// chain hash and an outpoint (txid and 4 byte index).
	arbLogKeyLen = chainhash.HashSize + chainhash.HashSize + 4
)

// chainHashChange is one network's move from the chain_hash this daemon used
// to advertise to the one it advertises now.
type chainHashChange struct {
	legacy  chainhash.Hash
	current chainhash.Hash
}

// changes is the same table as migration 36's, written out again so that
// this migration keeps behaving the same way whatever happens to that one.
// Hashes are in the order getblockhash prints them.
var changes = mustChanges([][2]string{
	// mainnet: the id of block 961,640, the first BLAKE2b block.
	{
		"0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb",
		"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
	},
	// testnet4.
	{
		"572c94664c77fb4ce6a9c4ee50ed8f0eb1bd363061342194ac66fca694aa63a6",
		"00000000da84f2bafbbc53dee25a72ae507ff4914b867c565be350b0da8bf043",
	},
	// signet.
	{
		"c283e28a744edd1bf7a47946620de35ae2e8ac84dc0a2b21e29f1ebf36ec6589",
		"00000008819873e925422c1ff0f99f7cc9bbb232af63a077a480a3633bee1ef6",
	},
	// simnet.
	{
		"b3ed44c85ec33da494089f280e551145119918a571577c4ed4acaec258411a5e",
		"683e86bd5c6d110d91b94b97137ba6bfe02dbbdb8e3dff722a669b5d69d77af6",
	},
	// regtest.
	{
		"2594d57b43169a2856ded0623840f0863b9e967b936f6f3d7945da28d909ab1a",
		"0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206",
	},
})

func mustChanges(pairs [][2]string) []chainHashChange {
	out := make([]chainHashChange, 0, len(pairs))
	for _, p := range pairs {
		legacy, err := chainhash.NewHashFromStr(p[0])
		if err != nil {
			panic(err)
		}
		current, err := chainhash.NewHashFromStr(p[1])
		if err != nil {
			panic(err)
		}
		out = append(out, chainHashChange{*legacy, *current})
	}

	return out
}

// changeFor returns the move for a legacy chain hash, if it is one.
func changeFor(hash []byte) (chainHashChange, bool) {
	for _, c := range changes {
		if bytes.Equal(hash, c.legacy[:]) {
			return c, true
		}
	}

	return chainHashChange{}, false
}

// MigrateChainHashState moves the arbitrator logs and the nursery onto the
// current chain hash, and rewrites the chain hash each graph edge records.
//
// It is a no-op on a database that never held anything under an old value.
func MigrateChainHashState(tx kvdb.RwTx) error {
	log.Infof("Migrating closing channel state and the channel graph " +
		"onto the chain hash this node now advertises")

	logs, err := migrateTopLevel(tx, arbLogMove)
	if err != nil {
		return fmt.Errorf("arbitrator logs: %w", err)
	}

	nursery, err := migrateTopLevel(tx, nurseryMove)
	if err != nil {
		return fmt.Errorf("nursery: %w", err)
	}

	edges, err := migrateGraphEdges(tx)
	if err != nil {
		return fmt.Errorf("graph edges: %w", err)
	}

	if logs == 0 && nursery == 0 && edges == 0 {
		log.Infof("Nothing stored under a previous chain hash, " +
			"nothing to do")

		return nil
	}

	log.Infof("Moved %d arbitrator log(s) and %d nursery bucket(s), and "+
		"rewrote %d graph edge(s), onto the current chain hash", logs,
		nursery, edges)

	return nil
}

// arbLogMove returns the new name of an arbitrator log's bucket, if key is
// one under a legacy chain hash.
func arbLogMove(key []byte) ([]byte, bool) {
	if len(key) != arbLogKeyLen {
		return nil, false
	}

	change, ok := changeFor(key[:chainhash.HashSize])
	if !ok {
		return nil, false
	}

	moved := make([]byte, len(key))
	copy(moved, change.current[:])
	copy(moved[chainhash.HashSize:], key[chainhash.HashSize:])

	return moved, true
}

// nurseryMove returns the new name of the nursery's root bucket, if key is
// the one under a legacy chain hash.
func nurseryMove(key []byte) ([]byte, bool) {
	if len(key) != len(nurseryPrefix)+chainhash.HashSize ||
		!bytes.HasPrefix(key, nurseryPrefix) {

		return nil, false
	}

	change, ok := changeFor(key[len(nurseryPrefix):])
	if !ok {
		return nil, false
	}

	moved := make([]byte, 0, len(key))
	moved = append(moved, nurseryPrefix...)
	moved = append(moved, change.current[:]...)

	return moved, true
}

// migrateTopLevel renames every top-level bucket for which rename returns a
// new name, returning how many it moved.
//
// A destination that already exists is only possible on a database a build
// with migration 36 but not this one has run, which may have started a fresh
// log for the same channel. The two are merged when no key collides. When one
// does, the old bucket is left where it is and a warning says so: that is
// what every such build did anyway, and refusing to start would be worse.
func migrateTopLevel(tx kvdb.RwTx,
	rename func([]byte) ([]byte, bool)) (int, error) {

	type move struct {
		from, to []byte
	}
	var todo []move

	err := tx.ForEachBucket(func(key []byte) error {
		if to, ok := rename(key); ok {
			todo = append(todo, move{
				from: append([]byte(nil), key...),
				to:   to,
			})
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	var moved int
	for _, m := range todo {
		src := tx.ReadWriteBucket(m.from)
		if src == nil {
			return 0, fmt.Errorf("bucket %x vanished mid migration",
				m.from)
		}

		dst := tx.ReadWriteBucket(m.to)
		if dst != nil {
			clash, err := collides(src, dst)
			if err != nil {
				return 0, err
			}
			if clash {
				log.Warnf("Bucket %x exists under both the old "+
					"and the current chain hash with "+
					"overlapping contents; leaving the old "+
					"one in place", m.from)

				continue
			}
		} else {
			dst, err = tx.CreateTopLevelBucket(m.to)
			if err != nil {
				return 0, err
			}
		}

		if err := copyBucket(src, dst); err != nil {
			return 0, err
		}

		if err := tx.DeleteTopLevelBucket(m.from); err != nil {
			return 0, fmt.Errorf("could not remove bucket %x: %w",
				m.from, err)
		}

		moved++
	}

	return moved, nil
}

// collides reports whether any key or nested bucket of src is also in dst.
func collides(src, dst kvdb.RBucket) (bool, error) {
	var clash bool
	err := src.ForEach(func(k, v []byte) error {
		if clash {
			return nil
		}

		if v != nil {
			clash = dst.Get(k) != nil || dst.NestedReadBucket(k) != nil

			return nil
		}

		if dst.Get(k) != nil {
			clash = true

			return nil
		}

		dstSub := dst.NestedReadBucket(k)
		if dstSub == nil {
			return nil
		}

		srcSub := src.NestedReadBucket(k)
		if srcSub == nil {
			return nil
		}

		sub, err := collides(srcSub, dstSub)
		clash = sub

		return err
	})

	return clash, err
}

// copyBucket copies every key and nested bucket of src into dst. The caller
// has checked there is nothing to overwrite.
func copyBucket(src kvdb.RwBucket, dst kvdb.RwBucket) error {
	return src.ForEach(func(k, v []byte) error {
		if v != nil {
			return dst.Put(k, v)
		}

		srcSub := src.NestedReadWriteBucket(k)
		if srcSub == nil {
			return nil
		}

		dstSub, err := dst.CreateBucketIfNotExists(k)
		if err != nil {
			return err
		}

		return copyBucket(srcSub, dstSub)
	})
}

// migrateGraphEdges rewrites the chain hash in each edge record that holds a
// legacy one, returning how many it rewrote.
//
// The graph is a cache of the network's gossip, not a record of funds, so an
// edge this cannot read is left alone rather than failing the upgrade.
func migrateGraphEdges(tx kvdb.RwTx) (int, error) {
	edges := tx.ReadWriteBucket(graphEdgeBucket)
	if edges == nil {
		return 0, nil
	}

	index := edges.NestedReadWriteBucket(edgeIndexBucket)
	if index == nil {
		return 0, nil
	}

	type patch struct {
		key, value []byte
	}
	var (
		todo       []patch
		unreadable int
	)

	err := index.ForEach(func(k, v []byte) error {
		if v == nil {
			return nil
		}

		offset, err := chainHashOffset(v)
		if err != nil {
			unreadable++

			return nil
		}

		change, ok := changeFor(v[offset : offset+chainhash.HashSize])
		if !ok {
			return nil
		}

		patched := make([]byte, len(v))
		copy(patched, v)
		copy(patched[offset:], change.current[:])
		todo = append(todo, patch{
			key:   append([]byte(nil), k...),
			value: patched,
		})

		return nil
	})
	if err != nil {
		return 0, err
	}

	if unreadable > 0 {
		log.Warnf("Left %d graph edge(s) that could not be read as "+
			"they stand", unreadable)
	}

	for _, p := range todo {
		if err := index.Put(p.key, p.value); err != nil {
			return 0, err
		}
	}

	return len(todo), nil
}

// chainHashOffset returns where the chain hash starts in an edge record.
//
// The record is four 33 byte keys, the features and four signatures as
// variable length byte strings, the channel point (32 byte txid and 4 byte
// index), the capacity and the short channel id as 8 byte integers, and then
// the chain hash. Only the variable length parts have to be read to find it.
func chainHashOffset(record []byte) (int, error) {
	r := bytes.NewReader(record)

	if _, err := r.Seek(4*33, io.SeekStart); err != nil {
		return 0, err
	}

	// The features, then the four signatures.
	for i := 0; i < 5; i++ {
		n, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return 0, err
		}
		if n > uint64(r.Len()) {
			return 0, io.ErrUnexpectedEOF
		}
		if _, err := r.Seek(int64(n), io.SeekCurrent); err != nil {
			return 0, err
		}
	}

	// Channel point, capacity and short channel id.
	const fixed = chainhash.HashSize + 4 + 8 + 8
	offset := len(record) - r.Len() + fixed
	if offset+chainhash.HashSize > len(record) {
		return 0, io.ErrUnexpectedEOF
	}

	return offset, nil
}
