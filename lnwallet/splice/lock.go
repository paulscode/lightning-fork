package splice

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// A signed splice waits for its confirmations. At our lock depth we send
// splice_locked; once both sides have sent it, the link moves the channel to
// the new funding output. Everything here survives restarts: the machine
// rebuilds the splice in progress from its record.

// peerAtRisk is what of a splice transaction the peer could undo by
// double-spending its inputs in a reorganisation: the value of the inputs
// it added, or what it put in if more. The lock depth grows with it.
func peerAtRisk(a *active) btcutil.Amount {
	risk := max(a.theirContribution, 0)
	if a.res == nil {
		return risk
	}
	var inputs btcutil.Amount
	for _, in := range a.res.Inputs {
		if in.Shared || ourSerial(a.role, in.SerialID) {
			continue
		}
		inputs += in.Value
	}

	return max(risk, inputs)
}

// watchConfirmation waits for the splice to reach our lock depth. Once per
// splice.
func (m *Machine) watchConfirmation(heightHint uint32) {
	m.watchConfirmationOf(m.active, heightHint)
}

// watchConfirmationOf waits for a transaction of the splice, the latest or
// an earlier one a replacement may lose to, to reach our lock depth.
func (m *Machine) watchConfirmationOf(a *active, heightHint uint32) {
	if a.watching {
		return
	}
	heightHint = m.heightHint(heightHint)
	depth := uint32(6)
	if m.cfg.Policy.LockDepth != nil {
		depth = m.cfg.Policy.LockDepth(peerAtRisk(a))
	}

	txid := a.txid
	conf, err := m.cfg.Chain.RegisterConfirmationsNtfn(
		&txid, a.fundingScript, depth, heightHint,
	)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to watch splice %v: %v",
			m.cfg.ChanID, txid, err)
		return
	}
	a.watching = true

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer conf.Cancel()

		select {
		case details, ok := <-conf.Confirmed:
			if !ok {
				return
			}
			scid := lnwire.ShortChannelID{
				BlockHeight: details.BlockHeight,
				TxIndex:     details.TxIndex,
				TxPosition:  uint16(a.fundingIndex),
			}
			m.enqueue(func() { m.reachedDepth(txid, scid) })

		case <-m.quit:
		}
	}()
}

// heightHint returns where the chain is searched from for a transaction of
// the splice: the height recorded at its broadcast or, that unknown (zero: a
// crash between publishing and recording it, or the chain's height unknown
// then), the current funding's confirmation, as the splice spends that
// output. The notifier refuses a zero hint, and the splice then never
// locked.
func (m *Machine) heightHint(h uint32) uint32 {
	if h != 0 {
		return h
	}
	if h = m.cfg.Channel.State().CurrentShortChanID().BlockHeight; h != 0 {
		return h
	}

	return 1
}

// watchWithheld watches a splice whose tx_signatures we sent and the peer
// didn't: it holds ours, so it can publish the splice, and we must follow
// it to its lock if it does. The search starts at the current funding's
// confirmation: the splice spends that output.
func (m *Machine) watchWithheld() {
	height := m.cfg.Channel.State().CurrentShortChanID().BlockHeight
	m.watchConfirmation(height)
	m.watchInputsOf(m.active, height)
}

// reachedDepth sends splice_locked once the splice is deep enough.
func (m *Machine) reachedDepth(txid chainhash.Hash,
	scid lnwire.ShortChannelID) {

	a := m.active
	if a == nil {
		return
	}
	if a.txid != txid {
		// An earlier transaction of the splice won over its
		// replacement.
		if a = m.adopt(txid); a == nil {
			return
		}
	}

	// Confirmed without the peer's tx_signatures reaching us: the peer
	// published it with ours, and kept its own or sent them while we
	// were apart (they are taken if they come). The splice goes on to
	// its lock; an honest peer can't be told from one that withheld them,
	// and the latter gains nothing, so neither is marked.
	if a.phase == phaseSigning {
		log.Warnf("ChannelID(%v): splice %v confirmed without the "+
			"peer's tx_signatures", m.cfg.ChanID, txid)
		m.update(func(a *active) { a.phase = phasePending })
		m.resume()
	}

	err := m.cfg.Channel.State().UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.LockedSent = true
			r.Negotiation.LockedScid = scid
			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record splice_locked: "+
			"%v", m.cfg.ChanID, err)
		return
	}
	m.update(func(a *active) {
		a.lockedSent = true
		a.scid = fn.Some(scid)
	})

	// Once per connection: the peer has it from our channel_reestablish,
	// or from the message. Before the reestablish it waits for it. Sent
	// on an earlier connection doesn't count: an earlier transaction of
	// the splice adopted again after a restart wasn't named.
	m.mu.Lock()
	tell := m.reestablished && m.lockedTold != txid
	if tell {
		m.lockedTold = txid
	}
	m.mu.Unlock()
	if tell {
		m.send(&lnwire.SpliceLocked{
			ChannelID: m.cfg.ChanID, SpliceTxid: txid,
		})
	}

	m.maybeComplete()
}

// handleSpliceLocked takes the peer's splice_locked.
func (m *Machine) handleSpliceLocked(txid chainhash.Hash) {
	a := m.active
	switch {
	case a == nil:
		// Already completed: the peer sent it again on reconnecting.
		return

	// For an earlier transaction of the splice: noted, the lock waits
	// for our own depth.
	case a.txid != txid && m.candidate(txid) != nil:
		m.candidateLocked(m.candidate(txid))
		return

	// For the splice the channel locked to before this one: again.
	case txid == m.cfg.Channel.State().CurrentFundingOutpoint().Hash:
		return

	// A peer withholding its tx_signatures can still publish the splice
	// and lock it: ours are out.
	case a.txid != txid ||
		(a.phase != phasePending && a.ourTxSigs == nil):

		m.warnAndDisconnect("splice_locked for %v, not the pending "+
			"splice", txid)
		return

	case a.lockedReceived:
		return
	}

	err := m.cfg.Channel.State().UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.LockedReceived = true
			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record the peer's "+
			"splice_locked: %v", m.cfg.ChanID, err)
		return
	}
	m.update(func(a *active) { a.lockedReceived = true })

	m.maybeComplete()
}

// maybeComplete moves the channel to the splice's funding output once both
// sides have locked it.
func (m *Machine) maybeComplete() {
	a := m.active
	if !a.lockedSent || !a.lockedReceived {
		return
	}
	scid, err := a.scid.UnwrapOrErr(errors.New("no scid"))
	if err != nil {
		return
	}

	// Held before the link moves to the new funding, so no HTLC of the
	// peer's sees the coins unheld.
	m.holdPeerInput(
		a.txid, a.theirContribution, a.fundingScript, scid.BlockHeight,
	)
	if err := m.wait(m.cfg.Link.LockSplice(a.txid, scid)); err != nil {
		log.Errorf("ChannelID(%v): unable to complete splice %v: %v",
			m.cfg.ChanID, a.txid, err)

		// The hold goes only if the lock didn't happen: stopping, or
		// the channel on the splice's funding all the same, it is
		// the next machine's to keep.
		locked := m.cfg.Channel.State().CurrentFundingOutpoint().Hash ==
			a.txid
		if !errors.Is(err, errStopping) && !locked {
			m.releaseHold(a.txid)
		}

		return
	}
	log.Infof("ChannelID(%v): splice %v locked at %v", m.cfg.ChanID,
		a.txid, scid)

	if len(a.ourInputs) > 0 {
		if err := ReleaseCoins(m.cfg.Wallet, a.ourInputs); err != nil {
			log.Errorf("ChannelID(%v): unable to release coins: %v",
				m.cfg.ChanID, err)
		}
	}
	m.recordLast("locked", "", false)
	m.notify("locked", a.txid, "", false)
	m.report(Update{State: StateLocked, Txid: a.txid})
	m.update(func(a *active) { a.lockDone = true })
	m.setActive(nil)
}

// ReestablishTLVs returns our splice fields for channel_reestablish.
func (m *Machine) ReestablishTLVs() (fn.Option[lnwire.FundingTxidFlags],
	fn.Option[lnwire.FundingTxidFlags]) {

	m.mu.Lock()
	defer m.mu.Unlock()

	var next fn.Option[lnwire.FundingTxidFlags]
	locked := fn.Some(lnwire.FundingTxidFlags{
		Txid: m.cfg.Channel.State().CurrentFundingOutpoint().Hash,
	})

	a := m.active
	switch {
	case a == nil:

	// Our commitment_signed went out and the peer's tx_signatures
	// haven't come: next_funding, asking for its commitment_signed if
	// that hasn't come either.
	case a.phase == phaseSigning && a.commitSent && !a.disowned:
		var flags uint8
		if !a.commitReceived {
			flags |= lnwire.RetransmitCommitSig
		}
		next = fn.Some(lnwire.FundingTxidFlags{
			Txid: a.txid, RetransmitFlags: flags,
		})
		m.namedNext = a.txid

	case a.phase == phasePending && a.lockedSent:
		locked = fn.Some(lnwire.FundingTxidFlags{Txid: a.txid})
		m.lockedTold = a.txid
	}

	// The announcement on the funding a splice moved the channel to, if
	// the peer's half hasn't come.
	state := m.cfg.Channel.State()
	current := state.CurrentFundingOutpoint().Hash
	moved := current != state.FundingOutpoint.Hash
	locked = fn.MapOption(func(
		f lnwire.FundingTxidFlags) lnwire.FundingTxidFlags {

		if moved && f.Txid == current &&
			m.cfg.AnnouncementPending != nil &&
			m.cfg.AnnouncementPending() {

			f.RetransmitFlags |= lnwire.RetransmitAnnounceSigs
		}

		return f
	})(locked)

	return next, locked
}

// TxSignaturesFor returns our tx_signatures for the splice transaction
// txid, the latest or an earlier one, if they went out, for the link to send
// again on reconnecting before anything else: the Core Lightning port takes
// nothing else until it has them. Noted, so they aren't sent twice. Safe
// from any goroutine.
func (m *Machine) TxSignaturesFor(txid chainhash.Hash) *lnwire.TxSignatures {
	m.mu.Lock()
	defer m.mu.Unlock()

	a := m.active
	if a == nil {
		return nil
	}
	var sigs *lnwire.TxSignatures
	if a.txid == txid {
		sigs = a.ourTxSigs
	}
	for _, c := range a.replaced {
		if c.txid == txid {
			sigs = c.ourTxSigs
		}
	}
	if sigs != nil {
		m.sigsResent = txid
	}

	return sigs
}

// sigsSentAgain reports whether the link sent our tx_signatures for txid
// again on this connection.
func (m *Machine) sigsSentAgain(txid chainhash.Hash) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.sigsResent == txid
}

// HandleReestablish takes the peer's channel_reestablish.
func (m *Machine) HandleReestablish(msg *lnwire.ChannelReestablish) {
	m.enqueue(func() { m.handleReestablish(msg) })
}

// handleReestablish resends what the peer's next_funding asks for, refuses
// a next_funding we don't know, and treats my_current_funding_locked naming
// the pending splice as its splice_locked.
func (m *Machine) handleReestablish(msg *lnwire.ChannelReestablish) {
	// A splice still negotiating can't survive the disconnection, nor
	// one restored without our commitment_signed. Dropped without a
	// tx_abort of its own, which before the peer's channel_reestablish
	// fails its link: a next_funding naming it gets one below.
	if m.active != nil && (m.active.phase == phaseNegotiating ||
		m.active.dropAtReestablish) {

		m.forget()
	}

	// A splice being signed, ours not out, that the peer's next_funding
	// doesn't name: the peer doesn't have it, or it would be waiting for
	// our signatures and name it. It can't have signed what it doesn't
	// have, so the splice is dropped.
	if a := m.active; a != nil && a.phase == phaseSigning &&
		a.ourTxSigs == nil {

		named := fn.MapOptionZ(msg.NextFunding,
			func(f lnwire.FundingTxidFlags) bool {
				return f.Txid == a.txid
			})
		if !named {
			log.Infof("ChannelID(%v): splice %v unknown to the "+
				"peer on reconnecting, dropped", m.cfg.ChanID,
				a.txid)
			m.forget()
		}
	}
	a := m.active
	m.dropDeadDisowned()

	// Disowned on an earlier connection (the link holds our updates from
	// its start): the close may not have started, stopped between the
	// record and the request.
	if a != nil && a.disowned {
		m.closeDisowned()
	}

	msg.NextFunding.WhenSome(func(next lnwire.FundingTxidFlags) {
		current := m.cfg.Channel.State().CurrentFundingOutpoint().Hash
		switch {
		// An earlier transaction of the splice: our tx_signatures
		// for it went out, so it gets them again, never a tx_abort
		// (the Core Lightning port fails the channel on one once it
		// signed).
		case a != nil && a.txid != next.Txid &&
			m.candidate(next.Txid) != nil:

			c := m.candidate(next.Txid)
			if c.ourTxSigs != nil && !m.sigsSentAgain(c.txid) {
				m.send(c.ourTxSigs)
			}

		// The splice the channel has locked to since: it is done.
		case next.Txid == current:
			log.Debugf("ChannelID(%v): next_funding names the "+
				"current funding %v", m.cfg.ChanID, current)

		case a == nil || a.txid != next.Txid:
			m.abortSent = true
			m.send(&lnwire.TxAbort{
				ChannelID: m.cfg.ChanID,
				Data:      []byte("unknown funding transaction"),
			})

		// Ours already went out: again, unless the link did.
		case a.ourTxSigs != nil:
			if !m.sigsSentAgain(a.txid) {
				m.send(a.ourTxSigs)
			}

		// Our turn now that both commitments are in.
		default:
			m.maybeSendTxSignatures()
		}
	})

	// Our splice_locked, if due and not named by our reestablish, goes
	// now.
	m.mu.Lock()
	m.reestablished = true
	tell := a != nil && a.phase == phasePending && a.lockedSent &&
		m.lockedTold != a.txid
	if tell {
		m.lockedTold = a.txid
	}
	m.mu.Unlock()
	if tell {
		m.send(&lnwire.SpliceLocked{
			ChannelID: m.cfg.ChanID, SpliceTxid: a.txid,
		})
	}

	msg.MyCurrentFundingLocked.WhenSome(func(f lnwire.FundingTxidFlags) {
		// As splice_locked: also while the peer's tx_signatures,
		// lost on the way, are still to come again, ours out.
		if a != nil && a.txid == f.Txid &&
			(a.phase == phasePending || a.ourTxSigs != nil) {

			m.handleSpliceLocked(f.Txid)
		}

		// The peer locked an earlier transaction of the splice: noted
		// as its splice_locked would be.
		if c := m.candidate(f.Txid); c != nil {
			m.candidateLocked(c)
		}

		// The peer asks for our half of the announcement on the
		// funding a splice moved the channel to.
		state := m.cfg.Channel.State()
		current := state.CurrentFundingOutpoint().Hash
		if f.RetransmitFlags&lnwire.RetransmitAnnounceSigs != 0 &&
			f.Txid == current &&
			current != state.FundingOutpoint.Hash &&
			m.cfg.ResendAnnouncement != nil {

			m.cfg.ResendAnnouncement()
		}
	})

	// Both splice_locked recorded before a restart, the lock not done:
	// done now, its short channel id recorded with ours.
	if a := m.active; a != nil && a.phase == phasePending {
		m.maybeComplete()
	}
}

// restore rebuilds the splice in progress from the channel's records after a
// restart.
func (m *Machine) restore() error {
	records, err := m.cfg.Channel.State().SpliceRecords()
	if err != nil {
		return err
	}

	// The splice's transactions: one, or several when the peer replaced
	// it, the latest paying the highest fee rate.
	var (
		attempts []*active

		// The splice's last attempt agreed, dead ones included: a
		// replacement inherits its values (bolts#20).
		lastAgreed *channeldb.SpliceRecord
	)
	for _, r := range records {
		// A dead one can't confirm: nothing to follow, though its
		// funding stays in the channel's set.
		n := r.Negotiation
		if r.Retired || n == nil || n.Tx == nil {
			continue
		}
		if lastAgreed == nil ||
			n.AgreedAt > lastAgreed.Negotiation.AgreedAt {

			lastAgreed = r
		}
		if n.Dead {
			m.restoreDead(r)
			continue
		}
		a, err := restoredActive(r)
		if err != nil {
			return err
		}
		attempts = append(attempts, a)
	}
	if len(attempts) == 0 {
		return nil
	}
	// The latest is the one not yet signed both ways, if any (only the
	// latest can be); else the highest fee rate, then the one agreed
	// last, then the highest fee. Equal ones were both signed and
	// published: either does.
	sort.SliceStable(attempts, func(i, j int) bool {
		si, sj := signedBoth(attempts[i]), signedBoth(attempts[j])
		ai, aj := attempts[i].agreedAt, attempts[j].agreedAt
		switch {
		case si != sj:
			return si
		case attempts[i].feeRate != attempts[j].feeRate:
			return attempts[i].feeRate < attempts[j].feeRate
		case !ai.IsZero() && !aj.IsZero() && !ai.Equal(aj):
			return ai.Before(aj)
		default:
			return attempts[i].res.Fee < attempts[j].res.Fee
		}
	})
	a := attempts[len(attempts)-1]
	for _, c := range attempts[:len(attempts)-1] {
		// Only a transaction signed both ways was replaced.
		if c.ourTxSigs == nil || c.theirTxSigs == nil {
			log.Warnf("ChannelID(%v): splice %v replaced unsigned, "+
				"left out", m.cfg.ChanID, c.txid)
			continue
		}
		c.phase = phasePending
		a.replaced = append(a.replaced, c)
	}

	// Agreed after it, a later attempt was dropped or died: still the
	// one a replacement inherits from.
	if n := lastAgreed.Negotiation; lastAgreed.Txid() != a.txid &&
		n.AgreedAt != 0 && time.Unix(n.AgreedAt, 0).After(a.agreedAt) {

		a.lastAgreed = &inFlightPair{
			ours:   n.OurMaxInFlight,
			theirs: n.TheirMaxInFlight,
		}
	}

	m.active = a
	if a.role == interactivetx.Acceptor && !a.oursReplaced {
		m.cfg.Limits.admit(m, m.cfg.ChanID, m.cfg.TheirNodeID, true)
	}
	for _, c := range a.replaced {
		m.watchConfirmationOf(c, c.broadcastHeight)
	}

	switch {
	case a.ourTxSigs != nil && a.theirTxSigs != nil:
		a.phase = phasePending
		m.resumePending(a.broadcastHeight)

	// Ours went out and the peer's never came: it may publish the
	// splice. Once we had locked it, it is pending (we owe splice_locked
	// on reconnecting, not next_funding).
	case a.ourTxSigs != nil:
		if a.lockedSent {
			a.phase = phasePending
		}
		m.watchWithheld()

		// The peer's may come once it reconnects; if not by the
		// timeout, the place it holds is given back.
		if a.phase == phaseSigning {
			m.startSigningTimeout()
		}

	// Our commitment_signed for it was never saved: we stopped between
	// recording the splice and signing. Nothing of it is signed, and
	// the channel can't take a commitment batch without ours, so it is
	// dropped once the peer has reestablished.
	case !a.commitSent:
		a.dropAtReestablish = true

	// Neither side's signatures: the peer has until the timeout to go on
	// with it after reconnecting.
	default:
		m.startSigningTimeout()
	}

	return nil
}

// restoreDead watches a dead splice transaction's inputs again while the
// wallet still holds it: the conflicting spend, deep enough, takes it out of
// the wallet and releases its coins, as before the restart. Without it, a
// restart between the splice's death and that depth left its coins counted
// as spent for good.
func (m *Machine) restoreDead(r *channeldb.SpliceRecord) {
	txid := r.Txid()
	a, err := restoredActive(r)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to read dead splice %v: %v",
			m.cfg.ChanID, txid, err)
		return
	}

	// Taken out of the wallet at depth, while the wallet holds it. Its
	// inputs are watched either way: a reorganisation taking the
	// conflicting spend out may confirm it after all (revive).
	if m.cfg.CoinSource != nil {
		if _, err := m.cfg.CoinSource.FetchTx(txid); err == nil {
			m.dead[txid] = deadSplice{
				tx: r.Negotiation.Tx, inputs: a.ourInputs,
			}
		}
	}

	height := m.heightHint(r.Negotiation.BroadcastHeight)
	for _, in := range a.res.Inputs {
		if in.Shared {
			continue
		}
		m.wg.Add(1)
		go m.watchInput(txid, in.OutPoint, in.PkScript, height)
	}
}

// signedBoth reports whether both sides' tx_signatures are in.
func signedBoth(a *active) bool {
	return a.ourTxSigs != nil && a.theirTxSigs != nil
}

// restoredActive rebuilds a splice transaction from its record.
func restoredActive(r *channeldb.SpliceRecord) (*active, error) {
	n := r.Negotiation
	res := &interactivetx.Result{
		Tx:           n.Tx,
		FundingIndex: r.OutPoint.Index,
	}
	for i, in := range n.Inputs {
		if i >= len(n.Tx.TxIn) {
			return nil, errors.New("splice record inputs don't " +
				"match its transaction")
		}
		res.Inputs = append(res.Inputs, &interactivetx.Input{
			SerialID: in.SerialID,
			OutPoint: n.Tx.TxIn[i].PreviousOutPoint,
			Value:    in.Value,
			PkScript: in.PkScript,
			Shared:   in.Shared,
		})
		if in.Shared {
			res.SharedIndex = uint32(i)
		}
	}

	witness, out, err := input.GenFundingPkScript(
		r.LocalKey.PubKey.SerializeCompressed(),
		r.RemoteKey.SerializeCompressed(), int64(r.Capacity),
	)
	if err != nil {
		return nil, err
	}

	role := interactivetx.Acceptor
	if n.WeInitiated {
		role = interactivetx.Initiator
	}
	a := &active{
		phase:             phaseSigning,
		role:              role,
		res:               res,
		txid:              r.Txid(),
		fundingIndex:      r.OutPoint.Index,
		fundingScript:     out.PkScript,
		fundingWitness:    witness,
		ourKey:            r.LocalKey,
		theirKey:          r.RemoteKey,
		ourContribution:   r.LocalDelta,
		theirContribution: r.RemoteDelta,
		capacity:          r.Capacity,
		feeRate:           chainfee.SatPerKWeight(n.FeeRate),
		commitSent:        r.SpliceSig != nil,
		commitReceived:    r.LocalCommit != nil,
		ourTxSigs:         n.OurTxSignatures,
		theirTxSigs:       n.TheirTxSignatures,
		lockedSent:        n.LockedSent,
		lockedReceived:    n.LockedReceived,
		broadcastHeight:   n.BroadcastHeight,
		ourMaxInFlight:    n.OurMaxInFlight,
		theirMaxInFlight:  n.TheirMaxInFlight,
	}
	if n.AgreedAt != 0 {
		a.agreedAt = time.Unix(n.AgreedAt, 0)
	}
	a.disowned = n.Disowned
	if n.SignFailedAt != 0 {
		a.signFailedAt = time.Unix(n.SignFailedAt, 0)
	}
	a.signFailures = n.SignFailures
	if n.LockedSent && n.LockedScid.ToUint64() != 0 {
		a.scid = fn.Some(n.LockedScid)
	}
	for _, op := range n.OurInputs {
		a.ourInputs = append(a.ourInputs, Coin{OutPoint: op})
	}
	a.oursReplaced = n.OursReplaced
	for _, o := range n.OurOutputs {
		a.ourOutputs = append(a.ourOutputs, interactivetx.Output{
			Value: btcutil.Amount(o.Value), PkScript: o.PkScript,
		})
	}

	// The fee, which a replacement must at least pay.
	var paid, kept int64
	for _, i := range n.Inputs {
		paid += int64(i.Value)
	}
	for _, o := range n.Tx.TxOut {
		kept += o.Value
	}
	res.Fee = btcutil.Amount(paid - kept)

	return a, nil
}

// resumePending picks a signed splice up again after a restart: published
// again (the mempool may have dropped it) and watched until it locks.
func (m *Machine) resumePending(broadcastHeight uint32) {
	a := m.active
	tx, err := m.signedTx()
	if err == nil {
		m.publish(tx)
	} else {
		log.Errorf("ChannelID(%v): unable to assemble splice %v: %v",
			m.cfg.ChanID, a.txid, err)
	}

	m.watchConfirmation(broadcastHeight)
	m.watchPending(broadcastHeight)
}

// spliceInputs returns the transaction's inputs as the record keeps them.
func spliceInputs(res *interactivetx.Result) []channeldb.SpliceInput {
	inputs := make([]channeldb.SpliceInput, len(res.Inputs))
	for i, in := range res.Inputs {
		inputs[i] = channeldb.SpliceInput{
			SerialID: in.SerialID,
			Value:    in.Value,
			PkScript: in.PkScript,
			Shared:   in.Shared,
		}
	}

	return inputs
}

// Depths at which a splice whose input was spent elsewhere is given up on,
// and at which its trace leaves the wallet.
const (
	deadDepth   = 6
	forgetDepth = 144
)

// watchPending starts what a published splice needs until it locks: it is
// published again at every block until it confirms (an evicted splice would
// otherwise only go out again at restart), and its inputs are watched for a
// spend by another transaction, which kills it.
func (m *Machine) watchPending(heightHint uint32) {
	m.watchPendingOf(m.active, heightHint)
}

// watchPendingOf starts watchPending's work for a transaction of the
// splice, once.
func (m *Machine) watchPendingOf(a *active, heightHint uint32) {
	if a.pendingWatched {
		return
	}
	a.pendingWatched = true
	heightHint = m.heightHint(heightHint)
	txid := a.txid

	m.wg.Add(1)
	go m.rebroadcastUntilConfirmed(txid, a.fundingScript, heightHint)

	m.watchInputsOf(a, heightHint)
}

// watchInputsOf watches a transaction of the splice's inputs for a spend
// by another, once: also one whose tx_signatures the peer withheld, which it
// can still publish and which its inputs can kill. The search starts no
// later than the negotiation (the transaction's locktime): a conflicting
// spend confirmed while it was signed isn't found from the broadcast on by
// a backend without a full index.
func (m *Machine) watchInputsOf(a *active, heightHint uint32) {
	if a == nil || a.inputsWatched || a.res == nil {
		return
	}
	a.inputsWatched = true

	// The locktime is the peer's to choose when it built the splice: no
	// earlier than the current funding's confirmation, which the splice
	// spends, or a locktime of 1 had every input searched for from the
	// chain's start, at every restart.
	if lt := a.res.Tx.LockTime; lt > 0 && lt < heightHint {
		floor := m.cfg.Channel.State().CurrentShortChanID().BlockHeight
		heightHint = max(lt, floor)
	}
	heightHint = m.heightHint(heightHint)

	for _, in := range a.res.Inputs {
		m.wg.Add(1)
		go m.watchInput(a.txid, in.OutPoint, in.PkScript, heightHint)
	}
}

// rebroadcastUntilConfirmed publishes the splice again at every block until
// its first confirmation.
func (m *Machine) rebroadcastUntilConfirmed(txid chainhash.Hash,
	script []byte, heightHint uint32) {

	defer m.wg.Done()

	conf, err := m.cfg.Chain.RegisterConfirmationsNtfn(
		&txid, script, 1, heightHint,
	)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to watch splice %v: %v",
			m.cfg.ChanID, txid, err)
		return
	}
	defer conf.Cancel()

	epochs, err := m.cfg.Chain.RegisterBlockEpochNtfn(nil)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to watch blocks: %v",
			m.cfg.ChanID, err)
		return
	}
	defer epochs.Cancel()

	for {
		select {
		case _, ok := <-epochs.Epochs:
			if !ok {
				return
			}
			m.enqueue(func() {
				a := m.active
				if a == nil || a.txid != txid ||
					a.phase != phasePending {

					return
				}
				tx, err := m.signedTx()
				if err != nil {
					return
				}
				status := m.publish(tx)
				if status.State == RelayRejected {
					log.Warnf("ChannelID(%v): splice %v "+
						"not relayed: %v", m.cfg.ChanID,
						txid, status.Reason)
				}
			})

		case <-conf.Confirmed:
			return

		case <-m.quit:
			return
		}
	}
}

// watchInput watches an input of the splice. Spent by the splice, it is
// done; spent by anything else, the splice is dead once that spend is
// deadDepth deep, and leaves the wallet once forgetDepth deep. A reorg
// taking the other spend out puts the input back under watch.
func (m *Machine) watchInput(txid chainhash.Hash, op wire.OutPoint,
	script []byte, heightHint uint32) {

	defer m.wg.Done()

	for {
		spendNtfn, err := m.cfg.Chain.RegisterSpendNtfn(
			&op, script, heightHint,
		)
		if err != nil {
			log.Errorf("ChannelID(%v): unable to watch splice input "+
				"%v: %v", m.cfg.ChanID, op, err)
			return
		}

		var spend *chainntnfs.SpendDetail
		select {
		case s, ok := <-spendNtfn.Spend:
			spendNtfn.Cancel()
			if !ok {
				return
			}
			spend = s
		case <-m.quit:
			spendNtfn.Cancel()
			return
		}
		// The splice itself. Declared dead before, a reorganisation
		// took the conflicting spend out and put it in: followed
		// again.
		// Watched until reorg-safe: taken out again and a conflicting
		// spend confirmed instead, the splice is dead after all.
		if *spend.SpenderTxHash == txid {
			m.enqueue(func() { m.revive(txid) })

			_, reorged := m.waitDepth(
				txid, spend.SpendingTx.TxOut[0].PkScript,
				forgetDepth, uint32(spend.SpendingHeight),
			)
			if reorged {
				continue
			}

			return
		}

		conflict := *spend.SpenderTxHash
		height := uint32(spend.SpendingHeight)
		outScript := spend.SpendingTx.TxOut[0].PkScript

		dead, reorged := m.waitDepth(conflict, outScript, deadDepth,
			height)
		if reorged {
			continue
		}
		if !dead {
			return
		}
		m.enqueue(func() { m.spliceDead(txid, conflict, op) })

		forget, reorged := m.waitDepth(conflict, outScript, forgetDepth,
			height)
		if reorged {
			continue
		}
		if forget {
			m.enqueue(func() { m.forgetDead(txid) })
		}

		return
	}
}

// waitDepth waits for a transaction to reach a depth: true when it does,
// reorged when a reorg takes it out first, both false when stopping.
func (m *Machine) waitDepth(txid chainhash.Hash, script []byte,
	depth, heightHint uint32) (bool, bool) {

	conf, err := m.cfg.Chain.RegisterConfirmationsNtfn(
		&txid, script, depth, heightHint,
	)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to watch %v: %v",
			m.cfg.ChanID, txid, err)
		return false, false
	}
	defer conf.Cancel()

	select {
	case <-conf.Confirmed:
		return true, false
	case <-conf.NegativeConf:
		return false, true
	case <-m.quit:
		return false, false
	}
}

// spliceDead gives up on a splice one of whose inputs another transaction
// spent: recorded dead, its coins and wallet trace waiting for forgetDead.
// Its funding stays in the channel's set, signed for, until the channel
// closes: the peer keeps it as well (the Core Lightning port drops a
// splice's funding only when another locks), and a commitment batch short
// of it fails the peer's channel.
func (m *Machine) spliceDead(txid, conflict chainhash.Hash,
	op wire.OutPoint) {

	a := m.active
	switch {
	case a == nil:
		return

	// Spent by another transaction of the same splice: that one is the
	// splice, adopted at its lock depth.
	case conflict == a.txid || m.candidate(conflict) != nil:
		return

	// An earlier transaction of the splice spent elsewhere: no longer a
	// candidate.
	case m.candidate(txid) != nil:
		m.dropCandidate(m.candidate(txid))
		return

	case a.txid != txid:
		return
	}

	// The latest transaction is dead; an earlier one that doesn't spend
	// that input is the splice again.
	if prev := m.liveCandidate(op); prev != nil {
		m.fallBackTo(prev, op)
		return
	}

	tx, err := m.signedTx()
	if err != nil {
		tx = a.res.Tx
	}
	m.dead[txid] = deadSplice{tx: tx, inputs: a.ourInputs}
	m.markDead(txid)

	reason := fmt.Sprintf("dead: an input was spent by %v", conflict)
	log.Warnf("ChannelID(%v): splice %v is %v", m.cfg.ChanID, txid,
		reason)
	m.recordLast("dead", reason, false)
	m.notify("dead", txid, reason, false)
	m.report(Update{State: StateAborted, Txid: txid, Reason: reason})
	m.setActive(nil)

	if a.disowned {
		m.dropDeadDisowned()
	}
}

// dropDeadDisowned takes the funding of each dead splice the peer disowned
// out of the channel's set. The peer doesn't have it, its batches leaving it
// out, and dead, it can't confirm: kept, as other dead splices are, the
// batches would never match. Dropped, a close under way can finish, and a
// channel whose close never started works again. Also at each reestablish:
// dropped through the link, which may not have been up.
func (m *Machine) dropDeadDisowned() {
	records, err := m.cfg.Channel.State().SpliceRecords()
	if err != nil {
		log.Errorf("ChannelID(%v): unable to read splices: %v",
			m.cfg.ChanID, err)
		return
	}
	for _, r := range records {
		n := r.Negotiation
		if r.Retired || n == nil || !n.Dead || !n.Disowned {
			continue
		}
		txid := r.Txid()
		if err := m.wait(m.cfg.Link.RemoveSpliceFunding(txid)); err != nil {
			log.Warnf("ChannelID(%v): unable to drop dead disowned "+
				"splice %v: %v", m.cfg.ChanID, txid, err)
			continue
		}
		log.Infof("ChannelID(%v): dead disowned splice %v dropped",
			m.cfg.ChanID, txid)
	}
}

// revive follows again a transaction of the splice recorded dead that
// confirmed after all, a reorganisation having taken the conflicting spend
// out: its funding stayed in the channel's set, signed for, so it locks as
// any. Followed by nothing before, the channel never locked and kept
// committing on an output already spent.
func (m *Machine) revive(txid chainhash.Hash) {
	records, err := m.cfg.Channel.State().SpliceRecords()
	if err != nil {
		log.Errorf("ChannelID(%v): unable to read splices: %v",
			m.cfg.ChanID, err)
		return
	}
	var rec *channeldb.SpliceRecord
	for _, r := range records {
		if r.Txid() == txid && !r.Retired {
			rec = r
		}
	}
	if rec == nil || rec.Negotiation == nil || !rec.Negotiation.Dead {
		return
	}
	a, err := restoredActive(rec)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to read splice %v: %v",
			m.cfg.ChanID, txid, err)
		return
	}
	err = m.cfg.Channel.State().UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.Dead = false
			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to revive splice %v: %v",
			m.cfg.ChanID, txid, err)
		return
	}
	delete(m.dead, txid)
	a.phase = phasePending
	log.Warnf("ChannelID(%v): splice %v recorded dead confirmed after "+
		"all; followed again", m.cfg.ChanID, txid)

	m.mu.Lock()
	if m.active == nil {
		m.active = a
	} else {
		m.active.replaced = append(m.active.replaced, a)
	}
	m.mu.Unlock()
	m.watchConfirmationOf(a, m.heightHint(rec.Negotiation.BroadcastHeight))
}

// markDead records a transaction of the splice as dead. Its funding stays
// in the channel's set: see spliceDead.
func (m *Machine) markDead(txid chainhash.Hash) {
	err := m.cfg.Channel.State().UpdateSpliceRecord(txid,
		func(r *channeldb.SpliceRecord) error {
			if r.Negotiation != nil {
				r.Negotiation.Dead = true
			}

			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record splice %v dead: %v",
			m.cfg.ChanID, txid, err)
	}
}

// forgetDead takes a dead splice out of the wallet, so the coins it spent
// count again, and releases its leases.
func (m *Machine) forgetDead(txid chainhash.Hash) {
	d, ok := m.dead[txid]
	if !ok {
		return
	}
	delete(m.dead, txid)

	if err := Forget(m.cfg.Wallet, d.tx, d.inputs); err != nil {
		log.Errorf("ChannelID(%v): unable to forget dead splice %v: %v",
			m.cfg.ChanID, txid, err)
	}
}
