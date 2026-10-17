package itest

import (
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
)

// testIndependentSecrets runs a channel with option_independent_secrets
// between two real nodes, one of which reveals per-commitment secrets that
// have no shachain relationship, and then has that node broadcast a revoked
// state.
//
// Bob runs with --dev.independent-secrets-sender, so on such a channel he
// generates his secrets with a KDF instead of a shachain. Carol runs the
// defaults. The test checks that:
//   - both nodes signal the add-on by default and negotiate it, and report it
//     over RPC;
//   - payments flow while Carol accepts and stores Bob's secrets, also across
//     restarts of both nodes;
//   - when Bob broadcasts a revoked commitment, Carol sweeps it with the
//     secret she stored for that state.
func testIndependentSecrets(ht *lntest.HarnessTest) {
	const (
		chanAmt     = funding.MaxBtcFundingAmount
		paymentAmt  = 10_000
		numInvoices = 6
	)

	commitType := lnrpc.CommitmentType_ANCHORS
	nodeArgs := lntest.NodeArgsForCommitType(commitType)

	// Carol is the honest party. --nolisten and a long backoff keep Bob
	// from reconnecting to her before his breach, which would trigger data
	// loss protection instead.
	carol := ht.NewNode("Carol", append(
		nodeArgs, "--hodl.exit-settle", "--nolisten", "--minbackoff=1h",
	))
	bob := ht.NewNode("Bob", append(
		nodeArgs, "--dev.independent-secrets-sender",
	))
	ht.ConnectNodes(carol, bob)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, carol)

	chanPoint := ht.OpenChannel(carol, bob, lntest.OpenChannelParams{
		CommitmentType: commitType,
		Amt:            chanAmt,
	})

	// Neither node was told to: the add-on is on by default.
	assertIndependentSecrets := func(hn *node.HarnessNode) {
		ch := ht.QueryChannelByChanPoint(hn, chanPoint)
		require.True(ht, ch.IndependentSecrets,
			"%s: channel without option_independent_secrets",
			hn.Name())
	}
	assertIndependentSecrets(carol)
	assertIndependentSecrets(bob)

	bobPayReqs, _, _ := ht.CreatePayReqs(bob, paymentAmt, numInvoices)

	// Each payment has Bob reveal secrets for two of his commitments.
	ht.CompletePaymentRequests(carol, bobPayReqs[:numInvoices/2])
	bobChan := ht.AssertChannelLocalBalance(
		bob, chanPoint, 3*paymentAmt,
	)

	// Both nodes come back with the channel intact: Carol still has every
	// secret, and Bob regenerates his from the stored root.
	ht.RestartNode(carol)
	ht.RestartNode(bob)
	ht.EnsureConnected(carol, bob)
	ht.AssertChannelActive(carol, chanPoint)
	assertIndependentSecrets(carol)
	assertIndependentSecrets(bob)

	// Keep Bob's current state, to broadcast once it has been revoked.
	bobStateNum := ht.QueryChannelByChanPoint(bob, chanPoint).NumUpdates
	require.Equal(ht, bobChan.NumUpdates, bobStateNum)
	ht.BackupDB(bob)
	ht.EnsureConnected(carol, bob)
	ht.AssertChannelExists(carol, chanPoint)

	ht.CompletePaymentRequests(carol, bobPayReqs[numInvoices/2:])
	ht.AssertChannelLocalBalance(bob, chanPoint, numInvoices*paymentAmt)

	// Bob travels back to the revoked state and force closes with it.
	ht.RestartNodeAndRestoreDB(bob)
	ht.AssertChannelCommitHeight(bob, chanPoint, int(bobStateNum))

	_, closeUpd := ht.CloseChannelAssertPending(bob, chanPoint, true)
	pending := closeUpd.GetClosePending()
	require.NotNil(ht, pending)
	breachTXID, err := chainhash.NewHash(pending.Txid)
	require.NoError(ht, err)

	// Carol must keep what she needs for retribution across a restart.
	ht.RestartNode(carol)

	block := ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, *breachTXID)

	// On an anchor channel the first two outputs are anchors, so Bob's
	// to_remote output is the third. Carol's justice transaction spends it
	// together with Bob's revoked to_local output, which she can only do
	// with the secret Bob revealed for this state.
	toRemote := wire.OutPoint{Hash: *breachTXID, Index: 2}
	justiceTx := ht.AssertOutpointInMempool(toRemote)
	for _, txIn := range justiceTx.TxIn {
		require.Equal(ht, *breachTXID, txIn.PreviousOutPoint.Hash,
			"justice tx not spending the breach transaction")
	}
	require.GreaterOrEqual(ht, len(justiceTx.TxIn), 2,
		"justice tx should sweep both of Bob's outputs")

	ht.RestartNode(carol)
	ht.AssertNumPendingSweeps(carol, 1)

	block = ht.MineBlocksAndAssertNumTxes(1, 1)[0]
	ht.AssertTxInBlock(block, justiceTx.TxHash())
	ht.AssertNodeNumChannels(carol, 0)

	ht.MineBlocks(defaultCSV - 2)
	ht.AssertNumPendingForceClose(bob, 0)
}
