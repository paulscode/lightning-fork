package splice

import (
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// A splice this node starts: funds from the wallet into the channel, or out
// of it to an address. In M1 the peer puts nothing in. Everything slow (coin
// selection, leases, keys) happens before stfu, since quiescence holds the
// channel and a peer with HTLCs pending disconnects after 60 seconds.

// Request is a splice this node starts.
type Request struct {
	// Direction says which way the funds move.
	Direction Direction

	// Amount is what goes in or out; ignored with UseMax.
	Amount btcutil.Amount

	// UseMax adds everything the wallet can spend after the fee, or
	// moves out everything the channel lets go after the fee.
	UseMax bool

	// FeeRate is the transaction's fee rate.
	FeeRate chainfee.SatPerKWeight

	// ForceLowFee accepts a fee rate below the initiator floor, never
	// below the minimum relay fee.
	ForceLowFee bool

	// Destination is where funds moved out go.
	Destination []byte

	// Outpoints, for funds in, are the only coins the splice may spend
	// (coin control); empty, the wallet chooses among all of them.
	Outpoints []wire.OutPoint
}

// State is a splice's progress as reported to whoever started it.
type State string

const (
	StatePreparing   State = "preparing"
	StateNegotiating State = "negotiating"
	StateSigned      State = "signed"
	StateBroadcast   State = "broadcast"
	StateLocked      State = "locked"
	StateAborted     State = "aborted"
)

// Update is a step of a splice this node started.
type Update struct {
	State State

	// Txid names the splice from StateSigned on, and FundingIndex is
	// its funding output's index.
	Txid         chainhash.Hash
	FundingIndex uint32

	// Reason, for StateAborted, is a refusal code or the peer's text.
	Reason string

	// ReasonFromPeer is true when Reason is the peer's text.
	ReasonFromPeer bool
}

// ErrRefused is wrapped in the error a refused request returns, with the
// refusal code first.
var ErrRefused = errors.New("splice refused")

// Splice starts a splice of the channel. The returned channel reports its
// steps and is closed after the last one (locked or aborted).
func (m *Machine) Splice(req Request) (<-chan Update, error) {
	result := make(chan error, 1)
	updates := make(chan Update, 16)
	m.enqueue(func() {
		result <- m.startSplice(req, updates)
	})

	select {
	case err := <-result:
		if err != nil {
			return nil, err
		}

		return updates, nil

	case <-m.quit:
		return nil, errors.New("splice machine stopping")
	}
}

// refused returns a refusal with its code.
func refused(code string, format string, args ...interface{}) error {
	if format == "" {
		return fmt.Errorf("%w: %s", ErrRefused, code)
	}

	return fmt.Errorf("%w: %s: %s", ErrRefused, code,
		fmt.Sprintf(format, args...))
}

// report sends an update to whoever started the splice, if anyone, closing
// the channel after the last one.
func (m *Machine) report(u Update) {
	a := m.active
	if a == nil || a.updates == nil {
		return
	}
	if u.Txid != (chainhash.Hash{}) {
		u.FundingIndex = a.fundingIndex
	}
	select {
	case a.updates <- u:
	default:
		log.Warnf("ChannelID(%v): splice update dropped: %v",
			m.cfg.ChanID, u.State)
	}
	if u.State == StateLocked || u.State == StateAborted {
		close(a.updates)
		a.updates = nil
	}
}

// startSplice prepares the splice and asks the link to quiesce. Nothing is
// sent to the peer until it is quiescent.
func (m *Machine) startSplice(req Request, updates chan Update) error {
	if code := m.spliceable(); code != "" {
		return refused(code, "")
	}
	if req.FeeRate <= 0 {
		return refused(codeFeeBelowFloor, "no fee rate")
	}
	err := CheckInitiatorFeeRate(req.FeeRate, m.cfg.Estimator,
		req.ForceLowFee)
	if err != nil {
		return refused(codeFeeBelowFloor, "%v", err)
	}

	state := m.cfg.Channel.State()
	if req.Direction == DirectionOut {
		close := state.LocalShutdownScript
		if len(close) > 0 && string(close) != string(req.Destination) {
			return refused("close_address_set", "")
		}
	}

	ourKey, err := m.cfg.KeyRing.DeriveNextKey(keychain.KeyFamilyMultiSig)
	if err != nil {
		return refused(codeInternal, "%v", err)
	}

	a := &active{
		phase:   phasePreparing,
		role:    interactivetx.Initiator,
		ourKey:  ourKey,
		feeRate: req.FeeRate,
		request: &req,
		updates: updates,
	}
	if err := m.prepareFunds(a, req); err != nil {
		return err
	}
	m.setActive(a)
	m.report(Update{State: StatePreparing})

	// Quiesce; splice_init goes out once the link says we are. A peer
	// that never answers our stfu would leave the link unable to send
	// updates, and our coins leased, for as long as the connection
	// lasts: past the wait, the connection is dropped, which ends the
	// request.
	stfu := m.cfg.Link.InitStfu()
	wait := 2 * m.cfg.Policy.NegotiationTimeout
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		select {
		case res := <-stfu:
			m.enqueue(func() { m.quiesced(a, res) })
		case <-time.After(wait):
			m.enqueue(func() { m.stfuUnanswered(a, wait) })
		case <-m.quit:
		}
	}()

	return nil
}

// prepareFunds works out our contribution: coins selected and leased for
// funds in, the amount and fee from our balance for funds out.
func (m *Machine) prepareFunds(a *active, req Request) error {
	_, capacity, _, _, err := m.currentFunding()
	if err != nil {
		return refused(codeInternal, "%v", err)
	}

	// The new funding output's script isn't known until the peer's key
	// arrives; any 2-of-2 P2WSH weighs the same.
	_, probe, err := input.GenFundingPkScript(
		a.ourKey.PubKey.SerializeCompressed(),
		a.ourKey.PubKey.SerializeCompressed(), int64(capacity),
	)
	if err != nil {
		return refused(codeInternal, "%v", err)
	}
	shape := Shape{
		SharedWitnessWeight: input.MultiSigWitnessSize,
		FundingScript:       probe.PkScript,
	}

	switch req.Direction {
	case DirectionIn:
		changeScript, err := m.cfg.ChangeScript()
		if err != nil {
			return refused(codeInternal, "%v", err)
		}

		var reserve btcutil.Amount
		if m.cfg.Reserve != nil {
			reserve, err = m.cfg.Reserve()
			if err != nil {
				return refused(codeInternal, "%v", err)
			}
		}

		var sel *AddFunds
		selectAndLease := func() error {
			coins, err := ListCoins(m.cfg.CoinSource)
			if err != nil {
				return err
			}
			coins, keep, err := OnlyCoins(
				coins, req.Outpoints, reserve,
			)
			if err != nil {
				return err
			}
			sel, err = SelectAddFunds(
				shape, coins, req.Amount, req.UseMax,
				req.FeeRate, changeScript, m.cfg.DustLimit,
				keep,
			)
			if err != nil {
				return err
			}

			return LeaseCoins(m.cfg.Wallet, sel.Inputs)
		}
		if m.cfg.WithCoinSelectLock != nil {
			err = m.cfg.WithCoinSelectLock(selectAndLease)
		} else {
			err = selectAndLease()
		}
		switch {
		case errors.Is(err, ErrInsufficientFunds):
			return refused(codeInsufficientFunds, "%v", err)
		case errors.Is(err, ErrBelowDust):
			return refused("below_min", "%v", err)
		case errors.Is(err, ErrUnknownCoin):
			return refused(codeUnknownCoin, "%v", err)
		case err != nil:
			return refused(codeInternal, "%v", err)
		}

		// With change, an output of ours, the construction wants
		// our balance at the reserve on the new capacity.
		balance, _ := m.ourBalance()
		if sel.Change > 0 && !keepsSpliceReserve(
			balance, capacity, sel.Contribution,
		) {
			err := ReleaseCoins(m.cfg.Wallet, sel.Inputs)
			if err != nil {
				log.Errorf("ChannelID(%v): unable to release "+
					"coins: %v", m.cfg.ChanID, err)
			}

			return refused("below_min", "%v in leaves our balance "+
				"below the reserve of %v", sel.Contribution,
				spliceReserve(capacity+sel.Contribution))
		}

		a.ourInputs = sel.Inputs
		a.ourContribution = sel.Contribution
		if sel.Change > 0 {
			a.ourOutputs = append(a.ourOutputs, interactivetx.Output{
				Value: sel.Change, PkScript: changeScript,
			})
		}

	case DirectionOut:
		amount := req.Amount
		if req.UseMax {
			amount = m.maxOut(shape, req.Destination, req.FeeRate)
		}
		out, err := NewMoveOut(
			shape, amount, req.Destination, req.FeeRate,
			m.cfg.DustLimit,
		)
		if err != nil {
			return refused("below_min", "%v", err)
		}
		if err := m.checkMoveOut(out.Contribution); err != nil {
			return err
		}
		a.ourContribution = out.Contribution
		a.ourOutputs = append(a.ourOutputs, interactivetx.Output{
			Value: out.Amount, PkScript: req.Destination,
		})

	default:
		return refused(codeInternal, "no direction")
	}

	return nil
}

// maxOut is the most a splice of ours can move out to destScript at
// feeRate, the fee on top: what we can send, and what keeps the reserve on
// the new capacity, as the estimate's most out.
func (m *Machine) maxOut(shape Shape, destScript []byte,
	feeRate chainfee.SatPerKWeight) btcutil.Amount {

	fee, _ := MoveOutFee(shape, destScript, feeRate)
	most := min(
		m.cfg.Channel.AvailableBalance().ToSatoshis(),
		maxOutKeepingReserve(m.ourBalance()),
	)

	return max(0, most-fee)
}

// ourBalance returns our balance on the channel's current commitment and
// its capacity, read under the channel's lock: before quiescence the link
// may be updating them.
func (m *Machine) ourBalance() (btcutil.Amount, btcutil.Amount) {
	snap := m.cfg.Channel.StateSnapshot()

	return snap.LocalBalance.ToSatoshis(), snap.Capacity
}

// checkMoveOut checks a negative contribution against what we can send:
// the balance above the reserve and the commitment fee, without the pending
// HTLCs. Checked again once the channel is quiescent.
func (m *Machine) checkMoveOut(contribution btcutil.Amount) error {
	available := m.cfg.Channel.AvailableBalance().ToSatoshis()
	if -contribution > available {
		return refused("above_max_out", "%v out, %v available",
			-contribution, available)
	}

	// And the reserve on the new capacity the construction keeps.
	keep := maxOutKeepingReserve(m.ourBalance())
	if -contribution > keep {
		return refused("above_max_out", "%v out, %v keeping the "+
			"reserve on the new capacity", -contribution, keep)
	}

	return nil
}

// quiesced sends splice_init once the channel is quiescent, if we started
// the quiescence. Lost to the peer's, or failed, the splice ends.
func (m *Machine) quiesced(a *active,
	res fn.Result[lntypes.ChannelParty]) {

	initiator, err := res.Unpack()
	if m.active != a || a.phase != phasePreparing {
		// Canceled while quiescence was being reached, and won: the
		// channel is ours to free, and the peer, quiescent too, waits
		// for a splice_init that won't come.
		if m.active != a && err == nil && initiator == lntypes.Local {
			reason := "canceled"
			if m.closing() {
				reason = codeClosing
			}
			m.endQuiescence(reason)
		}

		return
	}

	// A close begun while we waited, by either side: BOLT 2 allows no
	// splice from then on. Quiescence the link withdrew for it is no
	// failure to quiesce.
	if m.closing() {
		m.failOurs(codeClosing, "")
		if err == nil && initiator == lntypes.Local {
			m.endQuiescence(codeClosing)
		}

		return
	}

	switch {
	case err != nil:
		m.failOurs(codeNotQuiescent, "%v", err)
		return

	case initiator != lntypes.Local:
		// The peer quiesced first, or won the tie as the channel's
		// opener: its splice goes first, and ends quiescence. Not
		// resumed here, which would end it under the peer's
		// splice_init.
		m.failOurs(codeNotQuiescent, "the peer started quiescence")

		return
	}

	// The balance is frozen now: check again.
	if a.ourContribution < 0 {
		if err := m.checkMoveOut(a.ourContribution); err != nil {
			m.failOurs("above_max_out", "%v", err)
			m.endQuiescence("above_max_out")

			return
		}
	}

	height, err := m.cfg.Chain.BestHeight()
	if err != nil {
		m.failOurs(codeChainNotSynced, "%v", err)
		m.endQuiescence(codeChainNotSynced)

		return
	}

	m.update(func(a *active) {
		a.phase = phaseNegotiating
		a.lockTime = height
	})
	m.startTimeout()
	m.report(Update{State: StateNegotiating})

	m.notify("started", chainhash.Hash{}, "", false)
	m.send(&lnwire.SpliceInit{
		ChannelID:              m.cfg.ChanID,
		FundingContribution:    a.ourContribution,
		FundingFeeRatePerKw:    uint32(a.feeRate),
		LockTime:               height,
		FundingPubKey:          a.ourKey.PubKey,
		RequireConfirmedInputs: true,
	})
}

// stfuUnanswered ends a splice of ours whose stfu the peer didn't answer in
// time, and the connection with it, the link's request for quiescence
// still out, also when the splice was canceled meanwhile.
func (m *Machine) stfuUnanswered(a *active, wait time.Duration) {
	if m.active == a && a.phase == phasePreparing {
		m.failOurs(codeTimeout, "the peer didn't answer stfu")
	}
	m.warnAndDisconnect("stfu not answered within %v", wait)
}

// endQuiescence ends a quiescence we won without a splice_init to follow:
// a tx_abort tells the peer, which waits in quiescence for one (the Core
// Lightning port otherwise refuses our next update), and the link resumes.
// The peer's echo of it is ours.
func (m *Machine) endQuiescence(reason string) {
	m.abortSent = true
	m.send(&lnwire.TxAbort{ChannelID: m.cfg.ChanID, Data: []byte(reason)})
	m.resume()
}

// failOurs ends a splice we started that hasn't left preparation or
// negotiation.
func (m *Machine) failOurs(code string, format string, args ...interface{}) {
	reason := code
	if format != "" {
		reason = code + ": " + fmt.Sprintf(format, args...)
	}
	log.Infof("ChannelID(%v): splice failed: %v", m.cfg.ChanID, reason)

	// The coins go back before anyone hears of it.
	a := m.active
	if a != nil && len(a.ourInputs) > 0 {
		if err := ReleaseCoins(m.cfg.Wallet, a.ourInputs); err != nil {
			log.Errorf("ChannelID(%v): unable to release coins: %v",
				m.cfg.ChanID, err)
		}
		a.ourInputs = nil
	}

	m.recordLast("aborted", reason, false)
	m.notify("aborted", chainhash.Hash{}, reason, false)
	m.report(Update{State: StateAborted, Reason: reason})
	m.setActive(nil)
}

// handleSpliceAck takes the peer's splice_ack and starts the construction.
func (m *Machine) handleSpliceAck(msg *lnwire.SpliceAck) {
	a := m.active
	if a == nil || a.role != interactivetx.Initiator ||
		a.phase != phaseNegotiating || a.session != nil {

		if m.abortSent {
			log.Debugf("ChannelID(%v): splice_ack crossed our "+
				"tx_abort", m.cfg.ChanID)
			return
		}
		m.warnAndDisconnect("splice_ack without our splice_init")
		return
	}

	// In M1 the peer puts nothing in.
	if msg.FundingContribution != 0 {
		m.abort(codePeerContribUnsupp, "")
		m.forget()

		return
	}
	if msg.FundingPubKey == nil {
		m.abort(codeInternal, "no funding key")
		m.forget()

		return
	}

	session, b, err := m.newConstruction(
		interactivetx.Initiator, a.ourContribution, 0, a.feeRate,
		a.lockTime, a.ourKey, msg.FundingPubKey,
	)
	if err != nil {
		m.abort(codeInternal, "%v", err)
		m.forget()

		return
	}

	// The peer may add nothing to our splice.
	session.RefusePeerItems(codePeerContribUnsupp)

	if err := session.AddSharedInputAndFunding(); err != nil {
		m.abort(codeInternal, "%v", err)
		m.forget()

		return
	}
	for _, c := range a.ourInputs {
		w, _ := CoinWitnessWeight(c.PkScript)
		session.AddLocalInput(interactivetx.Input{
			OutPoint: c.OutPoint, Value: c.Value,
			PkScript: c.PkScript, PrevTx: c.PrevTx,
			Sequence:         interactivetx.MaxSequence,
			MaxWitnessWeight: w,
		})
	}
	for _, o := range a.ourOutputs {
		session.AddLocalOutput(o)
	}

	m.update(func(a *active) {
		a.session = session
		a.fundingScript = b.fundingScript
		a.fundingWitness = b.fundingWitness
		a.theirKey = b.theirKey
		a.theirContribution = 0
		a.capacity = b.capacity
	})

	m.advanceConstruction()
}

// Cancel ends a splice this node started if it hasn't reached the end of
// the construction: before splice_init nothing was sent; during the
// construction the peer gets tx_abort. Past that the splice carries on, and
// Cancel reports false.
func (m *Machine) Cancel() bool {
	result := make(chan bool, 1)
	m.enqueue(func() {
		a := m.active
		switch {
		// A replacement the peer is building isn't the user's to
		// cancel; the splice it replaces goes on either way.
		case a == nil || a.request == nil || len(a.replaced) > 0:
			result <- false

		case a.phase == phasePreparing:
			m.failOurs("canceled", "")
			result <- true

		case a.phase == phaseNegotiating:
			m.abort("canceled", "")
			m.forget()
			result <- true

		default:
			result <- false
		}
	})

	select {
	case ok := <-result:
		return ok
	case <-m.quit:
		return false
	}
}
