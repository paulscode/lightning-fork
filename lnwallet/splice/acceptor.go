package splice

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// A splice the peer starts. In M1 this node adds nothing to it: no inputs,
// no outputs, a contribution of zero. Everything it refuses is decided before
// its commitment_signed, since with nothing put in it signs first.

// handleSpliceInit answers the peer's splice_init: refused with tx_abort, or
// accepted with splice_ack and the construction started.
func (m *Machine) handleSpliceInit(msg *lnwire.SpliceInit) {
	// Ours, still waiting for the quiescence the peer won: the peer's
	// splice goes first, as when we learn it from the link.
	if a := m.active; a != nil && a.role == interactivetx.Initiator &&
		a.phase == phasePreparing {

		initiator, err := m.cfg.Link.QuiescenceInitiator().Unpack()
		if err == nil && initiator == lntypes.Remote {
			m.failOurs(codeNotQuiescent, "the peer started "+
				"quiescence")
		}
	}

	// A splice_init while another splice is negotiating or unlocked, or
	// from the side that didn't start quiescence, breaks the protocol.
	if m.active != nil {
		m.warnAndDisconnect("splice_init while a splice is in progress")
		return
	}
	initiator, err := m.cfg.Link.QuiescenceInitiator().Unpack()
	if err != nil || initiator != lntypes.Remote {
		m.warnAndDisconnect("splice_init without the peer having " +
			"started quiescence")
		return
	}
	// BOLT 2: a node that received shutdown sends a warning and closes
	// the connection. Shutdown we sent only, the peer may not have read
	// before its splice_init: refused with tx_abort, as spliceable has
	// it.
	if m.shutdownReceived() {
		m.warnAndDisconnect("splice_init after shutdown")
		return
	}

	if code := m.spliceable(); code != "" {
		m.abort(code, "")
		m.resume()

		return
	}
	if code, why := m.checkSpliceInit(msg); code != "" {
		m.abort(code, "%s", why)
		m.resume()

		return
	}

	// After every check, so that nothing refused takes a place, and
	// before deriving a key, which the wallet stores: a peer refused at
	// the limits, again and again, mustn't cost a key each time.
	code := m.cfg.Limits.admit(m, m.cfg.ChanID, m.cfg.TheirNodeID, false)
	if code != "" {
		m.abort(code, "")
		m.resume()

		return
	}
	failed := func(format string, args ...interface{}) {
		m.cfg.Limits.release(m, m.cfg.ChanID, false)
		m.abort(codeInternal, format, args...)
		m.resume()
	}

	ourKey, err := m.cfg.KeyRing.DeriveNextKey(keychain.KeyFamilyMultiSig)
	if err != nil {
		failed("")
		return
	}

	session, a, err := m.newConstruction(
		interactivetx.Acceptor, msg.FundingContribution, 0,
		chainfee.SatPerKWeight(msg.FundingFeeRatePerKw), msg.LockTime,
		ourKey, msg.FundingPubKey,
	)
	if err != nil {
		failed("%v", err)
		return
	}
	a.session = session
	m.setActive(a)
	m.startTimeout()

	m.notify("started", [32]byte{}, "", false)
	m.send(&lnwire.SpliceAck{
		ChannelID:              m.cfg.ChanID,
		FundingContribution:    0,
		FundingPubKey:          ourKey.PubKey,
		RequireConfirmedInputs: true,
	})
}

// checkSpliceInit applies this node's policy to a peer's splice_init: its fee
// rate, the new capacity, and what it takes out of the peer's balance.
func (m *Machine) checkSpliceInit(msg *lnwire.SpliceInit) (string, string) {
	if msg.FundingPubKey == nil {
		return codeInternal, "no funding key"
	}

	// Before any arithmetic on it: more than can exist is no amount.
	if c := msg.FundingContribution; c < -maxMoney || c > maxMoney {
		return codePeerMisbehaved, fmt.Sprintf("contribution %d out "+
			"of range", int64(c))
	}

	feeRate := chainfee.SatPerKWeight(msg.FundingFeeRatePerKw)
	if err := CheckAcceptorFeeRate(feeRate, m.cfg.Estimator); err != nil {
		// The reason without the code again, which the error starts
		// with.
		return codeFeeBelowFloor, strings.TrimPrefix(
			err.Error(), ErrFeeBelowFloor.Error()+": ",
		)
	}

	height, err := m.cfg.Chain.BestHeight()
	if err != nil {
		return codeChainNotSynced, err.Error()
	}
	// A locktime as a time would need the chain's median time past to
	// know the transaction final; peers use heights.
	if msg.LockTime >= txscript.LockTimeThreshold {
		return codeLocktime, fmt.Sprintf("locktime %d is a time",
			msg.LockTime)
	}
	if msg.LockTime > height+1 {
		return codeLocktime, fmt.Sprintf("locktime %d past tip %d",
			msg.LockTime, height)
	}

	state := m.cfg.Channel.State()
	contribution := msg.FundingContribution
	theirBalance := state.LocalCommitment.RemoteBalance.ToSatoshis()
	if contribution < 0 && -contribution > theirBalance {
		return codeAboveBalance, fmt.Sprintf("takes %v out of %v",
			-contribution, theirBalance)
	}

	// What the peer keeps must stay at the reserve it is held to, which
	// every commitment on the new funding is checked against: the
	// construction's own rule (1% of the new capacity) can be less, and
	// the channel would then refuse the peer's commitments there.
	// Refused here, before anything is signed.
	// And the construction's own, 1% of the new capacity, checked now:
	// at tx_complete the Core Lightning port has signed its part.
	_, theirReserve := state.Reserves()
	reserve := max(
		theirReserve, spliceReserve(state.Capacity+contribution),
	)
	if contribution < 0 && theirBalance+contribution < reserve {
		return codeAboveBalance, fmt.Sprintf("leaves %v, below the "+
			"reserve of %v", theirBalance+contribution, reserve)
	}

	capacity := state.Capacity + contribution
	if contribution > 0 && m.cfg.Policy.MaxChannelSize > 0 &&
		capacity > m.cfg.Policy.MaxChannelSize {

		return codeAboveMaxSize, fmt.Sprintf("capacity %v above %v",
			capacity, m.cfg.Policy.MaxChannelSize)
	}

	return "", ""
}

// newConstruction sets up the interactive construction of a splice of the
// channel: the shared input (the current funding output), the new funding
// output with both new keys, both contributions and both balances.
func (m *Machine) newConstruction(role interactivetx.Role,
	initContribution, accContribution btcutil.Amount,
	feeRate chainfee.SatPerKWeight, lockTime uint32,
	ourKey keychain.KeyDescriptor,
	theirKey *btcec.PublicKey) (*interactivetx.Session, *active, error) {

	outpoint, capacity, _, oldScript, err := m.currentFunding()
	if err != nil {
		return nil, nil, err
	}

	newCapacity := capacity + initContribution + accContribution
	if newCapacity <= 0 {
		return nil, nil, errors.New("splice empties the channel")
	}
	witness, out, err := input.GenFundingPkScript(
		ourKey.PubKey.SerializeCompressed(),
		theirKey.SerializeCompressed(), int64(newCapacity),
	)
	if err != nil {
		return nil, nil, err
	}

	state := m.cfg.Channel.State()
	ours := state.LocalCommitment.LocalBalance.ToSatoshis()
	theirs := state.LocalCommitment.RemoteBalance.ToSatoshis()
	initBalance, accBalance := ours, theirs
	ourContribution, theirContribution := initContribution, accContribution
	if role == interactivetx.Acceptor {
		initBalance, accBalance = theirs, ours
		ourContribution, theirContribution = accContribution,
			initContribution
	}

	cfg := interactivetx.Config{
		Role:      role,
		ChannelID: m.cfg.ChanID,
		Shared: interactivetx.SharedInput{
			OutPoint:      outpoint,
			Value:         capacity,
			PkScript:      oldScript,
			WitnessWeight: input.MultiSigWitnessSize,
		},
		FundingScript:         out.PkScript,
		InitiatorContribution: initContribution,
		AcceptorContribution:  accContribution,
		FeeRate:               feeRate,
		LockTime:              lockTime,
		MaxPeerAddMessages:    m.cfg.Policy.MaxPeerAddMessages,
		CheckPeerInput:        m.checkPeerInput,
		CheckPeerOutput:       m.checkPeerOutput,
		DustLimit:             m.cfg.DustLimit,
		WitnessWeight:         peerWitnessWeight,
		Reserve:               spliceReserve,
		InitiatorBalance:      initBalance,
		AcceptorBalance:       accBalance,
	}
	session, err := interactivetx.NewSession(cfg)
	if err != nil {
		return nil, nil, err
	}

	return session, &active{
		phase:             phaseNegotiating,
		role:              role,
		fundingScript:     out.PkScript,
		fundingWitness:    witness,
		ourKey:            ourKey,
		theirKey:          theirKey,
		ourContribution:   ourContribution,
		theirContribution: theirContribution,
		capacity:          newCapacity,
		feeRate:           feeRate,
	}, nil
}

// peerWitnessWeight accepts only the peer inputs whose witness size is known
// before it arrives: P2WPKH and P2TR key path.
func peerWitnessWeight(pkScript []byte) (int64, error) {
	w, ok := CoinWitnessWeight(pkScript)
	if !ok {
		return 0, errors.New("only P2WPKH and P2TR key path inputs")
	}

	return w, nil
}

// checkPeerInput applies this node's policy to an input the peer adds: not
// one of our own outpoints, and confirmed, unspent and not a coinbase output
// as our backend sees it, with the value and script the peer claims.
func (m *Machine) checkPeerInput(in *interactivetx.Input) error {
	if m.cfg.IsOurOutpoint != nil && m.cfg.IsOurOutpoint(in.OutPoint) {
		return fmt.Errorf("input %v is ours", in.OutPoint)
	}

	out, confs, coinbase, err := m.cfg.Chain.ConfirmedOutput(in.OutPoint)
	switch {
	case err != nil:
		return fmt.Errorf("input %v not found unspent: %w",
			in.OutPoint, err)

	case confs < 1:
		return fmt.Errorf("input %v unconfirmed", in.OutPoint)

	case coinbase:
		return fmt.Errorf("input %v is a coinbase output", in.OutPoint)

	case out.Value != int64(in.Value) ||
		!bytes.Equal(out.PkScript, in.PkScript):

		return fmt.Errorf("input %v doesn't match the chain",
			in.OutPoint)
	}

	return nil
}

// checkPeerOutput applies relay standardness to an output the peer adds:
// only witness v0 and v1 programs, which every policy relays.
func (m *Machine) checkPeerOutput(out *interactivetx.Output) error {
	script := out.PkScript
	switch {
	case txscript.IsPayToWitnessPubKeyHash(script),
		txscript.IsPayToWitnessScriptHash(script),
		txscript.IsPayToTaproot(script):

		return nil
	}

	return errors.New("output script not accepted")
}

// startSigningTimeout ends a splice whose signatures don't come in time
// once its transaction is agreed. Until our tx_signatures are out the peer
// can't complete the transaction, so the splice is forgotten: a peer that
// stops answering here would otherwise keep our coins leased, the channel
// unable to splice and, for its own splice, a slot of the node's. The time
// runs from the agreement, so a peer reconnecting doesn't get it again.
func (m *Machine) startSigningTimeout() {
	timeout := 2 * m.cfg.Policy.NegotiationTimeout
	a := m.active
	if a.timeout != nil {
		a.timeout.Stop()
	}
	// Never more than the timeout, a clock set back since the agreement
	// notwithstanding.
	left := timeout
	if !a.agreedAt.IsZero() {
		left = min(timeout, max(0, timeout-time.Since(a.agreedAt)))
	}
	a.timeout = time.AfterFunc(left, func() {
		m.enqueue(func() {
			if m.active != a || a.phase != phaseSigning {
				return
			}

			// Ours are out and the peer's haven't come: the
			// splice is kept, as the peer could publish it,
			// but it gives back its place for the peer's
			// splices, which a peer withholding its signatures
			// would otherwise hold for good.
			if a.ourTxSigs != nil {
				m.releaseWithheld(a)
				return
			}

			// Signing last with our commitment out, the peer
			// may have sent its tx_signatures: it keeps the
			// splice then, and a tx_abort would leave us apart
			// (the Core Lightning port fails the channel). The
			// splice waits; a reconnection settles it.
			if a.commitSent && !m.signsFirst() {
				m.releaseWithheld(a)
				return
			}

			// A transaction the peer built: the Core Lightning
			// port signs its part as soon as tx_complete is
			// exchanged and fails the channel on a tx_abort
			// after. Kept likewise; one the peer doesn't name on
			// reconnecting is dropped then, quietly.
			if a.role == interactivetx.Acceptor {
				m.releaseWithheld(a)
				return
			}

			// Restarted past the time: a tx_abort before the
			// peer's channel_reestablish fails its link, so the
			// splice goes then, as one never signed does.
			m.mu.Lock()
			reestablished := m.reestablished
			m.mu.Unlock()
			if !reestablished {
				a.dropAtReestablish = true
				return
			}
			m.abort(codeTimeout, "signatures took over %v",
				timeout)
			m.forget()
		})
	})
}

// releaseWithheld gives back the node's place a peer's splice holds once
// the peer withholds its tx_signatures, after ours or when it signs first.
func (m *Machine) releaseWithheld(a *active) {
	if a.role != interactivetx.Acceptor || a.oursReplaced ||
		a.theirTxSigs != nil {

		return
	}
	log.Warnf("ChannelID(%v): peer's signatures for splice %v not in "+
		"time; its place for the peer's splices given back",
		m.cfg.ChanID, a.txid)
	m.cfg.Limits.release(m, m.cfg.ChanID, false)
}

// startTimeout ends a negotiation that doesn't finish in time. A new
// negotiation also ends the wait for the echo of an earlier tx_abort.
func (m *Machine) startTimeout() {
	m.abortSent = false
	timeout := m.cfg.Policy.NegotiationTimeout
	a := m.active
	a.timeout = time.AfterFunc(timeout, func() {
		m.enqueue(func() {
			if m.active != a || a.phase != phaseNegotiating {
				return
			}
			m.abort(codeTimeout, "negotiation took over %v",
				timeout)
			m.forget()
		})
	})
}

// handleConstruction takes an interactive construction message.
func (m *Machine) handleConstruction(msg lnwire.Message) {
	a := m.active
	if a == nil || a.phase != phaseNegotiating || a.session == nil {
		// Sent before our tx_abort reached the peer: nothing wrong.
		if m.abortSent {
			log.Debugf("ChannelID(%v): %T crossed our tx_abort",
				m.cfg.ChanID, msg)
			return
		}
		m.warnAndDisconnect("%T outside a negotiation", msg)
		return
	}

	if err := a.session.Receive(msg); err != nil {
		m.constructionFailed(err)
		return
	}
	m.advanceConstruction()
}

// advanceConstruction sends what is ours to send, and finishes the
// construction once both sides are done.
func (m *Machine) advanceConstruction() {
	a := m.active
	for !a.session.Done() && a.session.OurTurn() {
		msg, err := a.session.Next()
		if err != nil {
			m.constructionFailed(err)
			return
		}
		m.send(msg)
	}
	if !a.session.Done() {
		return
	}

	res, err := a.session.Build()
	if err != nil {
		m.constructionFailed(err)
		return
	}
	// A replacement is the peer's to build, and the Core Lightning port
	// has signed its part once tx_complete is exchanged, failing the
	// channel on a tx_abort now. One paying less in total than the
	// transaction it replaces is kept as a candidate: it won't relay,
	// and dies when the other confirms.
	if err := m.checkReplacementFee(res); err != nil {
		log.Warnf("ChannelID(%v): peer's replacement kept, though: %v",
			m.cfg.ChanID, err)
	}

	// The transaction it replaces again: nothing new to sign. Dropped
	// with the connection, not a tx_abort; the peer's next_funding naming
	// it gets our tx_signatures for it again.
	if m.candidate(res.Tx.TxHash()) != nil {
		m.forget()
		m.warnAndDisconnect("replacement is the transaction it " +
			"replaces")

		return
	}
	m.constructionDone(res)
}

// constructionFailed aborts the negotiation.
func (m *Machine) constructionFailed(err error) {
	var abortErr *interactivetx.AbortError
	switch {
	case errors.As(err, &abortErr):
		m.abort(abortErr.Reason, "")

	case errors.Is(err, interactivetx.ErrNotOurTurn):
		m.warnAndDisconnect("%v", err)

		// A replacement: the splice it was to replace stays.
		if len(m.active.replaced) > 0 {
			m.forgetReplacement()
			return
		}
		m.setActive(nil)

		return

	default:
		m.abort(codeInternal, "%v", err)
	}
	m.forget()
}

// constructionDone records the agreed transaction, adds the splice to the
// channel's funding set and sends our commitment_signed for it.
func (m *Machine) constructionDone(res *interactivetx.Result) {
	a := m.active
	if a.timeout != nil {
		a.timeout.Stop()
	}

	txid := res.Tx.TxHash()
	m.update(func(a *active) {
		a.res = res
		a.txid = txid
		a.fundingIndex = res.FundingIndex
		a.session = nil
		a.phase = phaseSigning
		a.agreedAt = time.Now()
	})
	m.startSigningTimeout()

	record := &channeldb.SpliceRecord{
		OutPoint:    wire.OutPoint{Hash: txid, Index: res.FundingIndex},
		Capacity:    a.capacity,
		LocalKey:    a.ourKey,
		RemoteKey:   a.theirKey,
		LocalDelta:  a.ourContribution,
		RemoteDelta: a.theirContribution,
		Negotiation: &channeldb.SpliceNegotiation{
			WeInitiated:  a.role == interactivetx.Initiator,
			Tx:           res.Tx,
			FeeRate:      uint64(a.feeRate),
			OurInputs:    coinOutpoints(a.ourInputs),
			Inputs:       spliceInputs(res),
			OursReplaced: a.oursReplaced,
			OurOutputs:   txOutputs(a.ourOutputs),
			AgreedAt:     a.agreedAt.Unix(),
		},
	}
	if err := m.cfg.Channel.State().PutSpliceRecord(record); err != nil {
		m.abort(codeInternal, "%v", err)
		m.forget()

		return
	}

	err := m.wait(m.cfg.Link.AddSpliceFunding(lnwallet.PendingFunding{
		OutPoint:    record.OutPoint,
		Capacity:    a.capacity,
		LocalKey:    a.ourKey,
		RemoteKey:   a.theirKey,
		LocalDelta:  a.ourContribution,
		RemoteDelta: a.theirContribution,
	}))
	if errors.Is(err, errStopping) {
		return
	}
	if err != nil {
		m.abort(codeInternal, "%v", err)
		m.forget()

		return
	}

	err = m.wait(m.cfg.Link.SendSpliceCommitment(txid))
	if errors.Is(err, errStopping) {
		return
	}
	if err != nil {
		m.abort(codeInternal, "%v", err)
		m.forget()

		return
	}
	m.update(func(a *active) { a.commitSent = true })

	m.maybeSendTxSignatures()
}

// forgetRecord deletes the record of a splice that never got into the
// channel's funding set.
func (m *Machine) forgetRecord(txid [32]byte) {
	err := m.cfg.Channel.State().DeleteSpliceRecord(txid)
	if err != nil && !errors.Is(err, channeldb.ErrSpliceRecordNotFound) {
		log.Errorf("ChannelID(%v): unable to delete splice record: %v",
			m.cfg.ChanID, err)
	}
}

// wait waits for a link request's result.
func (m *Machine) wait(result <-chan error) error {
	select {
	case err := <-result:
		if errors.Is(err, ErrConnectionGone) {
			return errStopping
		}

		return err
	case <-m.quit:
		return errStopping
	}
}

// ErrConnectionGone is what a link step returns when the connection its
// machine belongs to has ended: the next connection's machine carries on,
// so, as when stopping, nothing is undone.
var ErrConnectionGone = errors.New("the connection has ended")

// errStopping is what a link step waited for returns when the machine stops
// first: the step may well have been done, so nothing is undone; the next
// connection's machine carries on from what was recorded.
var errStopping = errors.New("splice machine stopping")

// txOutputs returns the outputs as transaction outputs.
func txOutputs(outputs []interactivetx.Output) []*wire.TxOut {
	var outs []*wire.TxOut
	for _, o := range outputs {
		outs = append(outs, &wire.TxOut{
			Value: int64(o.Value), PkScript: o.PkScript,
		})
	}

	return outs
}

// coinOutpoints returns the coins' outpoints.
func coinOutpoints(coins []Coin) []wire.OutPoint {
	ops := make([]wire.OutPoint, len(coins))
	for i, c := range coins {
		ops[i] = c.OutPoint
	}

	return ops
}
