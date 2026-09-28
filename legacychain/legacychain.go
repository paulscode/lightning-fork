// Package legacychain names the chain_hash this daemon advertised on each
// network until 2026-09-17, when the Bitcoin BLAKE2b chain went back to the
// genesis hash it shares with Bitcoin.
//
// Releases up to v0.21.3-beta-blake2b.9 signed gossip, and stored state, under
// those values. channeldb migrations 36 and 37 move the stored state. What
// cannot be moved is a signature: a channel_announcement or channel_update
// made before the change commits to the old value, and nobody can re-sign it
// but the nodes that made it. So a reader has to know the old value to check
// one, which is what this package is for.
//
// It is a leaf on purpose. netann needs it and chainreg depends on netann, so
// the values are written out here rather than derived from chainreg; a test
// re-derives them.
package legacychain

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// pairs maps each network's current chain_hash, its genesis hash, to the one
// it replaced. Hashes are in the order getblockhash prints them.
var pairs = mustPairs([][2]string{
	// mainnet: the id of block 961,640, the first BLAKE2b block.
	{
		"000000000019d6689c085ae165831e934ff763ae46a2a6c172b3f1b60a8ce26f",
		"0000000000000050c1e5f69672f459293be14f46e5a494e7a8c8541396f18eeb",
	},
	// The test networks used TaggedHash("Lightning Fork chain_hash",
	// genesis), having no fixed activation block.
	// testnet4.
	{
		"00000000da84f2bafbbc53dee25a72ae507ff4914b867c565be350b0da8bf043",
		"572c94664c77fb4ce6a9c4ee50ed8f0eb1bd363061342194ac66fca694aa63a6",
	},
	// signet.
	{
		"00000008819873e925422c1ff0f99f7cc9bbb232af63a077a480a3633bee1ef6",
		"c283e28a744edd1bf7a47946620de35ae2e8ac84dc0a2b21e29f1ebf36ec6589",
	},
	// simnet.
	{
		"683e86bd5c6d110d91b94b97137ba6bfe02dbbdb8e3dff722a669b5d69d77af6",
		"b3ed44c85ec33da494089f280e551145119918a571577c4ed4acaec258411a5e",
	},
	// regtest.
	{
		"0f9188f13cb7b2c71f2a335e3a4fc328bf5beb436012afca590b1a11466e2206",
		"2594d57b43169a2856ded0623840f0863b9e967b936f6f3d7945da28d909ab1a",
	},
})

func mustPairs(in [][2]string) map[chainhash.Hash]chainhash.Hash {
	out := make(map[chainhash.Hash]chainhash.Hash, len(in))
	for _, p := range in {
		current, err := chainhash.NewHashFromStr(p[0])
		if err != nil {
			panic(err)
		}
		legacy, err := chainhash.NewHashFromStr(p[1])
		if err != nil {
			panic(err)
		}
		out[*current] = *legacy
	}

	return out
}

// For returns the chain_hash that the given current one replaced, if it
// replaced one.
func For(current chainhash.Hash) (chainhash.Hash, bool) {
	legacy, ok := pairs[current]

	return legacy, ok
}
