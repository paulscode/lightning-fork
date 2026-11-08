package contractcourt

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// waitForScriptConf waits for a confirmation registration of a script alone.
func waitForScriptConf(t *testing.T, h *chainWatcherTestHarness,
	pkScript []byte) *mockConfirmationEvent {

	t.Helper()

	var event *mockConfirmationEvent
	require.Eventually(t, func() bool {
		event = h.notifier.confEventForScript(pkScript)
		return event != nil
	}, 5*time.Second, 10*time.Millisecond)

	return event
}

// TestChainWatcherRestoredMovedFunding checks a restored channel's funding
// output spent by a transaction its backup didn't know, a splice after the
// backup, is no close: the channel is flagged, and the peer's commitment on
// the output the splice moved it to is found by our output on it and
// handled as the peer's close of a state we lost.
func TestChainWatcherRestoredMovedFunding(t *testing.T) {
	t.Parallel()

	h := newChainWatcherTestHarness(t, withRequiredConfs(3))
	alice := h.aliceChannel.State()
	require.NoError(t, alice.ApplyChanStatus(channeldb.ChanStatusRestored))

	// A splice after the backup, with a cooperative close's sequence.
	splice := h.createUnnegotiatedSpendTx(95_000)
	h.sendSpend(splice)
	h.waitForConfRegistration()
	h.mineBlocks(3)
	h.confirmTx(splice, h.currentHeight)

	h.assertNoCoopClose(200 * time.Millisecond)
	require.Eventually(t, func() bool {
		return alice.HasChanStatus(
			channeldb.ChanStatusFundingSpentUnknown,
		)
	}, 5*time.Second, 10*time.Millisecond)

	script, ok, err := restoredToRemoteScript(alice)
	require.NoError(t, err)
	require.True(t, ok)
	event := waitForScriptConf(t, h, script)
	require.EqualValues(t, 3, event.numConfs)

	// The peer's commitment, here on the old output: it pays our output
	// the same way on any.
	bobCommit := h.bobChannel.State().LocalCommitment.CommitTx
	event.confirmedChan <- &chainntnfs.TxConfirmation{
		Tx: bobCommit, BlockHeight: uint32(h.currentHeight + 10),
	}

	closeInfo := h.waitForRemoteUnilateralClose(5 * time.Second)
	h.assertRemoteUnilateralCloseTx(closeInfo, bobCommit)
	require.NotNil(t, closeInfo.CommitResolution)
	require.Equal(t, bobCommit.TxHash(),
		closeInfo.CommitResolution.SelfOutPoint.Hash)
}

// TestChainWatcherRestoredMovedNotCommitment checks a transaction paying our
// output script that isn't a commitment closes nothing.
func TestChainWatcherRestoredMovedNotCommitment(t *testing.T) {
	t.Parallel()

	h := newChainWatcherTestHarness(t, withRequiredConfs(1))
	alice := h.aliceChannel.State()
	require.NoError(t, alice.ApplyChanStatus(channeldb.ChanStatusRestored))

	// Before the spend: from it on, the watcher refreshes alice.
	script, _, err := restoredToRemoteScript(alice)
	require.NoError(t, err)
	h.sendSpend(h.createUnnegotiatedSpendTx(95_000))
	event := waitForScriptConf(t, h, script)

	payment := wire.NewMsgTx(2)
	payment.AddTxIn(&wire.TxIn{Sequence: wire.MaxTxInSequenceNum})
	payment.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})
	event.confirmedChan <- &chainntnfs.TxConfirmation{Tx: payment}

	select {
	case <-h.chanEvents.RemoteUnilateralClosure:
		t.Fatal("a payment taken for the peer's commitment")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestChainWatcherRestoredFollowsSplice checks a restored channel follows a
// splice its backup named, though it has no commitments on its output, and
// handles the peer's commitment there as the peer's close of a state we
// lost.
func TestChainWatcherRestoredFollowsSplice(t *testing.T) {
	t.Parallel()

	h := newChainWatcherTestHarness(t, withRequiredConfs(1))
	splice := pendSplice(t, h)

	// What a restore records: the splice's output and keys, no
	// commitments.
	alice := h.aliceChannel.State()
	records, err := alice.SpliceRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	r := records[0]
	require.NoError(t, alice.PutSpliceRecord(&channeldb.SpliceRecord{
		OutPoint: r.OutPoint, Capacity: r.Capacity,
		LocalKey: r.LocalKey, RemoteKey: r.RemoteKey,
	}))
	require.NoError(t, alice.ApplyChanStatus(channeldb.ChanStatusRestored))

	followSplice(t, h, splice)

	bobCommit, err := h.bobChannel.SignedSpliceCommitment(splice.TxHash())
	require.NoError(t, err)
	h.sendSpend(bobCommit)

	closeInfo := h.waitForRemoteUnilateralClose(5 * time.Second)
	h.assertRemoteUnilateralCloseTx(closeInfo, bobCommit)
	require.NotNil(t, closeInfo.CommitResolution)
	require.Equal(t, bobCommit.TxHash(),
		closeInfo.CommitResolution.SelfOutPoint.Hash)
	require.False(t, alice.HasChanStatus(
		channeldb.ChanStatusFundingSpentUnknown,
	))
}

// TestChainWatcherRestoredNoPreviousFundings checks a restored channel moved
// by a splice doesn't watch the funding output it was opened on: the backup
// knows neither the revoked commitments on it nor every splice since, and
// the splice that spent it may not be the one recorded.
func TestChainWatcherRestoredNoPreviousFundings(t *testing.T) {
	t.Parallel()

	for _, restored := range []bool{false, true} {
		h := newChainWatcherTestHarness(t, withRequiredConfs(1))
		splice := pendSplice(t, h)
		oldFunding := h.aliceChannel.State().FundingOutpoint
		require.NoError(t, h.aliceChannel.LockPendingFunding(
			splice.TxHash(), lnwire.ShortChannelID{
				BlockHeight: 900_000,
			},
		))
		state := h.aliceChannel.State()
		if restored {
			require.NoError(t, state.ApplyChanStatus(
				channeldb.ChanStatusRestored,
			))
		}

		// A restart: a new watcher for the channel as it is now.
		require.NoError(t, h.chainWatcher.Stop())
		h.notifier.perOutpoint = true
		cfg := h.chainWatcher.cfg
		cfg.chanState = state
		w, err := newChainWatcher(cfg)
		require.NoError(t, err)
		require.NoError(t, w.Start())

		watched := func() bool {
			_, ok := h.notifier.spendChanFor(oldFunding)
			return ok
		}
		if restored {
			time.Sleep(200 * time.Millisecond)
			require.False(t, watched())
		} else {
			require.Eventually(t, watched, 5*time.Second,
				10*time.Millisecond)
		}
		require.NoError(t, w.Stop())
	}
}

// TestChainWatcherRestoredCoopClose checks a restored channel still takes a
// close for one: its funding output spent alone with a close's sequence,
// paying our wallet and no P2WSH script. With a P2WSH output, the shape of a
// splice's new funding output, it is flagged instead.
func TestChainWatcherRestoredCoopClose(t *testing.T) {
	t.Parallel()

	ourScript := append([]byte{0x00, 0x14}, make([]byte, 20)...)
	p2wsh := append([]byte{0x00, 0x20}, make([]byte, 32)...)

	for _, splice := range []bool{false, true} {
		h := newChainWatcherTestHarness(t, withRequiredConfs(1))
		alice := h.aliceChannel.State()
		require.NoError(t, alice.ApplyChanStatus(
			channeldb.ChanStatusRestored,
		))
		h.chainWatcher.cfg.isOurAddr = func(a btcutil.Address) bool {
			return bytes.Equal(a.ScriptAddress(), ourScript[2:])
		}

		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: alice.FundingOutpoint,
			Sequence:         wire.MaxTxInSequenceNum,
		})
		tx.AddTxOut(&wire.TxOut{Value: 50_000, PkScript: ourScript})
		if splice {
			tx.AddTxOut(&wire.TxOut{Value: 900_000, PkScript: p2wsh})
		}
		h.sendSpend(tx)

		if !splice {
			closeInfo := h.waitForCoopClose(5 * time.Second)
			require.Equal(t, tx.TxHash(), closeInfo.ClosingTXID)
			continue
		}
		h.assertNoCoopClose(200 * time.Millisecond)
		require.Eventually(t, func() bool {
			return alice.HasChanStatus(
				channeldb.ChanStatusFundingSpentUnknown,
			)
		}, 5*time.Second, 10*time.Millisecond)
	}
}

// TestChainWatcherRestoredMovedKeepsWatching checks the search for the peer's
// commitment carries on past a transaction paying our output script that
// isn't one.
func TestChainWatcherRestoredMovedKeepsWatching(t *testing.T) {
	t.Parallel()

	h := newChainWatcherTestHarness(t, withRequiredConfs(1))
	alice := h.aliceChannel.State()
	require.NoError(t, alice.ApplyChanStatus(channeldb.ChanStatusRestored))

	// Before the spend: from it on, the watcher refreshes alice.
	script, _, err := restoredToRemoteScript(alice)
	require.NoError(t, err)
	h.sendSpend(h.createUnnegotiatedSpendTx(95_000))
	event := waitForScriptConf(t, h, script)

	payment := wire.NewMsgTx(2)
	payment.AddTxIn(&wire.TxIn{Sequence: wire.MaxTxInSequenceNum})
	payment.AddTxOut(&wire.TxOut{Value: 1000, PkScript: script})
	event.confirmedChan <- &chainntnfs.TxConfirmation{
		Tx: payment, BlockHeight: 500,
	}

	// Watched again, past it.
	var next *mockConfirmationEvent
	require.Eventually(t, func() bool {
		next = h.notifier.confEventForScript(script)
		return next != nil && next != event
	}, 5*time.Second, 10*time.Millisecond)

	bobCommit := h.bobChannel.State().LocalCommitment.CommitTx
	next.confirmedChan <- &chainntnfs.TxConfirmation{
		Tx: bobCommit, BlockHeight: 510,
	}
	closeInfo := h.waitForRemoteUnilateralClose(5 * time.Second)
	h.assertRemoteUnilateralCloseTx(closeInfo, bobCommit)
}

// TestChainWatcherNotifiesUnknownSpend checks the channel event stream hears
// of a channel flagged for a spend it doesn't recognise, once.
func TestChainWatcherNotifiesUnknownSpend(t *testing.T) {
	t.Parallel()

	h := newChainWatcherTestHarness(t, withRequiredConfs(1))
	type event struct {
		chanPoint wire.OutPoint
		txid      chainhash.Hash
	}
	events := make(chan event, 4)
	h.chainWatcher.cfg.notifySpentUnknown = func(op wire.OutPoint,
		txid chainhash.Hash) {

		events <- event{op, txid}
	}

	tx := h.createUnnegotiatedSpendTx(95_000)
	h.sendSpend(tx)

	select {
	case e := <-events:
		require.Equal(t, h.aliceChannel.State().FundingOutpoint,
			e.chanPoint)
		require.Equal(t, tx.TxHash(), e.txid)
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}

	// Seen again, as after a restart: flagged already, no new event.
	require.NoError(t, h.chainWatcher.handleUnknownSpend(
		&chainntnfs.SpendDetail{
			SpenderTxHash: &chainhash.Hash{1}, SpendingTx: tx,
		},
	))
	select {
	case <-events:
		t.Fatal("notified twice")
	case <-time.After(100 * time.Millisecond):
	}
}
