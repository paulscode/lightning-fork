//go:build !dev && !integration

package splice

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// dropCommitSigsFrom makes the pair lose every commitment_signed from one
// node: a peer that stops answering once the transaction is agreed.
func dropCommitSigsFrom(p *splicePair, from *node) {
	p.dropMu.Lock()
	p.drop = func(n *node, msg lnwire.Message) bool {
		_, isSig := msg.(*lnwire.CommitSig)
		return n == from && isSig
	}
	p.dropMu.Unlock()
}

// TestSpliceSigningTimeout checks our splice, its transaction agreed and
// our commitment_signed out, the peer's never coming. Signing last, we can't
// know the peer hasn't sent its tx_signatures, so the timeout drops nothing
// (a tx_abort then leaves the two apart). A reconnection settles it: the
// peer's channel_reestablish not naming it, the peer doesn't have it, and
// it is dropped, out of the channel's funding set, its coins released.
func TestSpliceSigningTimeout(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	p.alice.machine.cfg.Policy.NegotiationTimeout = 100 * time.Millisecond
	p.alice.fund(t, 1, 5_000_000)
	dropCommitSigsFrom(p, p.bob)

	updates, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.NoError(t, err)
	requireUpdates(t, updates, StatePreparing, StateNegotiating)

	// Past the timeout: still there.
	time.Sleep(500 * time.Millisecond)
	signing := func() bool {
		p.alice.machine.mu.Lock()
		defer p.alice.machine.mu.Unlock()

		a := p.alice.machine.active
		return a != nil && a.phase == phaseSigning
	}
	require.True(t, signing())

	// Reconnected, the peer naming no splice: dropped.
	p.alice.machine.HandleReestablish(&lnwire.ChannelReestablish{
		ChanID: p.alice.machine.cfg.ChanID,
	})
	requireUpdates(t, updates, StateAborted)
	require.Eventually(t, func() bool {
		p.alice.machine.mu.Lock()
		defer p.alice.machine.mu.Unlock()

		return p.alice.machine.active == nil
	}, 2*time.Second, 10*time.Millisecond)
	require.Zero(t, p.alice.wallet.leaseCount())
	require.Empty(t, p.alice.ch.PendingFundings())
}

// TestSpliceSigningTimeoutAcceptor checks the peer's splice stalled the same
// way: kept at the timeout, as the peer may have signed its part, and the
// node's place for the peer's splices given back, also when the node
// restarted in between.
func TestSpliceSigningTimeoutAcceptor(t *testing.T) {
	t.Parallel()

	for _, restart := range []bool{false, true} {
		p := newSplicePair(t)
		limits := NewLimits()
		p.alice.machine.cfg.Limits = limits
		timeout := 100 * time.Millisecond
		if restart {
			timeout = time.Hour
		}
		p.alice.machine.cfg.Policy.NegotiationTimeout = timeout
		p.alice.link.mu.Lock()
		p.alice.link.initiator = lntypes.Remote
		p.alice.link.mu.Unlock()
		p.bob.link.stfuResult = lntypes.Local
		p.bob.link.mu.Lock()
		p.bob.link.initiator = lntypes.Local
		p.bob.link.mu.Unlock()
		p.bob.fund(t, 1, 5_000_000)

		// Bob's commitment_signed never comes; a tx_abort of
		// Alice's is counted.
		var aborts atomic.Int32
		countAborts := func(n *node, msg lnwire.Message) bool {
			if _, ok := msg.(*lnwire.TxAbort); ok && n == p.alice {
				aborts.Add(1)
			}
			_, isSig := msg.(*lnwire.CommitSig)

			return n == p.bob && isSig
		}
		p.dropMu.Lock()
		p.drop = countAborts
		p.dropMu.Unlock()

		_, err := p.bob.machine.Splice(Request{
			Direction: DirectionIn, Amount: 1_000_000,
			FeeRate: 2_500,
		})
		require.NoError(t, err)

		signing := func() bool {
			p.alice.machine.mu.Lock()
			defer p.alice.machine.mu.Unlock()

			// The restart comes once our commitment_signed is
			// out: stopping before, the splice waits for the
			// reestablish to be dropped.
			a := p.alice.machine.active
			return a != nil && a.phase == phaseSigning &&
				a.commitSent
		}
		require.Eventually(t, signing, 5*time.Second,
			10*time.Millisecond)

		m := p.alice.machine
		if restart {
			cfg := m.cfg
			m.mu.Lock()
			txid := m.active.txid
			m.mu.Unlock()
			m.Stop()

			// The time runs from the agreement, across the
			// restart: past it, the splice ends at once though
			// the timeout is an hour.
			err := p.alice.ch.State().UpdateSpliceRecord(txid,
				func(r *channeldb.SpliceRecord) error {
					// Recorded with the splice.
					agreed := time.Unix(
						r.Negotiation.AgreedAt, 0,
					)
					require.WithinDuration(t, time.Now(),
						agreed, time.Minute)

					r.Negotiation.AgreedAt = time.Now().Add(
						-3 * time.Hour,
					).Unix()

					return nil
				})
			require.NoError(t, err)
			m = NewMachine(cfg)
			require.NoError(t, m.Start())
			t.Cleanup(m.Stop)
			time.Sleep(200 * time.Millisecond)
			m.mu.Lock()
			require.NotNil(t, m.active)
			m.mu.Unlock()

			m.HandleReestablish(&lnwire.ChannelReestablish{
				NextFunding: fn.Some(lnwire.FundingTxidFlags{
					Txid: txid,
				}),
			})
		}

		// A splice Bob built: the Core Lightning port signs its
		// part once tx_complete is exchanged and fails the channel
		// on a tx_abort after. Kept, named by Bob on reconnecting,
		// its place given back; never a tx_abort.
		require.Eventually(t, func() bool {
			limits.mu.Lock()
			defer limits.mu.Unlock()

			return len(limits.pending) == 0
		}, 5*time.Second, 10*time.Millisecond, "restart %v", restart)
		time.Sleep(200 * time.Millisecond)
		m.mu.Lock()
		kept := m.active != nil && m.active.phase == phaseSigning
		m.mu.Unlock()
		require.True(t, kept, "restart %v", restart)
		require.Zero(t, aborts.Load(), "restart %v", restart)
		require.Len(t, p.alice.ch.PendingFundings(), 1)
	}
}

// TestSpliceStopWhileSending checks a machine stopping while the link
// sends our commitment_signed undoes nothing: the commitment may be out, the
// peer signing first keeps the splice, so the record and our coins stay for
// the next machine.
func TestSpliceStopWhileSending(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	p.alice.link.commitAnswerLost = true
	p.alice.fund(t, 1, 5_000_000)

	_, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		recs, err := p.alice.ch.State().SpliceRecords()
		return err == nil && len(recs) == 1
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	p.alice.machine.Stop()

	recs, err := p.alice.ch.State().SpliceRecords()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotZero(t, p.alice.wallet.leaseCount())
	require.Len(t, p.alice.ch.PendingFundings(), 1)
}

// TestSpliceUnsignedAtRestart checks a splice recorded but never signed by
// us, as after stopping between the two: at the start it is dropped at
// once, out of the funding set and its coins released, rather than leaving
// the channel unable to take a commitment.
func TestSpliceUnsignedAtRestart(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	p.alice.machine.cfg.Policy.NegotiationTimeout = time.Hour
	p.alice.fund(t, 1, 5_000_000)
	dropCommitSigsFrom(p, p.bob)

	_, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.NoError(t, err)

	var txid chainhash.Hash
	require.Eventually(t, func() bool {
		p.alice.machine.mu.Lock()
		defer p.alice.machine.mu.Unlock()

		a := p.alice.machine.active
		if a == nil || a.phase != phaseSigning || !a.commitSent {
			return false
		}
		txid = a.txid

		return true
	}, 5*time.Second, 10*time.Millisecond)

	cfg := p.alice.machine.cfg
	p.alice.machine.Stop()
	require.NoError(t, p.alice.ch.State().UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.SpliceSig = nil
			return nil
		}))

	// Nothing goes out before the peer's channel_reestablish: a
	// tx_abort first fails its link.
	fromAlice := make(chan lnwire.Message, 8)
	p.dropMu.Lock()
	p.drop = func(n *node, msg lnwire.Message) bool {
		if n == p.alice {
			fromAlice <- msg
		}

		return true
	}
	p.dropMu.Unlock()

	m := NewMachine(cfg)
	require.NoError(t, m.Start())
	t.Cleanup(m.Stop)
	select {
	case msg := <-fromAlice:
		t.Fatalf("%T sent before channel_reestablish", msg)
	case <-time.After(200 * time.Millisecond):
	}

	m.HandleReestablish(&lnwire.ChannelReestablish{
		ChanID: m.cfg.ChanID,
	})
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()

		return m.active == nil
	}, 2*time.Second, 10*time.Millisecond)
	require.Empty(t, p.alice.ch.PendingFundings())
	require.Zero(t, p.alice.wallet.leaseCount())
}

// TestSpliceUseMaxKeepsReserve checks that adding everything keeps the
// wallet's reserve for fee-bumping its anchor channels' closes: it comes
// back as change.
func TestSpliceUseMaxKeepsReserve(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	const reserve = btcutil.Amount(30_000)
	p.alice.machine.cfg.Reserve = func() (btcutil.Amount, error) {
		return reserve, nil
	}
	p.alice.fund(t, 1, 1_000_000)

	updates, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, UseMax: true, FeeRate: 2_500,
	})
	require.NoError(t, err)
	requireUpdates(t, updates, StatePreparing, StateNegotiating,
		StateSigned, StateBroadcast)

	p.alice.machine.mu.Lock()
	tx, err := p.alice.machine.signedTx()
	p.alice.machine.mu.Unlock()
	require.NoError(t, err)
	var kept bool
	for _, out := range tx.TxOut {
		if btcutil.Amount(out.Value) == reserve {
			kept = true
		}
	}
	require.True(t, kept, "no change of the reserve")
}

// TestSpliceDisconnected checks the connection ending under a splice: one
// still negotiating ends with it, its coins released and its request told;
// one past its signatures goes on with the next connection's machine, and
// only the request's updates end.
func TestSpliceDisconnected(t *testing.T) {
	t.Parallel()

	t.Run("negotiating", func(t *testing.T) {
		t.Parallel()

		p := newSplicePair(t)
		p.alice.fund(t, 1, 5_000_000)
		p.dropMu.Lock()
		p.drop = func(from *node, msg lnwire.Message) bool {
			switch msg.(type) {
			case *lnwire.TxAddInput, *lnwire.TxAddOutput,
				*lnwire.TxComplete:

				return from == p.bob
			}

			return false
		}
		p.dropMu.Unlock()

		updates, err := p.alice.machine.Splice(Request{
			Direction: DirectionIn, UseMax: true, FeeRate: 2_500,
		})
		require.NoError(t, err)
		requireUpdates(t, updates, StatePreparing, StateNegotiating)
		require.Equal(t, 1, p.alice.wallet.leaseCount())

		p.alice.machine.Stop()
		u := requireUpdates(t, updates, StateAborted)
		require.Equal(t, codeDisconnected, u.Reason)
		_, open := <-updates
		require.False(t, open)
		require.Zero(t, p.alice.wallet.leaseCount())
	})

	t.Run("broadcast", func(t *testing.T) {
		t.Parallel()

		p := newSplicePair(t)
		p.alice.fund(t, 1, 5_000_000)
		updates, err := p.alice.machine.Splice(Request{
			Direction: DirectionIn, Amount: 1_000_000,
			FeeRate: 2_500,
		})
		require.NoError(t, err)
		requireUpdates(t, updates, StatePreparing, StateNegotiating,
			StateSigned, StateBroadcast)

		p.alice.machine.Stop()
		select {
		case _, open := <-updates:
			require.False(t, open)
		case <-time.After(5 * time.Second):
			t.Fatal("updates not ended")
		}

		// The splice is still the channel's, its coins leased.
		require.Len(t, p.alice.ch.PendingFundings(), 1)
		require.Equal(t, 1, p.alice.wallet.leaseCount())
	})
}

// TestSpliceWithheldGivesBackPlace checks a peer's splice whose tx_signatures
// the peer withholds after ours: kept, as the peer could publish it, but its
// place for the peer's splices given back at the timeout; after a restart,
// given back again, the time running from the agreement.
func TestSpliceWithheldGivesBackPlace(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	limits := NewLimits()
	h.machine.cfg.Limits = limits
	h.machine.cfg.Policy.NegotiationTimeout = time.Second
	s := startAliceSplice(t, h, 40_000_000, 2_500)
	s.construct(t, h)
	txid := s.res.Tx.TxHash()

	bobCommit := <-h.link.commits
	var bobKey *btcec.PublicKey
	for _, f := range h.bob.PendingFundings() {
		if f.OutPoint.Hash == txid {
			bobKey = f.LocalKey.PubKey
		}
	}
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
	next[*lnwire.TxSignatures](t, h)

	placed := func() int {
		limits.mu.Lock()
		defer limits.mu.Unlock()

		return len(limits.pending)
	}
	require.Eventually(t, func() bool { return placed() == 0 },
		5*time.Second, 10*time.Millisecond)
	require.Len(t, h.bob.PendingFundings(), 1)

	// Restarted past the deadline, which runs from the agreement and
	// not from the connection: the place goes back at once, not after
	// a new 2s.
	cfg := h.machine.cfg
	h.machine.Stop()
	m := NewMachine(cfg)
	require.NoError(t, m.Start())
	t.Cleanup(m.Stop)
	require.Eventually(t, func() bool { return placed() == 0 },
		time.Second, 10*time.Millisecond)
	m.mu.Lock()
	require.NotNil(t, m.active)
	m.mu.Unlock()

	// The agreement an hour ahead, as with a clock set back since: no
	// more than the timeout all the same.
	m.Stop()
	require.NoError(t, h.bob.State().UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.AgreedAt = time.Now().Add(time.Hour).Unix()
			return nil
		}))
	again := NewMachine(cfg)
	require.NoError(t, again.Start())
	t.Cleanup(again.Stop)
	require.Equal(t, 1, placed())
	require.Eventually(t, func() bool { return placed() == 0 },
		4*time.Second, 10*time.Millisecond)
}

// TestSpliceDisconnectedDuringReplacement checks the connection ending while
// the peer's replacement of our pending splice is negotiated: the splice it
// replaces is still ours and pending, its coins leased, and the request is
// not told it was aborted.
func TestSpliceDisconnectedDuringReplacement(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	p.alice.fund(t, 1, 5_000_000)
	updates, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.NoError(t, err)
	requireUpdates(t, updates, StatePreparing, StateNegotiating,
		StateSigned, StateBroadcast)
	leases := p.alice.wallet.leaseCount()
	require.Positive(t, leases)

	fromAlice := make(chan lnwire.Message, 64)
	p.dropMu.Lock()
	p.drop = func(from *node, msg lnwire.Message) bool {
		if from == p.alice {
			fromAlice <- msg
		}

		return true
	}
	p.dropMu.Unlock()
	p.alice.link.mu.Lock()
	p.alice.link.initiator = lntypes.Remote
	p.alice.link.mu.Unlock()
	p.alice.machine.HandleSpliceMessage(&lnwire.TxInitRbf{
		ChannelID:    p.alice.machine.cfg.ChanID,
		LockTime:     p.chain.height,
		FeeRatePerKw: 3_000,
	})
	select {
	case msg := <-fromAlice:
		require.IsType(t, &lnwire.TxAckRbf{}, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}

	p.alice.machine.Stop()
	for u := range updates {
		require.NotEqual(t, StateAborted, u.State, u.Reason)
	}
	require.Equal(t, leases, p.alice.wallet.leaseCount())
	require.Len(t, p.alice.ch.PendingFundings(), 1)
}

// TestSpliceLockedTLVWhileSigning checks the peer's my_current_funding_locked
// naming the splice while we still wait for its tx_signatures, lost on the
// way and coming again: taken as its splice_locked, as the message would be.
// It was dropped, and the peer doesn't send splice_locked again on that
// connection, so our lock waited for the next one.
func TestSpliceLockedTLVWhileSigning(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	s := splicePastSigning(t, h)
	txid := s.res.Tx.TxHash()

	h.machine.Stop()
	h.machine = NewMachine(h.machine.cfg)
	require.NoError(t, h.machine.Start())
	t.Cleanup(h.machine.Stop)
	h.machine.HandleReestablish(&lnwire.ChannelReestablish{
		MyCurrentFundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: txid,
		}),
	})
	h.machine.HandleSpliceMessage(s.aliceTxSignatures(t, h))
	requireResumed(t, h)

	h.chain.confirm(t, txid, 6, 1, &chainntnfs.TxConfirmation{
		BlockHeight: 900_010,
	})
	next[*lnwire.SpliceLocked](t, h)
	require.Eventually(t, func() bool {
		return h.bob.State().CurrentFundingOutpoint().Hash == txid
	}, 5*time.Second, 10*time.Millisecond)
}

// TestSpliceCanceledWhileQuiescing checks a splice canceled while the stfu
// exchange is under way, which our side then wins: the peer, quiescent too,
// hears a tx_abort and the channel resumes, rather than staying quiescent
// until the timeout disconnects it.
func TestSpliceCanceledWhileQuiescing(t *testing.T) {
	t.Parallel()

	alice, _ := soloAlice(t)
	alice.fund(t, 1, 5_000_000)
	alice.link.stfuGate = make(chan struct{})

	updates, err := alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.NoError(t, err)
	requireUpdates(t, updates, StatePreparing)
	require.True(t, alice.machine.Cancel())
	requireUpdates(t, updates, StateAborted)

	close(alice.link.stfuGate)
	select {
	case msg := <-alice.peer.msgs:
		require.IsType(t, &lnwire.TxAbort{}, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent")
	}
	select {
	case <-alice.link.resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("quiescence never ended after the cancel")
	}
}

// TestSpliceConnectionGone checks a link step answering that the machine's
// connection has ended, as the link of the next connection is refused to an
// old machine: nothing is undone, the record and our coins left for the
// next machine.
func TestSpliceConnectionGone(t *testing.T) {
	t.Parallel()

	p := newSplicePair(t)
	p.alice.link.commitErr = ErrConnectionGone
	p.alice.fund(t, 1, 5_000_000)

	_, err := p.alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		recs, err := p.alice.ch.State().SpliceRecords()
		return err == nil && len(recs) == 1
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)

	recs, err := p.alice.ch.State().SpliceRecords()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	require.NotZero(t, p.alice.wallet.leaseCount())
}

// TestTxSignaturesResentOnce checks our tx_signatures for a splice the
// peer's next_funding names: the link sends them again before anything
// else, and the machine doesn't send them a second time.
func TestTxSignaturesResentOnce(t *testing.T) {
	t.Parallel()

	for _, byLink := range []bool{true, false} {
		h := newAcceptorHarness(t)
		s := splicePastSigning(t, h)
		txid := s.res.Tx.TxHash()

		h.machine.Stop()
		h.machine = NewMachine(h.machine.cfg)
		require.NoError(t, h.machine.Start())
		t.Cleanup(h.machine.Stop)

		if byLink {
			sigs := h.machine.TxSignaturesFor(txid)
			require.NotNil(t, sigs)
			require.Equal(t, txid, sigs.TxID)
		}
		h.machine.HandleReestablish(&lnwire.ChannelReestablish{
			NextFunding: fn.Some(lnwire.FundingTxidFlags{
				Txid: txid,
			}),
		})
		if byLink {
			select {
			case msg := <-h.peer.msgs:
				t.Fatalf("sent %T again", msg)
			case <-time.After(200 * time.Millisecond):
			}
		} else {
			next[*lnwire.TxSignatures](t, h)
		}
	}
}

// TestSpliceShutdownWhileQuiescing checks our splice waiting for quiescence
// while a cooperative close begins: once quiescent, it fails with a
// tx_abort for the peer and the channel resumes, rather than a splice_init
// following our shutdown, which BOLT 2 forbids.
func TestSpliceShutdownWhileQuiescing(t *testing.T) {
	t.Parallel()

	alice, _ := soloAlice(t)
	alice.link.stfuGate = make(chan struct{})
	updates, err := alice.machine.Splice(Request{
		Direction: DirectionOut, Amount: 100_000, FeeRate: 2_500,
		Destination: p2tr(1),
	})
	require.NoError(t, err)
	requireUpdates(t, updates, StatePreparing)
	require.True(t, alice.machine.Busy())
	require.True(t, alice.machine.Preparing())

	require.NoError(t, alice.ch.State().MarkShutdownSent(
		channeldb.NewShutdownInfo(p2tr(2), true),
	))
	close(alice.link.stfuGate)

	u := requireUpdates(t, updates, StateAborted)
	require.Contains(t, u.Reason, codeClosing)
	select {
	case msg := <-alice.peer.msgs:
		require.IsType(t, &lnwire.TxAbort{}, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent")
	}
	select {
	case <-alice.link.resumed:
	case <-time.After(5 * time.Second):
		t.Fatal("not resumed")
	}
	require.False(t, alice.machine.Busy())
}

// TestSpliceRefusedAfterPeerShutdown checks a splice asked for once the
// peer's shutdown has been read, before we answer it and the channel's state
// shows the close: refused with "closing", as the estimate reports it,
// nothing sent and no coin leased. lnd sent stfu and splice_init then, which
// the Core Lightning port refused with a warning.
func TestSpliceRefusedAfterPeerShutdown(t *testing.T) {
	t.Parallel()

	alice, _ := soloAlice(t)
	alice.fund(t, 1, 5_000_000)
	require.Empty(t, alice.machine.Refusal())

	require.False(t, alice.machine.ShutdownReceived())
	require.Equal(t, codeClosing, alice.machine.Refusal())

	_, err := alice.machine.Splice(Request{
		Direction: DirectionIn, Amount: 1_000_000, FeeRate: 2_500,
	})
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, codeClosing)
	require.Zero(t, alice.wallet.leaseCount())
	require.False(t, alice.machine.Busy())
	select {
	case msg := <-alice.peer.msgs:
		t.Fatalf("sent %T", msg)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestSplicePeerShutdownWhileQuiescing checks our splice waiting for
// quiescence when the peer's shutdown arrives: it fails with "closing" at
// once, its coins released. Our stfu still owed, the link withdraws it and
// nothing follows; our stfu out and answered, a tx_abort ends the quiescence
// we won. The shutdown read only as the link withdraws our stfu, the splice
// fails with "closing" too, not as a failure to quiesce.
func TestSplicePeerShutdownWhileQuiescing(t *testing.T) {
	t.Parallel()

	for _, c := range []string{"withdrawn", "answered", "flag only"} {
		t.Run(c, func(t *testing.T) {
			t.Parallel()

			alice, _ := soloAlice(t)
			alice.fund(t, 1, 5_000_000)
			alice.link.stfuGate = make(chan struct{})

			updates, err := alice.machine.Splice(Request{
				Direction: DirectionIn, Amount: 1_000_000,
				FeeRate: 2_500,
			})
			require.NoError(t, err)
			requireUpdates(t, updates, StatePreparing)
			require.NotZero(t, alice.wallet.leaseCount())

			if c == "flag only" {
				alice.machine.mu.Lock()
				alice.machine.peerShutdown = true
				alice.machine.mu.Unlock()
			} else {
				m := alice.machine
				require.True(t, m.ShutdownReceived())
				u := requireUpdates(t, updates, StateAborted)
				require.Contains(t, u.Reason, codeClosing)
				require.Zero(t, alice.wallet.leaseCount())
				require.False(t, alice.machine.Busy())
			}

			if c != "answered" {
				alice.link.stfuErr = errors.New("withdrawn")
			}
			close(alice.link.stfuGate)

			if c == "flag only" {
				u := requireUpdates(t, updates, StateAborted)
				require.Contains(t, u.Reason, codeClosing)
				require.Zero(t, alice.wallet.leaseCount())
			}
			if c != "answered" {
				select {
				case msg := <-alice.peer.msgs:
					t.Fatalf("sent %T", msg)
				case <-alice.link.resumed:
					t.Fatal("resumed quiescence never had")
				case <-time.After(200 * time.Millisecond):
				}

				return
			}

			select {
			case msg := <-alice.peer.msgs:
				abort, ok := msg.(*lnwire.TxAbort)
				require.True(t, ok, "%T", msg)
				require.Equal(t, codeClosing,
					string(abort.Data))
			case <-time.After(5 * time.Second):
				t.Fatal("nothing sent")
			}
			select {
			case <-alice.link.resumed:
			case <-time.After(5 * time.Second):
				t.Fatal("not resumed")
			}
			alice.peer.mu.Lock()
			require.Nil(t, alice.peer.disconnected)
			alice.peer.mu.Unlock()
		})
	}
}

// TestSpliceStfuUnanswered checks a peer that never answers our stfu: past
// the wait, our splice fails, its coins released, and the connection drops,
// which ends the link's request for quiescence; also when the splice was
// canceled meanwhile. The link otherwise sent no update for as long as the
// connection lasted.
func TestSpliceStfuUnanswered(t *testing.T) {
	t.Parallel()

	for _, cancel := range []bool{false, true} {
		alice, _ := soloAlice(t)
		alice.machine.cfg.Policy.NegotiationTimeout = 50 * time.Millisecond
		alice.fund(t, 1, 5_000_000)
		alice.link.stfuGate = make(chan struct{})
		t.Cleanup(func() { close(alice.link.stfuGate) })

		updates, err := alice.machine.Splice(Request{
			Direction: DirectionIn, Amount: 1_000_000,
			FeeRate: 2_500,
		})
		require.NoError(t, err)
		requireUpdates(t, updates, StatePreparing)
		if cancel {
			require.True(t, alice.machine.Cancel())
		}

		u := requireUpdates(t, updates, StateAborted)
		if !cancel {
			require.Contains(t, u.Reason, codeTimeout)
		}
		require.Eventually(t, func() bool {
			alice.peer.mu.Lock()
			defer alice.peer.mu.Unlock()

			return alice.peer.disconnected != nil
		}, 5*time.Second, 10*time.Millisecond, "cancel %v", cancel)
		require.Zero(t, alice.wallet.leaseCount())
	}
}

// TestWithheldInputsWatched checks a splice whose tx_signatures the peer
// withheld, ours out, restored after a restart: its inputs are watched, so a
// conflicting spend makes it dead. It was watched for its confirmation only.
func TestWithheldInputsWatched(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	s := splicePastSigning(t, h)

	h.machine.Stop()
	h.machine = NewMachine(h.machine.cfg)
	require.NoError(t, h.machine.Start())
	t.Cleanup(h.machine.Stop)

	require.Eventually(t, func() bool {
		h.chain.mu.Lock()
		defer h.chain.mu.Unlock()

		return len(h.chain.spends[s.coin.OutPoint]) > 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestWithheldSpliceDead checks a splice whose peer never sent its
// tx_signatures, withheld or disowned, one of whose inputs another
// transaction spent: declared dead at depth, its transaction unsigned,
// where assembling the signed one crashed the node.
func TestWithheldSpliceDead(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	s := splicePastSigning(t, h)

	conflict := wire.NewMsgTx(2)
	conflict.AddTxIn(&wire.TxIn{PreviousOutPoint: s.coin.OutPoint})
	conflict.AddTxOut(&wire.TxOut{Value: 1_000, PkScript: p2tr(3)})
	h.chain.spend(t, s.coin.OutPoint, conflict, 1)
	h.chain.confirm(t, conflict.TxHash(), deadDepth, 1,
		&chainntnfs.TxConfirmation{})

	require.Eventually(t, func() bool {
		last, err := h.bob.State().LastSplice()
		return err == nil && last != nil && last.Outcome == "dead"
	}, 5*time.Second, 10*time.Millisecond)
}

// TestDisownedSpliceDead checks a disowned splice one of whose inputs another
// transaction spent: dead, its funding leaves the set, the peer's batches,
// which leave it out, matching again.
func TestDisownedSpliceDead(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	s := splicePastSigning(t, h)
	h.machine.HandleSpliceMessage(&lnwire.TxAbort{
		ChannelID: h.machine.cfg.ChanID, Data: []byte("user"),
	})
	next[*lnwire.TxAbort](t, h)
	require.Len(t, h.bob.PendingFundings(), 1)

	conflict := wire.NewMsgTx(2)
	conflict.AddTxIn(&wire.TxIn{PreviousOutPoint: s.coin.OutPoint})
	conflict.AddTxOut(&wire.TxOut{Value: 1_000, PkScript: p2tr(3)})
	h.chain.spend(t, s.coin.OutPoint, conflict, 1)
	h.chain.confirm(t, conflict.TxHash(), deadDepth, 1,
		&chainntnfs.TxConfirmation{})

	require.Eventually(t, func() bool {
		return len(h.bob.PendingFundings()) == 0
	}, 5*time.Second, 10*time.Millisecond)
	held, _ := h.machine.Disowned()
	require.False(t, held)
}

// TestDisownedSpliceDeadDroppedAtReestablish checks a dead disowned splice
// whose funding the link wasn't up to drop: dropped at the next
// reestablish.
func TestDisownedSpliceDeadDroppedAtReestablish(t *testing.T) {
	t.Parallel()

	h := newAcceptorHarness(t)
	splicePastSigning(t, h)
	recs, err := h.bob.State().SpliceRecords()
	require.NoError(t, err)
	require.NoError(t, h.bob.State().UpdateSpliceRecord(recs[0].Txid(),
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.Dead = true
			r.Negotiation.Disowned = true

			return nil
		}))
	require.Len(t, h.bob.PendingFundings(), 1)

	h.machine.HandleReestablish(&lnwire.ChannelReestablish{})
	require.Eventually(t, func() bool {
		return len(h.bob.PendingFundings()) == 0
	}, 5*time.Second, 10*time.Millisecond)
}

// TestPeerAtRisk checks the amount the lock depth grows with: the value of
// the peer's inputs, or what it put in if more, by the negotiation's serial
// ids from either side.
func TestPeerAtRisk(t *testing.T) {
	t.Parallel()

	res := &interactivetx.Result{Inputs: []*interactivetx.Input{
		{SerialID: 0, Value: 2_000_000, Shared: true},
		{SerialID: 2, Value: 300_000},
		{SerialID: 1, Value: 5_000_000},
	}}

	// Ours: the peer added 5,000,000 and took back all but 10,000.
	a := &active{
		role: interactivetx.Initiator, res: res,
		theirContribution: 10_000,
	}
	require.Equal(t, btcutil.Amount(5_000_000), peerAtRisk(a))

	// The peer's: its input is the even one.
	a.role = interactivetx.Acceptor
	require.Equal(t, btcutil.Amount(300_000), peerAtRisk(a))

	// Its contribution when more, and never below zero.
	a.theirContribution = 400_000
	require.Equal(t, btcutil.Amount(400_000), peerAtRisk(a))
	a = &active{theirContribution: -1}
	require.Zero(t, peerAtRisk(a))
}
