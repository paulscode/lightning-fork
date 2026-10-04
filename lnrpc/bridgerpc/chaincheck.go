//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// errChainNotYet is a SHA256 node that has not reached the BLAKE2b activation
// height on a network where the chain it follows matters, so which chain it
// follows cannot be told yet. The bridge waits and asks again.
var errChainNotYet = errors.New("the SHA256 node has not reached the " +
	"BLAKE2b activation height yet, so the chain it follows cannot be " +
	"checked; waiting for it to catch up")

// sha256HeaderSize is a block header on the SHA256 chain. The BLAKE2b chain's
// is longer.
const sha256HeaderSize = 80

// CheckNotBlake2b refuses a SHA256 node that follows the BLAKE2b chain.
//
// A stock lnd reads blocks from whatever node it is given and checks neither
// proof of work nor which side of the fork the node took: the two chains share
// their genesis block, network name and address format. Pointed at a node
// that follows BLAKE2b (Bitcoin Knots from 29.4.1, say), it syncs, gives out
// addresses and opens channels on the wrong chain, and the bridge would pay
// SHA256 invoices from nothing.
//
// The block at the activation height tells them apart. On the SHA256 chain it
// is not this node's block there, and its header is the 80 bytes whose double
// SHA256 is the block's id. Both are asked, so a node that answers with
// BLAKE2b data either way is refused.
//
// height and blake2bHash are this node's activation block. A height of zero
// means this network has none (a local test network), and nothing is checked.
// strict says the network holds real money: there a node that cannot answer
// is waited for or refused rather than passed.
func (r *Remote) CheckNotBlake2b(ctx context.Context, height int32,
	blake2bHash chainhash.Hash, strict bool) error {

	if height <= 0 {
		return nil
	}
	if r == nil || r.chain == nil {
		return errors.New("no chain client for the SHA256 node")
	}

	hashResp, err := r.chain.GetBlockHash(
		ctx, &chainrpc.GetBlockHashRequest{BlockHeight: int64(height)},
	)
	switch {
	case status.Code(err) == codes.Unimplemented:
		// A stock lnd built without chainrpc. The official images have
		// it, so the supervised node always does; an operator's own
		// node may not, and refusing it outright would refuse a setup
		// that has been working.
		if strict {
			return fmt.Errorf("%w: the SHA256 node cannot say which "+
				"chain it follows because it was built without "+
				"chainrpc; use a build with it, such as the "+
				"official lnd images", ErrConfig)
		}
		log.Warnf("Bridge cannot check which chain the SHA256 node " +
			"follows: it was built without chainrpc")

		return nil

	case err != nil:
		// Below the activation height lnd answers that the height is
		// out of range, which says only that it has not got there.
		best, bestErr := r.BestBlock(ctx)
		if bestErr == nil && best.Height < height {
			if strict {
				return errChainNotYet
			}

			return nil
		}

		return fmt.Errorf("the SHA256 node's block at height %d: %w",
			height, err)
	}

	got, err := chainhash.NewHash(hashResp.GetBlockHash())
	if err != nil {
		return fmt.Errorf("the SHA256 node's block hash at height %d: "+
			"%w", height, err)
	}
	if got.IsEqual(&blake2bHash) {
		return fmt.Errorf("%w: the SHA256 node follows the BLAKE2b "+
			"chain (its block %d is %v, this node's). Its chain "+
			"backend must be a node on the SHA256 chain, such as "+
			"Bitcoin Core or a Bitcoin Knots before 29.4.1",
			ErrConfig, height, got)
	}

	hdr, err := r.chain.GetBlockHeader(ctx, &chainrpc.GetBlockHeaderRequest{
		BlockHash: hashResp.GetBlockHash(),
	})
	if err != nil {
		return fmt.Errorf("the SHA256 node's header at height %d: %w",
			height, err)
	}
	raw := hdr.GetRawBlockHeader()
	id := chainhash.DoubleHashH(raw)
	if len(raw) != sha256HeaderSize || !bytes.Equal(id[:], got[:]) {
		return fmt.Errorf("%w: the SHA256 node's block %d is not a "+
			"SHA256 chain block (a %d byte header that does not "+
			"hash to its id). Its chain backend must be a node on "+
			"the SHA256 chain", ErrConfig, height, len(raw))
	}

	return nil
}
