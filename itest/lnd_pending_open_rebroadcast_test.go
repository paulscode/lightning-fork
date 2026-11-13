//go:build integration

package itest

import (
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// testPendingOpenAfterRestart opens a channel whose funding output is output
// 0, stops the funder before the funding transaction confirms and restarts it
// once the transaction has confirmed. On startup the funder republishes the
// funding transaction, and the wallet's rebroadcaster watches for its
// confirmation from the tip, on the same transaction and output the funding
// manager watches from the broadcast height. Whichever registers first, the
// channel must open.
//
// It only exercises the bug with bitcoind without txindex: with a transaction
// index the lookup by txid ignores the height the watch starts at. On the old
// code the channel stayed pending whenever the rebroadcaster registered first
// after the confirmation, which it does in practice since the funding manager
// republishes before it registers its own watch; the restarts give it more
// than one chance.
func testPendingOpenAfterRestart(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)
	bob := ht.NewNode("Bob", nil)

	// One UTXO, spent whole, so the funding transaction has no change
	// output and the funding output is output 0.
	ht.FundCoins(btcutil.SatoshiPerBitcoin/100, alice)
	ht.ConnectNodes(alice, bob)

	pending := ht.OpenChannelAssertPending(alice, bob,
		lntest.OpenChannelParams{FundMax: true},
	)
	require.Zero(ht, pending.OutputIndex, "funding output index")
	ht.AssertNumTxsInMempool(1)

	restart := ht.SuspendNode(alice)

	// Confirm the funding transaction while Alice is down, then a few
	// more blocks so the tip is well past it.
	ht.MineBlocksAndAssertNumTxes(1, 1)
	ht.MineEmptyBlocks(5)

	require.NoError(ht, restart())

	// Restart twice more: once a run has stored a hint past the
	// confirmation, every later run starts from it.
	for i := 0; i < 2; i++ {
		ht.RestartNode(alice)
	}

	err := wait.NoError(func() error {
		resp := alice.RPC.PendingChannels()
		if n := len(resp.PendingOpenChannels); n != 0 {
			return fmt.Errorf("%d pending open channels", n)
		}

		return nil
	}, 30*time.Second)
	require.NoError(ht, err, "channel still pending after its funding "+
		"transaction confirmed")

	chanPoint := &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			FundingTxidBytes: pending.Txid,
		},
		OutputIndex: pending.OutputIndex,
	}
	ht.AssertChannelActive(alice, chanPoint)
	ht.AssertChannelActive(bob, chanPoint)
}
