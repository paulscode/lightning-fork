package peer

import (
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// ErrPeerNoCommonChain is returned when a peer's init message lists networks
// but none of them is the chain this node runs on.
type ErrPeerNoCommonChain struct {
	Ours   chainhash.Hash
	Theirs []chainhash.Hash
}

// Error implements the error interface.
func (e *ErrPeerNoCommonChain) Error() string {
	return fmt.Sprintf("peer advertises no common chain: ours is %v, "+
		"peer listed %v", e.Ours, e.Theirs)
}

// ErrPeerNetworksMissing is returned when a peer's init message carries no
// networks list and this node requires one.
type ErrPeerNetworksMissing struct {
	Ours chainhash.Hash
}

// Error implements the error interface.
func (e *ErrPeerNetworksMissing) Error() string {
	return fmt.Sprintf("peer did not advertise the chains it serves; ours "+
		"is %v (a peer that sends no networks is most likely a Bitcoin "+
		"SHA256d node sharing our genesis block; "+
		"allow-peers-without-networks keeps such peers)", e.Ours)
}

// checkPeerNetworks decides whether a peer's BOLT 1 `networks` list is
// compatible with the chain this node runs on.
//
// Bitcoin and Bitcoin BLAKE2b share a genesis block, so the chain hash in
// open_channel and gossip is the only protocol-level difference between a
// peer on the other chain and one of ours, and by then a connection and its
// gossip exchange are already under way. The init `networks` list is the one
// place to tell them apart at the handshake. Core Lightning sends it; LND
// does not, so a silent peer is most likely a stock LND node on the SHA256d
// chain. When required is set, silence is refused too.
func checkPeerNetworks(theirs []chainhash.Hash, ours chainhash.Hash,
	required bool) error {

	if len(theirs) == 0 {
		if required {
			return &ErrPeerNetworksMissing{Ours: ours}
		}
		return nil
	}

	for _, h := range theirs {
		if h == ours {
			return nil
		}
	}

	return &ErrPeerNoCommonChain{Ours: ours, Theirs: theirs}
}
