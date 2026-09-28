package discovery

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/legacychain"
)

// normaliseLegacyChainHash replaces h with current when h is the chain_hash
// this network advertised before the Bitcoin BLAKE2b chain went back to the
// genesis hash, and reports whether it did.
//
// Gossip written before the change can still arrive with the old value:
// channel_updates a node queued for a peer before upgrading are resent from
// its message store on every reconnect until a newer one replaces them. Such
// a message is for this chain, and was signed under the old value, which
// netann's signature check accepts under the current one. Only this network's
// own old value is recognised; it is the id of a BLAKE2b block on mainnet and
// a tagged hash elsewhere, so no node on the chain that did not upgrade has
// ever sent it.
func normaliseLegacyChainHash(h *chainhash.Hash,
	current chainhash.Hash) bool {

	legacy, ok := legacychain.For(current)
	if !ok || *h != legacy {
		return false
	}

	*h = current

	return true
}
