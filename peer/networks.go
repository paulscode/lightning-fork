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
		"is %v (require-peer-networks is set, and sending the list is "+
		"optional in BOLT 1, so this refuses peers that simply do not "+
		"send it; option_blake2b is what separates the chains)", e.Ours)
}

// checkPeerNetworks decides whether a peer's BOLT 1 `networks` list is
// compatible with the chain this node runs on.
//
// A peer that lists chains and does not list ours is always refused. That is
// the BOLT 1 rule and it separates this node from a chain that is neither of
// the two here; it does not separate the two, because Bitcoin and Bitcoin
// BLAKE2b share a genesis block and therefore send the same chain hash.
//
// Silence is a different question and is not refused by default. It used to
// be, on the reasoning that LND never sent the list so a silent peer was
// probably a stock LND node on the SHA256d chain. That was a heuristic
// standing in for a mechanism, and it drops anything that simply does not
// send an optional field, including client applications that speak the wire
// protocol only to reach a node's RPC. The mechanism is option_blake2b, an
// even feature bit: a node on the other chain must disconnect on seeing it,
// by BOLT 1's own rule, whatever it sends in its networks list. required
// restores the old behaviour for an operator who wants it.
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
