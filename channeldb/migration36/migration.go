// Package migration36 moves channels out of the chain_hash this daemon used
// to advertise and into the genesis hash it advertises now.
//
// Until 2026-09-17 the Bitcoin BLAKE2b chain was given a chain_hash of its
// own: the id of the first BLAKE2b block on mainnet, and a tagged hash of the
// genesis block on the test networks. That was withdrawn in favour of keeping
// the genesis hash both chains share and separating them elsewhere, in the
// init feature bit, the gossip height floor, the channel type and the invoice
// prefix.
//
// lnd keys channels by chain hash, so a node that opened channels under the
// old value cannot start against a build using the new one: the server's
// access-control pass finds a node bucket with no sub-bucket for the chain it
// is running on and fails with "no chain bucket exists". Without this
// migration the operator's only ways out are to stay on a build that can no
// longer peer with anything, or to restore from a static channel backup, which
// force-closes every channel by design. Both cost real money, so the daemon
// absorbs the change instead.
//
// Two things move:
//
//   - openChannelBucket/<node>/<chainHash>/... , whose bucket key is the chain
//     hash, and whose per-channel chan-info record repeats it.
//   - close-summaries/<chainHash>/... , whose bucket key is the chain hash and
//     which holds no copy of it.
//
// One thing deliberately does not. A ChannelCloseSummary carries a ChainHash
// field, written from the channel's own at the moment it closed. Those are
// historical records of channels that are already closed, nothing reads them
// to decide anything, and every future close takes its value from the channel
// state this migration fixes. Patching them would mean rewriting a record with
// optional trailing fields for no gain.
package migration36

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// openChannelBucket is the bucket that holds a sub-bucket per peer,
	// each of which holds a sub-bucket per chain.
	openChannelBucket = []byte("open-chan-bucket")

	// closeSummaryBucket is the top level bucket holding resolver reports
	// for closed channels, keyed by chain hash.
	closeSummaryBucket = []byte("close-summaries")

	// chanInfoKey is the key within a channel's bucket holding the record
	// that begins with the channel type and the chain hash.
	chanInfoKey = []byte("chan-info-key")
)

// chainHashChange is one network's move from the chain_hash this daemon used
// to advertise to the one it advertises now.
type chainHashChange struct {
	legacy  chainhash.Hash
	current chainhash.Hash
}

// changes is every value this daemon ever advertised as a chain_hash, paired
// with what replaced it.
//
// Written out rather than derived, for two reasons. A migration has to keep
// behaving the same way forever, and deriving these would tie it to helpers
// that are free to change. And these are the values to be matched against a
// user's database, so they are worth being able to read. TestChangesAreTheKnown
// Derivations re-derives them and fails if they ever drift.
//
// Hashes are given in the order getblockhash prints them, which is the reverse
// of the order they are stored in.
var changes = mustChanges([][2]string{
	// mainnet: the id of block 961,640, the first BLAKE2b block.
	{
		"0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb",
		"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
	},
	// The test networks used TaggedHash("Lightning Fork chain_hash",
	// genesis) instead, having no fixed activation block.
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

// changeFor returns the move that applies to a bucket key, if any.
func changeFor(key []byte) (chainHashChange, bool) {
	for _, c := range changes {
		if bytes.Equal(key, c.legacy[:]) {
			return c, true
		}
	}

	return chainHashChange{}, false
}

// MigrateChainHash rewrites every channel stored under a chain_hash this
// daemon no longer advertises so that it is stored under the one it does.
//
// It is a no-op on any database that has never held such a channel, which is
// every node created after the change and every node that never opened a
// channel before it. Nothing is guessed: a bucket is moved only when its key
// is one of the specific values above, and a chan-info record is rewritten
// only when the bytes being replaced are already the expected legacy hash.
func MigrateChainHash(tx kvdb.RwTx) error {
	log.Infof("Migrating channels onto the chain hash this node now " +
		"advertises")

	moved, err := migrateOpenChannels(tx)
	if err != nil {
		return err
	}

	reports, err := migrateCloseSummaries(tx)
	if err != nil {
		return err
	}

	if moved == 0 && reports == 0 {
		log.Infof("No channels stored under a previous chain hash, " +
			"nothing to do")

		return nil
	}

	log.Infof("Moved %d channel(s) and %d resolver report bucket(s) onto "+
		"the current chain hash", moved, reports)

	return nil
}

// migrateOpenChannels moves each peer's channels from a legacy chain hash
// bucket to the current one, rewriting the chain hash each channel records.
func migrateOpenChannels(tx kvdb.RwTx) (int, error) {
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return 0, nil
	}

	// Collect first, act afterwards. Creating and deleting buckets while
	// iterating the bucket they live in is not something to rely on.
	type work struct {
		node   []byte
		change chainHashChange
	}
	var todo []work

	err := openChanBucket.ForEach(func(node, v []byte) error {
		// A value rather than a nested bucket.
		if v != nil {
			return nil
		}

		nodeBucket := openChanBucket.NestedReadBucket(node)
		if nodeBucket == nil {
			return nil
		}

		return nodeBucket.ForEach(func(chain, v []byte) error {
			if v != nil {
				return nil
			}
			if change, ok := changeFor(chain); ok {
				todo = append(todo, work{
					node:   append([]byte(nil), node...),
					change: change,
				})
			}

			return nil
		})
	})
	if err != nil {
		return 0, err
	}

	var moved int
	for _, w := range todo {
		nodeBucket := openChanBucket.NestedReadWriteBucket(w.node)
		if nodeBucket == nil {
			return 0, fmt.Errorf("node bucket %x vanished mid "+
				"migration", w.node)
		}

		n, err := moveChainBucket(nodeBucket, w.change, true)
		if err != nil {
			return 0, fmt.Errorf("node %x: %w", w.node, err)
		}

		moved += n
	}

	return moved, nil
}

// migrateCloseSummaries does the same for the resolver reports of closed
// channels, which are keyed by chain hash one level higher up and hold no copy
// of it.
func migrateCloseSummaries(tx kvdb.RwTx) (int, error) {
	closeBucket := tx.ReadWriteBucket(closeSummaryBucket)
	if closeBucket == nil {
		return 0, nil
	}

	var todo []chainHashChange
	err := closeBucket.ForEach(func(chain, v []byte) error {
		if v != nil {
			return nil
		}
		if change, ok := changeFor(chain); ok {
			todo = append(todo, change)
		}

		return nil
	})
	if err != nil {
		return 0, err
	}

	var moved int
	for _, change := range todo {
		if _, err := moveChainBucket(closeBucket, change, false); err != nil {
			return 0, err
		}

		moved++
	}

	return moved, nil
}

// moveChainBucket moves parent/<legacy> to parent/<current>, returning how
// many immediate children it carried across. When rewrite is set, each of
// those children is treated as a channel bucket and its recorded chain hash is
// updated too.
func moveChainBucket(parent kvdb.RwBucket, change chainHashChange,
	rewrite bool) (int, error) {

	src := parent.NestedReadWriteBucket(change.legacy[:])
	if src == nil {
		return 0, nil
	}

	dst, err := parent.CreateBucketIfNotExists(change.current[:])
	if err != nil {
		return 0, err
	}

	moved, err := copyBucket(src, dst)
	if err != nil {
		return 0, err
	}

	if rewrite {
		err = dst.ForEach(func(k, v []byte) error {
			if v != nil {
				return nil
			}

			chanBucket := dst.NestedReadWriteBucket(k)
			if chanBucket == nil {
				return nil
			}

			return rewriteChanInfo(chanBucket, change)
		})
		if err != nil {
			return 0, err
		}
	}

	if err := parent.DeleteNestedBucket(change.legacy[:]); err != nil {
		return 0, fmt.Errorf("could not remove the old bucket: %w", err)
	}

	return moved, nil
}

// copyBucket copies every key and nested bucket of src into dst, refusing
// rather than overwriting if a key is already present.
//
// Refusing matters. The only way both buckets can exist is a database this
// migration has not seen before, and silently merging one over the other could
// lose a channel. Stopping leaves the database as it was, since the whole
// migration runs in one transaction.
func copyBucket(src kvdb.RwBucket, dst kvdb.RwBucket) (int, error) {
	var n int
	err := src.ForEach(func(k, v []byte) error {
		if v != nil {
			if dst.Get(k) != nil {
				return fmt.Errorf("key %x exists under both "+
					"the old and the new chain hash", k)
			}

			return dst.Put(k, v)
		}

		srcSub := src.NestedReadWriteBucket(k)
		if srcSub == nil {
			return nil
		}

		if dst.NestedReadBucket(k) != nil {
			return fmt.Errorf("bucket %x exists under both the "+
				"old and the new chain hash", k)
		}

		dstSub, err := dst.CreateBucket(k)
		if err != nil {
			return err
		}

		if _, err := copyBucket(srcSub, dstSub); err != nil {
			return err
		}

		n++

		return nil
	})

	return n, err
}

// rewriteChanInfo replaces the chain hash a channel records with the current
// one.
//
// The record begins with the channel type as a variable length integer and
// then the 32 byte chain hash, so where the hash starts depends on the channel
// type and has to be read rather than assumed. The bytes about to be replaced
// are checked against the legacy hash first: if they are anything else this
// is not the record it is believed to be, and stopping is the only safe
// response.
func rewriteChanInfo(chanBucket kvdb.RwBucket, change chainHashChange) error {
	info := chanBucket.Get(chanInfoKey)
	if info == nil {
		return fmt.Errorf("channel has no %s", chanInfoKey)
	}

	r := bytes.NewReader(info)
	var buf [8]byte
	if _, err := tlv.ReadVarInt(r, &buf); err != nil {
		return fmt.Errorf("could not read the channel type: %w", err)
	}

	start := len(info) - r.Len()
	end := start + chainhash.HashSize
	if end > len(info) {
		return fmt.Errorf("channel record is %d bytes, too short to "+
			"hold a chain hash at offset %d", len(info), start)
	}

	if !bytes.Equal(info[start:end], change.legacy[:]) {
		return fmt.Errorf("channel records %x where the old chain "+
			"hash was expected; refusing to rewrite it",
			info[start:end])
	}

	patched := make([]byte, len(info))
	copy(patched, info)
	copy(patched[start:end], change.current[:])

	return chanBucket.Put(chanInfoKey, patched)
}
