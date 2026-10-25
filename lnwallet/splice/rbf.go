package splice

import (
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwallet"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// A peer may replace a splice that is signed both ways and not yet locked,
// with a transaction paying at least as much (tx_init_rbf). This node takes
// part as it did in the splice: adding nothing to the peer's, and to its
// own the same coins, or the same amount to the same destination. The
// splice's earlier transactions stay candidates until one of them, or the
// replacement, confirms: they all spend the same funding output, so only
// one can. This node doesn't start replacements yet.

// The replacement must pay at least the fee of the transaction it replaces,
// in total: checked once its construction is done (checkReplacementFee).

// maxReplacements bounds the replacements of one splice, each of which is
// another candidate funding the channel signs commitments for.
const maxReplacements = 10

// handleTxInitRbf answers the peer's tx_init_rbf: refused with tx_abort, or
// accepted with tx_ack_rbf and the replacement's construction started.
func (m *Machine) handleTxInitRbf(msg *lnwire.TxInitRbf) {
	initiator, err := m.cfg.Link.QuiescenceInitiator().Unpack()
	if err != nil || initiator != lntypes.Remote {
		m.warnAndDisconnect("tx_init_rbf without the peer having " +
			"started quiescence")
		return
	}
	// After shutdown, as for splice_init.
	if m.shutdownReceived() {
		m.warnAndDisconnect("tx_init_rbf after shutdown")
		return
	}

	// A splice still negotiated or signed can't be replaced, and a
	// tx_abort would end it: the peer broke the protocol.
	a := m.active
	if a != nil && a.phase != phasePending {
		m.warnAndDisconnect("tx_init_rbf while the splice is being " +
			"negotiated or signed")
		return
	}
	if code, why := m.checkTxInitRbf(msg); code != "" {
		m.cfg.Limits.noteAbort(m.cfg.ChanID)
		m.abort(code, "%s", why)
		m.resume()

		return
	}

	// The peer's contribution it may change; absent, the peer puts in
	// nothing, as the spec reads (the Core Lightning port always sends
	// it).
	contribution := msg.FundingOutputContribution.UnwrapOr(0)
	feeRate := chainfee.SatPerKWeight(msg.FeeRatePerKw)

	// Replacing a splice we started, we put in what we did: the same
	// coins, or the same amount to the same destination, paying for
	// them at the new rate.
	var (
		ours    btcutil.Amount
		coins   []Coin
		outputs []interactivetx.Output
	)
	if weStarted(a) {
		if limit := m.ourRateLimit(a); feeRate > limit {
			m.abort(codeFeeBelowFloor, "%v is above the %v we pay "+
				"for our part of a replacement", feeRate, limit)
			m.resume()

			return
		}
		ours, coins, outputs, err = m.ourPart(a, feeRate)
		if err != nil {
			m.abort(codeInsufficientFunds, "%v", err)
			m.resume()

			return
		}
	}

	// The size limit counts what both put in.
	capacity := m.cfg.Channel.State().Capacity + contribution + ours
	if size := m.cfg.Policy.MaxChannelSize; size > 0 && capacity > size {
		m.abort(codeAboveMaxSize, "capacity %v above %v", capacity, size)
		m.resume()

		return
	}

	session, b, err := m.newConstruction(
		interactivetx.Acceptor, contribution, ours, feeRate,
		msg.LockTime, a.ourKey, a.theirKey,
	)
	if err != nil {
		m.abort(codeInternal, "%v", err)
		m.resume()

		return
	}
	for _, c := range coins {
		witness, _ := CoinWitnessWeight(c.PkScript)
		session.AddLocalInput(interactivetx.Input{
			OutPoint: c.OutPoint, Value: c.Value,
			PkScript: c.PkScript, Sequence: interactivetx.MaxSequence,
			MaxWitnessWeight: witness, PrevTx: c.PrevTx,
		})
	}
	for _, o := range outputs {
		session.AddLocalOutput(o)
	}
	// Our part again, in the peer's replacement: the reserve on the
	// capacity it chose binds the peer's side, not ours.
	if weStarted(a) {
		session.WaiveOwnReserve()
	}
	b.ourInputs = coins
	b.ourOutputs = outputs
	b.oursReplaced = weStarted(a)

	// The user's request follows the splice to its latest transaction.
	handOver(a, b)

	// The splice keeps its place in the limits: the replacement is the
	// same splice.
	b.session = session
	b.replaced = append(append([]*active(nil), a.replaced...), a)
	m.mu.Lock()
	m.active = b
	m.mu.Unlock()
	m.startTimeout()

	log.Infof("ChannelID(%v): peer replaces splice %v (%v) at %v",
		m.cfg.ChanID, a.txid, a.feeRate, b.feeRate)
	m.send(&lnwire.TxAckRbf{
		ChannelID:                 m.cfg.ChanID,
		FundingOutputContribution: fn.Some(ours),
		RequireConfirmedInputs:    true,
	})
}

// weStarted reports whether this node started the splice a transaction
// belongs to: it built it, or it is the peer's replacement of one it did.
func weStarted(a *active) bool {
	return a.role == interactivetx.Initiator || a.oursReplaced
}

// handOver passes the user's request, and the stream of its updates, from
// one transaction of the splice to the one that becomes the splice.
func handOver(from, to *active) {
	if from == to || from.request == nil {
		return
	}
	to.request, to.updates = from.request, from.updates
	from.request, from.updates = nil, nil
}

// ourRateLimit is the highest fee rate this node pays its part of a
// replacement of its own splice at: four times the rate it chose, or three
// times the node's own estimate for the next blocks, whichever is higher.
// Above that a replacement would mostly spend our money on fees.
func (m *Machine) ourRateLimit(a *active) chainfee.SatPerKWeight {
	chosen := a.feeRate
	for _, c := range a.replaced {
		chosen = min(chosen, c.feeRate)
	}
	limit := 4 * chosen
	if m.cfg.Estimator != nil {
		if fast, err := m.cfg.Estimator.EstimateFeePerKW(2); err == nil {
			limit = max(limit, 3*fast)
		}
	}

	return limit
}

// ourPart returns what this node puts into a replacement of a splice it
// started: the same coins with the change left after their fee at the new
// rate (dropped if dust), or the same amount to the same destination with
// that output's fee from our side of the channel.
func (m *Machine) ourPart(a *active, feeRate chainfee.SatPerKWeight) (
	btcutil.Amount, []Coin, []interactivetx.Output, error) {

	outs := a.ourOutputs
	if len(outs) == 0 && a.role == interactivetx.Initiator {
		// A record from before our outputs were kept: in M1 the peer
		// adds nothing to our splices, so every output but the
		// funding one is ours.
		for i, o := range a.res.Tx.TxOut {
			if uint32(i) != a.fundingIndex {
				outs = append(outs, interactivetx.Output{
					Value:    btcutil.Amount(o.Value),
					PkScript: o.PkScript,
				})
			}
		}
	}

	// Moving out: one output, the destination, paid from our side.
	if a.ourContribution < 0 {
		if len(outs) != 1 {
			return 0, nil, nil, fmt.Errorf("%d outputs moving out",
				len(outs))
		}
		out := interactivetx.Output{
			Value: outs[0].Value, PkScript: outs[0].PkScript,
		}
		fee := feeRate.FeeForWeight(lntypes.WeightUnit(
			interactivetx.OutputWeight(out.PkScript),
		))
		ours := -(out.Value + fee)

		// What we can send is the least on any funding, the splice
		// being replaced among them, where this move-out is already
		// taken: it moves the same funds, so that much is added back.
		// The session's balance and reserve checks still apply.
		available := m.cfg.Channel.AvailableBalance().ToSatoshis()
		movedOut := -a.ourContribution
		for _, c := range a.replaced {
			if -c.ourContribution > movedOut {
				movedOut = -c.ourContribution
			}
		}
		available += movedOut
		if -ours > available {
			return 0, nil, nil, fmt.Errorf("moving %v out of %v "+
				"available", -ours, available)
		}

		return ours, nil, []interactivetx.Output{out}, nil
	}

	// Adding funds: our coins, from the wallet's copy of each one's
	// transaction.
	var (
		coins  []Coin
		total  btcutil.Amount
		weight int64
	)
	for _, in := range a.res.Inputs {
		if in.Shared || !ourSerial(a.role, in.SerialID) {
			continue
		}
		prev, err := m.cfg.CoinSource.FetchTx(in.OutPoint.Hash)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("coin %v: %w",
				in.OutPoint, err)
		}
		witness, ok := CoinWitnessWeight(in.PkScript)
		if !ok {
			return 0, nil, nil, fmt.Errorf("coin %v can't be "+
				"spent here", in.OutPoint)
		}
		coins = append(coins, Coin{
			OutPoint: in.OutPoint, Value: in.Value,
			PkScript: in.PkScript, PrevTx: prev,
		})
		total += in.Value
		weight += interactivetx.InputWeight(witness) + peerInputMargin
	}

	// Change to the address it went to, or a new one if there was none:
	// what isn't change must be no more than the session lets a side
	// overpay.
	amount := a.ourContribution
	var change []byte
	switch {
	case len(outs) == 1:
		change = outs[0].PkScript
	case m.cfg.ChangeScript != nil:
		if script, err := m.cfg.ChangeScript(); err == nil {
			change = script
		}
	}
	if change != nil {
		withChange := weight + interactivetx.OutputWeight(change)
		left := total - amount -
			feeRate.FeeForWeight(lntypes.WeightUnit(withChange))
		if left >= m.cfg.DustLimit(change) {
			if err := m.keepsReserve(coins, left); err != nil {
				return 0, nil, nil, err
			}

			return amount, coins, []interactivetx.Output{{
				Value: left, PkScript: change,
			}}, nil
		}
	}
	if total-amount < feeRate.FeeForWeight(lntypes.WeightUnit(weight)) {
		return 0, nil, nil, fmt.Errorf("our coins don't pay for "+
			"themselves at %v", feeRate)
	}
	if err := m.keepsReserve(coins, 0); err != nil {
		return 0, nil, nil, err
	}

	return amount, coins, nil, nil
}

// keepsReserve checks that the wallet keeps its reserve for fee-bumping its
// anchor channels' closes once our part of a replacement is paid for: the
// coins outside the splice and the change left at the peer's higher rate.
// Our splice kept it; the replacement's fee mustn't eat it.
func (m *Machine) keepsReserve(ours []Coin, change btcutil.Amount) error {
	if m.cfg.Reserve == nil {
		return nil
	}
	reserve, err := m.cfg.Reserve()
	if err != nil || reserve <= 0 {
		return err
	}
	coins, err := ListCoins(m.cfg.CoinSource)
	if err != nil {
		return err
	}
	spent := make(map[wire.OutPoint]bool, len(ours))
	for _, c := range ours {
		spent[c.OutPoint] = true
	}
	kept := change
	for _, c := range coins {
		if !spent[c.OutPoint] {
			kept += c.Value
		}
	}
	if kept < reserve {
		return fmt.Errorf("%v would be left of the %v kept for fee "+
			"bumps", kept, reserve)
	}

	return nil
}

// checkTxInitRbf returns why the peer's replacement is refused, or "".
func (m *Machine) checkTxInitRbf(msg *lnwire.TxInitRbf) (string, string) {
	a := m.active
	switch {
	case a == nil || a.phase != phasePending:
		return codeRbfUnsupported, "no signed splice to replace"

	case a.lockedSent || a.lockedReceived:
		return codeRbfUnsupported, "already locked"

	case len(a.replaced) >= maxReplacements:
		return codeTooManySplices, fmt.Sprintf("%d replacements",
			len(a.replaced))

	case m.cfg.Disabled:
		return codeDisabled, ""

	case shutdownSent(m.cfg.Channel.State()):
		return codeClosing, ""

	case m.misbehaved || m.cfg.Limits.isMisbehaved(m.cfg.ChanID):
		return codePeerMisbehaved, ""

	// Failed replacements count as aborts: each kept the channel
	// quiescent.
	case m.cfg.Limits.abortsReached(m.cfg.ChanID):
		return codeTooManyAborts, ""

	// The rate may stay (the Core Lightning port replaces at the fee
	// floor), not fall.
	case chainfee.SatPerKWeight(msg.FeeRatePerKw) < a.feeRate:
		return codeFeeBelowFloor, fmt.Sprintf("%d sat/kw replacing %v",
			msg.FeeRatePerKw, a.feeRate)

	case m.cfg.Chain != nil && !m.cfg.Chain.Synced():
		return codeChainNotSynced, ""
	}

	// The checks of a splice_init, with the splice's keys.
	return m.checkSpliceInit(&lnwire.SpliceInit{
		FundingContribution: msg.FundingOutputContribution.UnwrapOr(0),
		FundingFeeRatePerKw: msg.FeeRatePerKw,
		LockTime:            msg.LockTime,
		FundingPubKey:       a.theirKey,
	})
}

// forgetReplacement drops a replacement that didn't get past negotiation or
// our signatures: the transaction it was to replace is the splice again.
func (m *Machine) forgetReplacement() {
	a := m.active
	if a.timeout != nil {
		a.timeout.Stop()
	}
	m.cfg.Limits.noteAbort(m.cfg.ChanID)
	if a.res != nil {
		m.removeFunding(a.txid)
	}

	prev := a.replaced[len(a.replaced)-1]
	prev.replaced = a.replaced[:len(a.replaced)-1]
	handOver(a, prev)
	m.mu.Lock()
	m.active = prev
	m.mu.Unlock()
	log.Infof("ChannelID(%v): replacement of splice %v dropped: %v",
		m.cfg.ChanID, prev.txid, a.abortReason)

	m.ensurePending()
	m.resume()
}

// removeFunding drops a transaction of the splice from the channel's
// funding set, and its record.
func (m *Machine) removeFunding(txid chainhash.Hash) {
	err := m.cfg.Channel.RemovePendingFunding(txid)
	switch {
	case errors.Is(err, lnwallet.ErrUnknownFunding):
		m.forgetRecord(txid)

	case err != nil:
		log.Errorf("ChannelID(%v): unable to drop splice %v: %v",
			m.cfg.ChanID, txid, err)
	}
}

// ensurePending republishes the splice's transaction and starts watching
// it, if that isn't running: for one that became the splice again, or that
// a restart left as a candidate only.
func (m *Machine) ensurePending() {
	a := m.active
	if a == nil || a.phase != phasePending || a.pendingWatched {
		return
	}
	if tx, err := m.signedTx(); err == nil {
		m.publish(tx)
	}
	height := a.broadcastHeight
	if height == 0 {
		height, _ = m.cfg.Chain.BestHeight()
	}
	m.watchConfirmationOf(a, height)
	m.watchPendingOf(a, height)
}

// dropCandidate drops an earlier transaction of the splice that can no
// longer confirm, one of its inputs spent elsewhere: no longer followed,
// recorded dead, its funding left in the channel's set (see spliceDead).
func (m *Machine) dropCandidate(c *active) {
	a := m.active
	kept := a.replaced[:0]
	for _, r := range a.replaced {
		if r != c {
			kept = append(kept, r)
		}
	}
	m.mu.Lock()
	a.replaced = kept
	m.mu.Unlock()
	m.markDead(c.txid)
	log.Infof("ChannelID(%v): splice candidate %v can no longer confirm",
		m.cfg.ChanID, c.txid)

	// An earlier transaction of a disowned splice: the last gone,
	// nothing will lock in time, so the channel is closed now.
	if a.disowned {
		m.closeDisowned()
	}
}

// liveCandidate returns the newest earlier transaction of the splice that
// doesn't spend op.
func (m *Machine) liveCandidate(op wire.OutPoint) *active {
	a := m.active
	for i := len(a.replaced) - 1; i >= 0; i-- {
		if !spends(a.replaced[i], op) {
			return a.replaced[i]
		}
	}

	return nil
}

// spends reports whether a transaction of the splice spends op.
func spends(a *active, op wire.OutPoint) bool {
	for _, in := range a.res.Inputs {
		if in.OutPoint == op {
			return true
		}
	}

	return false
}

// fallBackTo makes an earlier transaction of the splice the splice again:
// the latest is dead, op having been spent elsewhere. Candidates spending
// op are dead too; each stays in the channel's funding set (see spliceDead).
func (m *Machine) fallBackTo(prev *active, op wire.OutPoint) {
	a := m.active
	m.markDead(a.txid)

	var kept []*active
	for _, c := range a.replaced {
		switch {
		case c == prev:
		case spends(c, op):
			m.markDead(c.txid)
		default:
			kept = append(kept, c)
		}
	}
	prev.replaced = kept
	handOver(a, prev)
	m.mu.Lock()
	m.active = prev
	m.mu.Unlock()
	log.Warnf("ChannelID(%v): splice replacement %v can no longer "+
		"confirm (%v spent elsewhere); %v is the splice again",
		m.cfg.ChanID, a.txid, op, prev.txid)

	m.ensurePending()
}

// candidate returns the earlier transaction of the splice with txid, if
// any.
func (m *Machine) candidate(txid chainhash.Hash) *active {
	a := m.active
	if a == nil {
		return nil
	}
	for _, c := range a.replaced {
		if c.txid == txid {
			return c
		}
	}

	return nil
}

// adopt makes the splice's earlier transaction with txid the splice: it
// confirmed, so the others never can.
func (m *Machine) adopt(txid chainhash.Hash) *active {
	c := m.candidate(txid)
	if c == nil {
		return nil
	}

	a := m.active
	if a.timeout != nil {
		a.timeout.Stop()
	}
	c.replaced = nil
	handOver(a, c)
	m.mu.Lock()
	m.active = c
	m.mu.Unlock()
	log.Infof("ChannelID(%v): splice %v confirmed, not its replacement %v",
		m.cfg.ChanID, txid, a.txid)

	// A replacement still being built kept the channel quiescent.
	if a.phase != phasePending {
		m.resume()
	}
	m.ensurePending()

	return c
}

// candidateLocked records the peer's splice_locked for an earlier
// transaction of the splice. Which one we lock is decided by which reaches
// our own depth: the peer's word alone doesn't change it.
func (m *Machine) candidateLocked(c *active) {
	if c.lockedReceived {
		return
	}
	err := m.cfg.Channel.State().UpdateSpliceRecord(c.txid,
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.LockedReceived = true
			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record the peer's "+
			"splice_locked: %v", m.cfg.ChanID, err)
		return
	}
	m.mu.Lock()
	c.lockedReceived = true
	m.mu.Unlock()
}

// checkReplacementFee refuses a replacement paying less in fees than the
// transaction it replaces.
func (m *Machine) checkReplacementFee(res *interactivetx.Result) error {
	a := m.active
	if len(a.replaced) == 0 {
		return nil
	}
	prev := a.replaced[len(a.replaced)-1]
	if prev.res == nil || res.Fee >= prev.res.Fee {
		return nil
	}

	return fmt.Errorf("replacement pays %v in fees, less than the %v "+
		"of %v", res.Fee, prev.res.Fee, prev.txid)
}
