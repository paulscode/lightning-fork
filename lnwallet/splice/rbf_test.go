//go:build !dev && !integration

package splice

import (
	"github.com/btcsuite/btcd/wire"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// signSplice runs Alice's splice from its construction to both
// tx_signatures, Bob signing first; it returns Bob's funding key.
func signSplice(t *testing.T, h *acceptorHarness, s *aliceSplice,
	bobKey *btcec.PublicKey) *btcec.PublicKey {

	t.Helper()

	txid := s.res.Tx.TxHash()
	var bobCommit *lnwire.CommitSig
	select {
	case bobCommit = <-h.link.commits:
	case <-time.After(5 * time.Second):
		t.Fatal("no commitment_signed from the machine")
	}
	if bobKey == nil {
		for _, f := range h.bob.PendingFundings() {
			if f.OutPoint.Hash == txid {
				bobKey = f.LocalKey.PubKey
			}
		}
	}
	require.NotNil(t, bobKey)

	require.NoError(t, h.alice.AddPendingFunding(
		s.pendingFunding(h, bobKey),
	))
	require.NoError(t, h.alice.ReceiveSpliceCommitment(txid,
		&lnwallet.CommitSigs{
			CommitSig: bobCommit.CommitSig,
			HtlcSigs:  bobCommit.HtlcSigs,
		}))
	aliceSigs, err := h.alice.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	h.machine.SpliceCommitReceived(txid,
		h.bob.ReceiveSpliceCommitment(txid, aliceSigs))

	bobTxSigs := next[*lnwire.TxSignatures](t, h)
	require.Equal(t, txid, bobTxSigs.TxID)
	h.machine.HandleSpliceMessage(s.aliceTxSignatures(t, h))
	requireResumed(t, h)

	return bobKey
}

// replaceAliceSplice has Alice replace her signed splice at a higher fee
// rate: tx_init_rbf, Bob's tx_ack_rbf, and her new construction with the
// same coin and keys, not yet run.
func replaceAliceSplice(t *testing.T, h *acceptorHarness, prev *aliceSplice,
	bobKey *btcec.PublicKey, feeRate chainfee.SatPerKWeight) *aliceSplice {

	t.Helper()

	return replaceAliceSpliceWith(t, h, prev, bobKey, feeRate, nil)
}

// replaceAliceSpliceWith is replaceAliceSplice with more coins of Alice's
// in the replacement.
func replaceAliceSpliceWith(t *testing.T, h *acceptorHarness,
	prev *aliceSplice, bobKey *btcec.PublicKey,
	feeRate chainfee.SatPerKWeight, extra []Coin) *aliceSplice {

	t.Helper()

	amount := prev.sel.Contribution
	h.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
		ChannelID:                 h.machine.cfg.ChanID,
		LockTime:                  h.chain.height,
		FeeRatePerKw:              uint32(feeRate),
		FundingOutputContribution: fn.Some(amount),
		RequireConfirmedInputs:    true,
	})
	ack := next[*lnwire.TxAckRbf](t, h)
	require.Equal(t, fn.Some(btcutil.Amount(0)),
		ack.FundingOutputContribution)

	state := h.alice.State()
	_, out, err := input.GenFundingPkScript(
		state.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		state.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		int64(state.Capacity),
	)
	require.NoError(t, err)
	_, newOut, err := input.GenFundingPkScript(
		prev.fundingKey.PubKey().SerializeCompressed(),
		bobKey.SerializeCompressed(), int64(state.Capacity+amount),
	)
	require.NoError(t, err)

	// Change to another address of hers: the transaction differs even at
	// the same fee rate, as the port's random serial ids make it.
	s := &aliceSplice{
		coinKey: prev.coinKey, coin: prev.coin,
		fundingKey: prev.fundingKey, change: p2wpkh(0x43),
	}
	s.sel, err = SelectAddFunds(Shape{
		SharedWitnessWeight: input.MultiSigWitnessSize,
		FundingScript:       newOut.PkScript,
	}, []Coin{s.coin}, amount, false, feeRate, s.change, dustLimit, 0)
	require.NoError(t, err)
	if len(extra) > 0 {
		// Every coin in, a fee within the session's bounds out of
		// the change.
		total := s.coin.Value
		for _, c := range extra {
			total += c.Value
			h.chain.outs[c.OutPoint] = c.PrevTx.TxOut[0]
		}
		s.sel.Change = total - amount - 5_000
	}

	s.session, err = interactivetx.NewSession(interactivetx.Config{
		Role:      interactivetx.Initiator,
		ChannelID: h.machine.cfg.ChanID,
		Shared: interactivetx.SharedInput{
			OutPoint:      state.CurrentFundingOutpoint(),
			Value:         state.Capacity,
			PkScript:      out.PkScript,
			WitnessWeight: input.MultiSigWitnessSize,
		},
		FundingScript:         newOut.PkScript,
		InitiatorContribution: amount,
		FeeRate:               feeRate,
		LockTime:              h.chain.height,
		CheckPeerInput:        func(*interactivetx.Input) error { return nil },
		CheckPeerOutput:       func(*interactivetx.Output) error { return nil },
		DustLimit:             dustLimit,
		WitnessWeight:         peerWitnessWeight,
		Reserve:               func(c btcutil.Amount) btcutil.Amount { return c / 100 },
		InitiatorBalance:      state.LocalCommitment.LocalBalance.ToSatoshis(),
		AcceptorBalance:       state.LocalCommitment.RemoteBalance.ToSatoshis(),
	})
	require.NoError(t, err)
	require.NoError(t, s.session.AddSharedInputAndFunding())
	s.session.AddLocalInput(interactivetx.Input{
		OutPoint: s.coin.OutPoint, Value: s.coin.Value,
		PkScript: s.coin.PkScript, Sequence: interactivetx.MaxSequence,
		MaxWitnessWeight: input.P2WKHWitnessSize, PrevTx: s.coin.PrevTx,
	})
	for _, c := range extra {
		s.session.AddLocalInput(interactivetx.Input{
			OutPoint: c.OutPoint, Value: c.Value,
			PkScript: c.PkScript, Sequence: interactivetx.MaxSequence,
			MaxWitnessWeight: input.P2WKHWitnessSize, PrevTx: c.PrevTx,
		})
	}
	if s.sel.Change > 0 {
		s.session.AddLocalOutput(interactivetx.Output{
			Value: s.sel.Change, PkScript: s.change,
		})
	}

	return s
}

// rbfPair runs Alice's splice to signed, then her replacement to signed:
// two candidates.
func rbfPair(t *testing.T, h *acceptorHarness) (*aliceSplice,
	*aliceSplice) {

	t.Helper()

	first := startAliceSplice(t, h, 40_000_000, 2_500)
	first.construct(t, h)
	bobKey := signSplice(t, h, first, nil)

	second := replaceAliceSplice(t, h, first, bobKey, 5_000)
	second.construct(t, h)
	signSplice(t, h, second, bobKey)

	return first, second
}

// lockOn confirms txid to our depth and completes the lock with Alice's
// splice_locked.
func lockOn(t *testing.T, h *acceptorHarness, s *aliceSplice) {
	t.Helper()

	txid := s.res.Tx.TxHash()
	h.chain.confirm(t, txid, 6, 1, &chainntnfs.TxConfirmation{
		BlockHeight: 900_006, TxIndex: 3,
	})
	locked := next[*lnwire.SpliceLocked](t, h)
	require.Equal(t, txid, locked.SpliceTxid)
	h.machine.HandleSpliceMessage(&lnwire.SpliceLocked{
		ChannelID: h.machine.cfg.ChanID, SpliceTxid: txid,
	})
	require.Eventually(t, func() bool {
		return h.bob.State().CurrentFundingOutpoint().Hash == txid
	}, 5*time.Second, 20*time.Millisecond)
	require.Empty(t, h.bob.PendingFundings())
}

// TestAcceptSpliceReplaced checks a peer replacing its splice: both
// transactions are candidates, the replacement published, and the channel
// locks on whichever confirms.
func TestAcceptSpliceReplaced(t *testing.T) {
	t.Parallel()

	t.Run("the replacement confirms", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		capacity := h.bob.State().Capacity
		first, second := rbfPair(t, h)
		require.NotEqual(t, first.res.Tx.TxHash(),
			second.res.Tx.TxHash())

		// Both fund the channel until one confirms; the
		// replacement is what was published last.
		require.Len(t, h.bob.PendingFundings(), 2)
		_, ok := h.wallet.publishedTxs()[second.res.Tx.TxHash()]
		require.True(t, ok)

		lockOn(t, h, second)
		require.Equal(t, capacity+40_000_000, h.bob.State().Capacity)
	})

	t.Run("the original confirms", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first, _ := rbfPair(t, h)

		// The peer's splice_locked for the original comes first:
		// noted, nothing moves on its word.
		h.machine.HandleSpliceMessage(&lnwire.SpliceLocked{
			ChannelID:  h.machine.cfg.ChanID,
			SpliceTxid: first.res.Tx.TxHash(),
		})
		time.Sleep(50 * time.Millisecond)
		require.Len(t, h.bob.PendingFundings(), 2)

		// The replacement's input spent by the original is no
		// dead splice.
		h.chain.spend(t, h.bob.State().CurrentFundingOutpoint(),
			first.res.Tx, 1)

		// At our depth the original is the splice, locked with
		// the peer's earlier splice_locked.
		txid := first.res.Tx.TxHash()
		h.chain.confirm(t, txid, 6, 1, &chainntnfs.TxConfirmation{
			BlockHeight: 900_006, TxIndex: 3,
		})
		locked := next[*lnwire.SpliceLocked](t, h)
		require.Equal(t, txid, locked.SpliceTxid)
		require.Eventually(t, func() bool {
			return h.bob.State().CurrentFundingOutpoint().Hash ==
				txid
		}, 5*time.Second, 20*time.Millisecond)
		require.Empty(t, h.bob.PendingFundings())

		// Recorded just after the lock.
		var last *channeldb.LastSplice
		require.Eventually(t, func() bool {
			var err error
			last, err = h.bob.State().LastSplice()
			return err == nil && last != nil
		}, 5*time.Second, 20*time.Millisecond)
		require.Equal(t, "locked", last.Outcome)
		require.Equal(t, txid, last.Txid)
	})

	t.Run("a replacement given up falls back", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		limits := NewLimits()
		h.machine.cfg.Limits = limits
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		bobKey := signSplice(t, h, first, nil)

		second := replaceAliceSplice(t, h, first, bobKey, 5_000)
		msg, err := second.session.Next()
		require.NoError(t, err)
		h.machine.HandleSpliceMessage(msg)
		next[*lnwire.TxComplete](t, h)
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID, Data: []byte("nah"),
		})
		next[*lnwire.TxAbort](t, h)
		requireResumed(t, h)

		// Given up, it counts against the channel's aborts.
		limits.mu.Lock()
		aborts := limits.recentAborts(h.machine.cfg.ChanID)
		limits.mu.Unlock()
		require.Equal(t, 1, aborts)

		// The original is the splice again, and locks.
		require.Len(t, h.bob.PendingFundings(), 1)
		lockOn(t, h, first)
	})

	t.Run("after a restart", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first, _ := rbfPair(t, h)

		restarted := NewMachine(h.machine.cfg)
		startReestablished(t, restarted)
		t.Cleanup(restarted.Stop)
		h.machine.Stop()
		h.machine = restarted

		restarted.mu.Lock()
		latest, replaced := restarted.active, restarted.active.replaced
		restarted.mu.Unlock()
		require.Equal(t, chainfee.SatPerKWeight(5_000), latest.feeRate)
		require.Len(t, replaced, 1)
		require.Equal(t, first.res.Tx.TxHash(), replaced[0].txid)

		// The original confirms: the restarted machine follows it.
		lockOn(t, h, first)
	})

	// The original confirmed and adopted over its replacement before a
	// restart, our splice_locked recorded as sent: the restarted machine,
	// the replacement its splice again, sends it once the original is
	// deep enough, the peer not having it on this connection.
	t.Run("after a restart, the one we locked", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first, _ := rbfPair(t, h)
		require.NoError(t, h.bob.State().UpdateSpliceRecord(
			first.res.Tx.TxHash(),
			func(r *channeldb.SpliceRecord) error {
				r.Negotiation.LockedSent = true
				return nil
			}))

		restarted := NewMachine(h.machine.cfg)
		startReestablished(t, restarted)
		t.Cleanup(restarted.Stop)
		h.machine.Stop()
		h.machine = restarted

		lockOn(t, h, first)
	})
}

// TestAcceptSpliceReplacementRefused checks the replacements refused: one
// paying less in fees, one of a splice not yet signed both ways (a
// disconnection), and one
// after our splice_locked; and one at the same fee rate accepted. The
// splice carries on.
func TestAcceptSpliceReplacementRefused(t *testing.T) {
	t.Parallel()

	rbf := func(h *acceptorHarness, feeRate uint32) {
		h.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
			ChannelID:                 h.machine.cfg.ChanID,
			LockTime:                  h.chain.height,
			FeeRatePerKw:              feeRate,
			FundingOutputContribution: fn.Some(btcutil.Amount(40_000_000)),
		})
	}

	// A replacement at a falling fee rate is refused at once.
	t.Run("a falling fee rate", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		signSplice(t, h, first, nil)

		rbf(h, 2_000)
		require.Contains(t, abortReason(t, h), codeFeeBelowFloor)
		requireResumed(t, h)
		require.Len(t, h.bob.PendingFundings(), 1)
		lockOn(t, h, first)
	})

	t.Run("the same fee rate", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		bobKey := signSplice(t, h, first, nil)

		second := replaceAliceSplice(t, h, first, bobKey, 2_500)
		second.construct(t, h)
		signSplice(t, h, second, bobKey)
		require.Len(t, h.bob.PendingFundings(), 2)
		lockOn(t, h, first)
	})

	t.Run("not yet signed", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)

		// Not answered with tx_abort, which would end the splice:
		// the peer broke the protocol.
		rbf(h, 5_000)
		requireDisconnected(t, h)
	})

	// The peer's shutdown read: as for splice_init, a warning and the
	// connection closed. The splice carries on.
	t.Run("after the peer's shutdown", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		signSplice(t, h, first, nil)

		require.False(t, h.machine.ShutdownReceived())
		rbf(h, 5_000)
		w := next[*lnwire.Warning](t, h)
		require.Contains(t, string(w.Data), "after shutdown")
		requireDisconnected(t, h)
		require.Len(t, h.bob.PendingFundings(), 1)
	})

	t.Run("after our splice_locked", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		signSplice(t, h, first, nil)
		txid := first.res.Tx.TxHash()
		h.chain.confirm(t, txid, 6, 1, &chainntnfs.TxConfirmation{
			BlockHeight: 900_006, TxIndex: 3,
		})
		next[*lnwire.SpliceLocked](t, h)

		rbf(h, 5_000)
		require.Contains(t, abortReason(t, h), codeRbfUnsupported)
	})
}

// TestCheckReplacementFee checks that a replacement must pay at least the
// total fee of what it replaces.
func TestCheckReplacementFee(t *testing.T) {
	t.Parallel()

	prev := &active{res: &interactivetx.Result{Fee: 1_000}}
	m := &Machine{active: &active{replaced: []*active{prev}}}

	require.NoError(t, m.checkReplacementFee(&interactivetx.Result{Fee: 1_000}))
	require.NoError(t, m.checkReplacementFee(&interactivetx.Result{Fee: 1_001}))
	require.Error(t, m.checkReplacementFee(&interactivetx.Result{Fee: 999}))

	// Not a replacement: nothing to compare.
	m.active.replaced = nil
	require.NoError(t, m.checkReplacementFee(&interactivetx.Result{Fee: 1}))
}

// TestAcceptSpliceReplacedEdges checks the replacement's harder cases: a
// restart in the middle of signing an equal-rate replacement, the peer's
// lock of the original named in channel_reestablish, a replacement whose
// own coin is spent elsewhere, a replacement dropped after a restart, and a
// replacement whose construction breaks.
func TestAcceptSpliceReplacedEdges(t *testing.T) {
	t.Parallel()

	restart := func(t *testing.T, h *acceptorHarness) *Machine {
		t.Helper()

		restarted := NewMachine(h.machine.cfg)
		require.NoError(t, restarted.Start())
		t.Cleanup(restarted.Stop)
		h.machine.Stop()
		h.machine = restarted

		return restarted
	}

	// Our signatures out for an equal-rate replacement, Alice's not: the
	// restarted machine takes the replacement as the latest and asks
	// for its signatures on reconnecting.
	t.Run("a restart while signing an equal-rate replacement",
		func(t *testing.T) {
			t.Parallel()

			h := newAcceptorHarness(t)
			first := startAliceSplice(t, h, 40_000_000, 2_500)
			first.construct(t, h)
			bobKey := signSplice(t, h, first, nil)
			second := replaceAliceSplice(t, h, first, bobKey, 2_500)
			second.construct(t, h)
			commitOnly(t, h, second, bobKey)

			m := restart(t, h)
			next, _ := m.ReestablishTLVs()
			require.Equal(t, second.res.Tx.TxHash(),
				next.UnsafeFromSome().Txid)
		})

	// Alice locked the original and says so only in her reestablish.
	t.Run("the peer's lock named on reconnecting", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first, _ := rbfPair(t, h)
		txid := first.res.Tx.TxHash()
		h.machine.HandleReestablish(&lnwire.ChannelReestablish{
			MyCurrentFundingLocked: fn.Some(lnwire.FundingTxidFlags{
				Txid: txid,
			}),
		})
		h.chain.confirm(t, txid, 6, 1, &chainntnfs.TxConfirmation{
			BlockHeight: 900_006, TxIndex: 3,
		})
		next[*lnwire.SpliceLocked](t, h)
		require.Eventually(t, func() bool {
			return h.bob.State().CurrentFundingOutpoint().Hash ==
				txid
		}, 5*time.Second, 20*time.Millisecond)
	})

	// The replacement adds a coin the original doesn't spend; spent
	// elsewhere, the replacement is dead and the original the splice.
	t.Run("a dead replacement falls back", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		bobKey := signSplice(t, h, first, nil)
		extra := coin(78, first.coin.PkScript, 1_000_000)
		second := replaceAliceSpliceWith(t, h, first, bobKey, 2_600,
			[]Coin{extra})
		second.construct(t, h)
		signSplice(t, h, second, bobKey)
		require.Len(t, h.bob.PendingFundings(), 2)

		elsewhere := wire.NewMsgTx(2)
		elsewhere.AddTxIn(&wire.TxIn{PreviousOutPoint: extra.OutPoint})
		elsewhere.AddTxOut(&wire.TxOut{Value: 900_000, PkScript: p2tr(9)})
		h.chain.spend(t, extra.OutPoint, elsewhere, 1)
		h.chain.confirm(t, elsewhere.TxHash(), deadDepth, 1,
			&chainntnfs.TxConfirmation{})

		// The original is the splice again; the replacement is
		// recorded dead but stays in the funding set, signed for, as
		// the peer's does, until the original's lock retires it.
		require.Eventually(t, func() bool {
			h.machine.mu.Lock()
			defer h.machine.mu.Unlock()

			a := h.machine.active
			return a != nil && a.txid == first.res.Tx.TxHash()
		}, 5*time.Second, 10*time.Millisecond)
		require.Len(t, h.bob.PendingFundings(), 2)
		recs, err := h.bob.State().SpliceRecords()
		require.NoError(t, err)
		for _, r := range recs {
			require.Equal(t, r.Txid() == second.res.Tx.TxHash(),
				r.Negotiation.Dead)
		}
		lockOn(t, h, first)
	})

	// After a restart the replacement, signed by neither and not named
	// by the peer's reestablish, is dropped there, quietly: the
	// original, a candidate only since the restart, is published and
	// watched again. Kept, it held the channel quiescent until its
	// deadline, then a tx_abort went to a peer whose splice was signed
	// both ways.
	t.Run("a replacement dropped after a restart", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		bobKey := signSplice(t, h, first, nil)
		second := replaceAliceSplice(t, h, first, bobKey, 2_600)
		second.construct(t, h)
		<-h.link.commits

		restart(t, h)
		before := h.wallet.publishCount()
		h.machine.HandleReestablish(&lnwire.ChannelReestablish{})
		require.Eventually(t, func() bool {
			return h.wallet.publishCount() > before
		}, 5*time.Second, 10*time.Millisecond)
		signing, _ := h.machine.Signing()
		require.False(t, signing)
		require.Len(t, h.bob.PendingFundings(), 1)
		select {
		case msg := <-h.peer.msgs:
			t.Fatalf("sent %T", msg)
		case <-time.After(100 * time.Millisecond):
		}
		lockOn(t, h, first)
	})

	// A replacement whose construction breaks (a tx_complete before the
	// shared input): aborted, the original stays the splice; the peer's
	// next message, which may have crossed our tx_abort, is ignored.
	t.Run("a broken construction", func(t *testing.T) {
		t.Parallel()

		h := newAcceptorHarness(t)
		first := startAliceSplice(t, h, 40_000_000, 2_500)
		first.construct(t, h)
		bobKey := signSplice(t, h, first, nil)
		replaceAliceSplice(t, h, first, bobKey, 2_600)

		// Alice's construction never starts properly.
		complete := &lnwire.TxComplete{ChannelID: h.machine.cfg.ChanID}
		h.machine.HandleSpliceMessage(complete)
		next[*lnwire.TxComplete](t, h)
		h.machine.HandleSpliceMessage(complete)
		next[*lnwire.TxAbort](t, h)
		h.machine.HandleSpliceMessage(complete)
		time.Sleep(100 * time.Millisecond)
		h.peer.mu.Lock()
		require.Nil(t, h.peer.disconnected)
		h.peer.mu.Unlock()
		require.Len(t, h.bob.PendingFundings(), 1)
		h.machine.mu.Lock()
		a := h.machine.active
		h.machine.mu.Unlock()
		require.NotNil(t, a)
		require.Equal(t, first.res.Tx.TxHash(), a.txid)
	})
}

// commitOnly runs a replacement's commitment exchange and Bob's
// tx_signatures, keeping Alice's back.
func commitOnly(t *testing.T, h *acceptorHarness, s *aliceSplice,
	bobKey *btcec.PublicKey) {

	t.Helper()

	txid := s.res.Tx.TxHash()
	bobCommit := <-h.link.commits
	require.NoError(t, h.alice.AddPendingFunding(
		s.pendingFunding(h, bobKey),
	))
	require.NoError(t, h.alice.ReceiveSpliceCommitment(txid,
		&lnwallet.CommitSigs{
			CommitSig: bobCommit.CommitSig,
			HtlcSigs:  bobCommit.HtlcSigs,
		}))
	aliceSigs, err := h.alice.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	h.machine.SpliceCommitReceived(txid,
		h.bob.ReceiveSpliceCommitment(txid, aliceSigs))
	require.Equal(t, txid, next[*lnwire.TxSignatures](t, h).TxID)
}

// TestRefusedReplacementEcho checks that the peer echoing our tx_abort to a
// replacement we refused isn't taken for an abort of the signed splice: the
// peer isn't marked as misbehaving. A tx_abort after that, with nothing of
// ours to answer, still is.
func TestRefusedReplacementEcho(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	first := startAliceSplice(t, h, 40_000_000, 2_500)
	first.construct(t, h)
	signSplice(t, h, first, nil)

	// A lower fee rate: refused.
	h.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
		ChannelID:                 h.machine.cfg.ChanID,
		LockTime:                  h.chain.height,
		FeeRatePerKw:              2_000,
		FundingOutputContribution: fn.Some(btcutil.Amount(40_000_000)),
	})
	require.Contains(t, abortReason(t, h), "fee_below_floor")
	requireResumed(t, h)

	misbehaved := func() bool {
		var mis bool
		done := make(chan struct{})
		h.machine.enqueue(func() {
			mis = h.machine.misbehaved
			close(done)
		})
		<-done

		return mis
	}

	h.machine.HandleSpliceMessage(&lnwire.TxAbort{
		ChannelID: h.machine.cfg.ChanID,
	})
	require.False(t, misbehaved(), "the echo marked the peer")

	h.machine.HandleSpliceMessage(&lnwire.TxAbort{
		ChannelID: h.machine.cfg.ChanID, Data: []byte("no"),
	})
	require.True(t, misbehaved())
}

// TestReplacementAbortsCounted checks that failed replacements count
// against the channel's aborts, as aborted splices do: each kept the channel
// quiescent, so once the channel has had its aborts, a replacement is
// refused whatever its rate.
func TestReplacementAbortsCounted(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	h.machine.cfg.Limits = NewLimits()
	first := startAliceSplice(t, h, 40_000_000, 2_500)
	first.construct(t, h)
	signSplice(t, h, first, nil)

	rbf := func(feeRate uint32) {
		h.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
			ChannelID:                 h.machine.cfg.ChanID,
			LockTime:                  h.chain.height,
			FeeRatePerKw:              feeRate,
			FundingOutputContribution: fn.Some(btcutil.Amount(40_000_000)),
		})
	}
	for range DefaultMaxAborts {
		rbf(2_000)
		require.Contains(t, abortReason(t, h), codeFeeBelowFloor)
		requireResumed(t, h)
		h.machine.HandleSpliceMessage(&lnwire.TxAbort{
			ChannelID: h.machine.cfg.ChanID,
		})
	}

	rbf(5_000)
	require.Contains(t, abortReason(t, h), codeTooManyAborts)
}

// TestReplacementWithoutContribution checks a tx_init_rbf without
// funding_output_contribution: the peer puts nothing in the replacement, as
// the spec reads, whatever it put in the splice replaced.
func TestReplacementWithoutContribution(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	first := startAliceSplice(t, h, 40_000_000, 2_500)
	first.construct(t, h)
	signSplice(t, h, first, nil)

	h.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
		ChannelID:    h.machine.cfg.ChanID,
		LockTime:     h.chain.height,
		FeeRatePerKw: 5_000,
	})
	next[*lnwire.TxAckRbf](t, h)
	require.Eventually(t, func() bool {
		h.machine.mu.Lock()
		defer h.machine.mu.Unlock()

		a := h.machine.active
		return a != nil && len(a.replaced) == 1 &&
			a.theirContribution == 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestTxAbortWithoutSplice checks a tx_abort for a splice we don't have is
// echoed, as BOLT 2 asks (the Core Lightning port waits for the echo), every
// time, and that an echo, empty, isn't: two idle nodes of ours stop after
// one rather than echoing each other for good. Either ends quiescence.
func TestTxAbortWithoutSplice(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	h.machine.HandleSpliceMessage(&lnwire.TxAbort{
		ChannelID: h.machine.cfg.ChanID, Data: []byte("gone"),
	})
	echo := next[*lnwire.TxAbort](t, h)
	require.Equal(t, h.machine.cfg.ChanID, echo.ChannelID)

	// A quiescence the peer won ends with it.
	requireResumed(t, h)

	// An empty one, no splice of ours out: not echoed, but it ends the
	// peer's quiescence, so ours ends too.
	h.machine.HandleSpliceMessage(&lnwire.TxAbort{
		ChannelID: h.machine.cfg.ChanID,
	})
	requireResumed(t, h)
	select {
	case msg := <-h.peer.msgs:
		t.Fatalf("sent %T", msg)
	case <-time.After(100 * time.Millisecond):
	}

	p := newSplicePair(t)
	var aborts atomic.Int32
	p.dropMu.Lock()
	p.drop = func(_ *node, msg lnwire.Message) bool {
		if _, ok := msg.(*lnwire.TxAbort); ok {
			aborts.Add(1)
		}

		return false
	}
	p.dropMu.Unlock()
	stray := &lnwire.TxAbort{
		ChannelID: p.alice.machine.cfg.ChanID, Data: []byte("gone"),
	}
	p.alice.machine.HandleSpliceMessage(stray)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, int32(1), aborts.Load())

	// A second on the connection: echoed too.
	p.alice.machine.HandleSpliceMessage(stray)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, int32(2), aborts.Load())
}

// TestMessagesCrossingOurAbort checks a construction message or splice_ack
// the peer sent before our tx_abort reached it: ignored until its echo,
// rather than taken for a protocol breach and the connection dropped. One
// with no tx_abort of ours out still is.
func TestMessagesCrossingOurAbort(t *testing.T) {
	t.Parallel()

	for _, aborted := range []bool{true, false} {
		h := newAcceptorHarness(t)

		// Our tx_abort: the peer's splice_init refused.
		if aborted {
			key, err := btcec.NewPrivateKey()
			require.NoError(t, err)
			h.machine.cfg.Estimator = estimateOnly{rate: 10_000}
			h.machine.HandleSpliceMessage(&lnwire.SpliceInit{
				ChannelID:           h.machine.cfg.ChanID,
				FundingContribution: 1_000_000,
				FundingFeeRatePerKw: 300,
				LockTime:            h.chain.height,
				FundingPubKey:       key.PubKey(),
			})
			next[*lnwire.TxAbort](t, h)
		}
		h.machine.HandleSpliceMessage(&lnwire.TxAddOutput{
			ChannelID: h.machine.cfg.ChanID, SerialID: 2,
			Amount: 1_000, Script: p2tr(1),
		})
		h.machine.HandleSpliceMessage(&lnwire.SpliceAck{
			ChannelID: h.machine.cfg.ChanID,
		})
		time.Sleep(100 * time.Millisecond)

		h.peer.mu.Lock()
		dropped := h.peer.disconnected != nil
		h.peer.mu.Unlock()
		require.Equal(t, !aborted, dropped, "aborted %v", aborted)
	}
}

// TestReestablishNamesEarlierOrLocked checks the peer's next_funding naming
// an earlier transaction of the splice, whose tx_signatures we sent: they go
// again, never a tx_abort, which the Core Lightning port fails the channel
// on once it signed. One naming the funding the channel has locked to since
// is done with; a splice_locked for it, sent again, is ignored.
func TestReestablishNamesEarlierOrLocked(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	first, _ := rbfPair(t, h)
	firstTxid := first.res.Tx.TxHash()
	h.machine.HandleReestablish(&lnwire.ChannelReestablish{
		NextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: firstTxid}),
	})
	sigs := next[*lnwire.TxSignatures](t, h)
	require.Equal(t, firstTxid, sigs.TxID)

	lockOn(t, h, first)
	h.machine.HandleReestablish(&lnwire.ChannelReestablish{
		NextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: firstTxid}),
	})
	h.machine.HandleSpliceMessage(&lnwire.SpliceLocked{
		ChannelID: h.machine.cfg.ChanID, SpliceTxid: firstTxid,
	})
	select {
	case msg := <-h.peer.msgs:
		t.Fatalf("sent %T", msg)
	case <-time.After(100 * time.Millisecond):
	}
	h.peer.mu.Lock()
	require.Nil(t, h.peer.disconnected)
	h.peer.mu.Unlock()
}

// TestPeerSpliceInitWhilePreparing checks the peer's splice_init reaching
// us while our own splice still waits for quiescence, which the peer won:
// ours fails and the peer's goes ahead, rather than a disconnection for a
// splice_init with a splice in progress.
func TestPeerSpliceInitWhilePreparing(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	h.link.stfuGate = make(chan struct{})
	t.Cleanup(func() { close(h.link.stfuGate) })
	updates, err := h.machine.Splice(Request{
		Direction: DirectionOut, Amount: 100_000, FeeRate: 2_500,
		Destination: p2tr(1),
	})
	require.NoError(t, err)
	requireUpdates(t, updates, StatePreparing)

	startAliceSplice(t, h, 40_000_000, 2_500)
	u := requireUpdates(t, updates, StateAborted)
	require.Contains(t, u.Reason, codeNotQuiescent)
	h.peer.mu.Lock()
	require.Nil(t, h.peer.disconnected)
	h.peer.mu.Unlock()
}

// TestReplacementPayingLessKept checks a replacement of the peer's at a
// higher rate that pays less in total, having fewer inputs: kept as a
// candidate, not answered with a tx_abort once tx_complete is exchanged,
// which the Core Lightning port, having signed its part then, fails the
// channel on. It won't relay; whichever transaction confirms is followed.
func TestReplacementPayingLessKept(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	first := startAliceSplice(t, h, 40_000_000, 2_500)
	first.construct(t, h)
	bobKey := signSplice(t, h, first, nil)

	extra := coin(79, first.coin.PkScript, 1_000_000)
	second := replaceAliceSpliceWith(t, h, first, bobKey, 2_600,
		[]Coin{extra})
	second.construct(t, h)
	signSplice(t, h, second, bobKey)

	third := replaceAliceSplice(t, h, second, bobKey, 2_700)
	third.construct(t, h)
	require.Less(t, int64(third.res.Fee), int64(second.res.Fee))
	signSplice(t, h, third, bobKey)

	h.machine.mu.Lock()
	txid, replaced := h.machine.active.txid, len(h.machine.active.replaced)
	h.machine.mu.Unlock()
	require.Equal(t, third.res.Tx.TxHash(), txid)
	require.Equal(t, 2, replaced)
	require.Len(t, h.bob.PendingFundings(), 3)
}
