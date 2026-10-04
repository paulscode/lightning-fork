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

// The SHA256 chain's block at the mainnet activation height, pinned as the
// BLAKE2b one is (chainreg): mempool.space and blockstream.info agree on it,
// and its 80-byte header hashes to it. Not the BLAKE2b block is not enough
// on mainnet: a node on a third chain (a BIP 110 Knots 29.4, which follows
// neither, say) is not the BLAKE2b chain either, and is no SHA256 node.
const sha256MainnetActivationHeight = 961640

var sha256MainnetActivationHash = mustHash(
	"00000000000000000001d82da6ecccf08e07afa383f9212b0e1b95cc72430c00",
)

func mustHash(s string) chainhash.Hash {
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		panic(err)
	}

	return *h
}

// CheckNotBlake2b refuses a SHA256 node that follows the BLAKE2b chain.
//
// A stock lnd reads blocks from whatever node it is given and checks neither
// proof of work nor which side of the fork the node took: the two chains share
// their genesis block, network name and address format. Pointed at a node
// that follows BLAKE2b (Bitcoin Knots from 29.4.1, say), it syncs, gives out
// addresses and opens channels on the wrong chain, and the bridge would pay
// SHA256 invoices from nothing.
//
// The block at the activation height tells them apart: on the SHA256 chain it
// is not this node's block there. That comparison is the check that matters;
// a stock lnd re-serialises every header it reads as 80 bytes, so the header
// test below (80 bytes, hashing to the reported id) only catches a node that
// reports BLAKE2b data some other way, and costs one call.
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
	if strict && height == sha256MainnetActivationHeight &&
		!got.IsEqual(&blake2bHash) &&
		!got.IsEqual(&sha256MainnetActivationHash) {

		return fmt.Errorf("%w: the SHA256 node follows neither chain (its "+
			"block %d is %v, which is neither the SHA256 chain's nor "+
			"the BLAKE2b chain's). Its chain backend must be a node on "+
			"the SHA256 chain, such as Bitcoin Core or a Bitcoin Knots "+
			"before 29.4", ErrConfig, height, got)
	}
	if got.IsEqual(&blake2bHash) {
		return fmt.Errorf("%w: the SHA256 node follows the BLAKE2b "+
			"chain (its block %d is %v, this node's). Its chain "+
			"backend must be a node on the SHA256 chain, such as "+
			"Bitcoin Core or a Bitcoin Knots before 29.4",
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
	if len(raw) != sha256HeaderSize {
		return fmt.Errorf("%w: the SHA256 node's block %d is not a "+
			"SHA256 chain block (a %d byte header). Its chain "+
			"backend must be a node on the SHA256 chain", ErrConfig,
			height, len(raw))
	}
	if !bytes.Equal(id[:], got[:]) {
		return fmt.Errorf("%w: the SHA256 node's block %d is not a "+
			"SHA256 chain block (its header does not hash to its "+
			"id). Its chain backend must be a node on the SHA256 "+
			"chain", ErrConfig, height)
	}

	return nil
}
