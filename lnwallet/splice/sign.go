package splice

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// The splice's signatures. Its own commitment_signed goes both ways first;
// then tx_signatures, the side that put in less first. From the moment ours
// leaves, the splice is ours to keep: it stays in the channel's funding set
// whatever the peer does.

// handleCommitReceived takes the link's result for the peer's
// commitment_signed for the splice.
func (m *Machine) handleCommitReceived(txid [32]byte, err error) {
	a := m.active
	if a == nil || a.txid != txid || a.phase != phaseSigning {
		log.Warnf("ChannelID(%v): commitment_signed for splice %x "+
			"not expected", m.cfg.ChanID, txid)
		return
	}

	if err != nil {
		// Refused before our tx_signatures left: nothing is lost.
		if a.ourTxSigs == nil {
			m.abort(codeInvalidCommitment, "%v", err)
			m.forget()
		}

		return
	}

	m.update(func(a *active) { a.commitReceived = true })
	m.maybeSendTxSignatures()
}

// signingFailed handles a failure to sign or record our tx_signatures. Before
// the peer's, the splice is aborted, which is safe: nobody can publish it.
// After the peer's, a tx_abort would read as the peer's own abandoning the
// splice after signing it, and the peer, a Lightning Fork node too, would
// close the channel for it: the splice is kept, still being signed, and the
// connection dropped, so the peer's channel_reestablish, naming the splice,
// has us try again, at each reconnection, until we can (a remote signer
// back, say).
func (m *Machine) signingFailed(err error) {
	if m.active.theirTxSigs == nil {
		m.abort(codeInternal, "%v", err)
		m.forget()

		return
	}

	// Failing for good (a remote signer with the wrong key, say): the
	// channel would stay quiescent with no end. Our tx_signatures never
	// left, so nobody can publish the splice, and our commitment on the
	// current funding is current: force closed, a while after the first
	// failure (recorded, so reconnections don't start it again; not from
	// the agreement, or a node down for days would close at its first
	// transient failure).
	a := m.active
	now := time.Now()
	m.update(func(a *active) {
		if a.signFailedAt.IsZero() {
			a.signFailedAt = now
		}
		a.signFailures++
	})
	werr := m.cfg.Channel.State().UpdateSpliceRecord(a.txid,
		func(r *channeldb.SpliceRecord) error {
			if r.Negotiation != nil {
				r.Negotiation.SignFailedAt = a.signFailedAt.Unix()
				r.Negotiation.SignFailures = a.signFailures
			}

			return nil
		})
	if werr != nil {
		log.Errorf("ChannelID(%v): unable to record the failure to "+
			"sign splice %v: %v", m.cfg.ChanID, a.txid, werr)
	}
	// A day since the first, and a few attempts: not two transient
	// failures with a long time offline between them.
	if time.Since(a.signFailedAt) > signingGiveUp &&
		a.signFailures >= signingGiveUpAttempts && m.cfg.Close != nil {

		log.Errorf("ChannelID(%v): unable to sign splice %v for over "+
			"%v; force closing: %v", m.cfg.ChanID, a.txid,
			signingGiveUp, err)
		m.cfg.Close(true)

		return
	}

	log.Errorf("ChannelID(%v): unable to sign splice %v, the peer's "+
		"tx_signatures in; trying again on reconnecting: %v",
		m.cfg.ChanID, a.txid, err)
	m.cfg.Peer.Disconnect(fmt.Errorf("unable to sign splice: %w", err))
}

// signingGiveUp is how long after our first failure to sign a splice, the
// peer's tx_signatures in, we keep failing before force closing.
const signingGiveUp = 24 * time.Hour

// signingGiveUpAttempts is how many failed attempts to sign, one per
// reconnection, come before giving up.
const signingGiveUpAttempts = 5

// signsFirst reports whether we send tx_signatures first.
func (m *Machine) signsFirst() bool {
	a := m.active

	return interactivetx.SignsFirst(
		a.res, a.role, m.cfg.OurNodeID[:], m.cfg.TheirNodeID[:],
	)
}

// maybeSendTxSignatures sends our tx_signatures once both commitments are
// exchanged, if it's our turn: we go first, or the peer's arrived.
func (m *Machine) maybeSendTxSignatures() {
	a := m.active
	if a == nil || a.phase != phaseSigning || a.ourTxSigs != nil ||
		!a.commitSent || !a.commitReceived {

		return
	}
	if !m.signsFirst() && a.theirTxSigs == nil {
		return
	}

	sigs, err := m.ourTxSignatures()
	if err != nil {
		m.signingFailed(err)
		return
	}

	// Signing last, the transaction is complete with ours: tested
	// against the backend's mempool, but not dropped if refused. The
	// peer signed it already: it keeps it whatever we say, and the Core
	// Lightning port fails the channel on a tx_abort once it signed, so
	// it goes on as a splice that may not relay, to bump or to die as
	// any stuck one.
	if !m.signsFirst() && m.cfg.CheckMempool != nil {
		tx, err := m.signedTxWith(a, sigs, a.theirTxSigs)
		if err == nil {
			err = m.cfg.CheckMempool(tx)
		}
		if err != nil {
			log.Warnf("ChannelID(%v): splice %v not accepted by "+
				"the mempool, signed all the same, the peer "+
				"having signed: %v", m.cfg.ChanID, a.txid, err)
		}
	}

	// Recorded before it leaves: from then on the splice is ours to
	// keep.
	err = m.cfg.Channel.State().UpdateSpliceRecord(a.txid,
		func(r *channeldb.SpliceRecord) error {
			if r.Negotiation == nil {
				return errors.New("splice record without " +
					"negotiation")
			}
			r.Negotiation.OurTxSignatures = sigs

			return nil
		})
	if err != nil {
		m.signingFailed(err)
		return
	}
	m.update(func(a *active) { a.ourTxSigs = sigs })
	m.send(sigs)
	m.notify("signed", a.txid, "", false)
	m.report(Update{State: StateSigned, Txid: a.txid})

	if a.theirTxSigs != nil {
		m.signed()
		return
	}

	// We went first: the peer holds what it needs to publish the
	// splice, with its tx_signatures or without sending them.
	m.watchWithheld()
}

// ourTxSignatures signs our part of the splice transaction: the shared
// input with our current funding key, and our wallet inputs.
func (m *Machine) ourTxSignatures() (*lnwire.TxSignatures, error) {
	a := m.active
	res := a.res
	fetcher := interactivetx.PrevOutFetcher(res)

	_, _, witnessScript, _, err := m.currentFunding()
	if err != nil {
		return nil, err
	}
	state := m.cfg.Channel.State()

	tx := res.Tx.Copy()
	sigHashes := txscript.NewTxSigHashes(tx, fetcher)
	shared := res.Inputs[res.SharedIndex]
	rawSig, err := m.cfg.Signer.SignOutputRaw(tx, &input.SignDescriptor{
		KeyDesc:       state.LocalChanCfg.MultiSigKey,
		WitnessScript: witnessScript,
		Output: &wire.TxOut{
			Value: int64(shared.Value), PkScript: shared.PkScript,
		},
		HashType:          interactivetx.SharedInputHashType,
		SigHashes:         sigHashes,
		PrevOutputFetcher: fetcher,
		InputIndex:        int(res.SharedIndex),
	})
	if err != nil {
		return nil, fmt.Errorf("shared input: %w", err)
	}

	// Checked against the shared input's digest before it leaves: a
	// remote signer that signed another digest would give the peer a
	// signature that can't complete the splice once ours is out.
	ecSig, ok := rawSig.(*ecdsa.Signature)
	if !ok {
		return nil, errors.New("shared input signature isn't ECDSA")
	}
	err = interactivetx.VerifySharedInputSig(
		res, witnessScript, ecSig, state.LocalChanCfg.MultiSigKey.PubKey,
		interactivetx.SharedInputHashType,
	)
	if err != nil {
		return nil, fmt.Errorf("our shared input signature: %w", err)
	}

	sharedSig, err := lnwire.NewSigFromSignature(rawSig)
	if err != nil {
		return nil, err
	}

	// Our wallet inputs, in serial id order as they are in the
	// transaction.
	var witnesses []wire.TxWitness
	for i, in := range res.Inputs {
		if in.Shared || !ourSerial(a.role, in.SerialID) {
			continue
		}
		taproot := txscript.IsPayToTaproot(in.PkScript)
		signDesc := &input.SignDescriptor{
			Output: &wire.TxOut{
				Value: int64(in.Value), PkScript: in.PkScript,
			},
			HashType:          input.SoleSignerSigHash(taproot),
			SigHashes:         sigHashes,
			PrevOutputFetcher: fetcher,
			InputIndex:        i,
		}
		if taproot {
			signDesc.SignMethod =
				input.TaprootKeySpendBIP0086SignMethod
		}
		script, err := m.cfg.Signer.ComputeInputScript(tx, signDesc)
		if err != nil {
			return nil, fmt.Errorf("input %v: %w", in.OutPoint, err)
		}
		witnesses = append(witnesses, script.Witness)
	}

	return &lnwire.TxSignatures{
		ChannelID:            m.cfg.ChanID,
		TxID:                 a.txid,
		Witnesses:            witnesses,
		SharedInputSignature: fn.Some(sharedSig),
	}, nil
}

// ourSerial reports whether role added the item with serial id.
func ourSerial(role interactivetx.Role, serialID uint64) bool {
	initiator := serialID%2 == 0
	if role == interactivetx.Initiator {
		return initiator
	}

	return !initiator
}

// handleTxSignatures takes the peer's tx_signatures: checked against the
// transaction under the chain's rules before anything else.
func (m *Machine) handleTxSignatures(msg *lnwire.TxSignatures) {
	a := m.active
	switch {
	// For the splice the channel is on now: it locked before they came.
	case a == nil && msg.TxID ==
		m.cfg.Channel.State().CurrentFundingOutpoint().Hash:

		return

	case a == nil || a.txid != msg.TxID:
		m.warnAndDisconnect("tx_signatures for unknown splice %v",
			msg.TxID)
		return

	case a.theirTxSigs != nil:
		// Sent again after a reconnection.
		return

	// Late: the splice confirmed before they came (a reconnection in
	// between), and went on without them.
	case a.phase == phasePending && a.ourTxSigs != nil:

	case a.phase != phaseSigning || !a.commitReceived:
		m.warnAndDisconnect("tx_signatures before commitment_signed")
		return

	case m.signsFirst() && a.ourTxSigs == nil:
		m.warnAndDisconnect("tx_signatures out of turn")
		return
	}

	if err := m.checkTheirTxSignatures(msg); err != nil {
		// Once ours left, the splice can't be forgotten: the peer
		// could still publish it. Otherwise refuse it.
		if a.ourTxSigs == nil {
			m.abort(codeInvalidSignatures, "%v", err)
			m.forget()
		} else {
			log.Errorf("ChannelID(%v): invalid tx_signatures for "+
				"splice %v after ours: %v", m.cfg.ChanID,
				a.txid, err)
			m.setMisbehaved()
		}

		return
	}

	err := m.cfg.Channel.State().UpdateSpliceRecord(a.txid,
		func(r *channeldb.SpliceRecord) error {
			r.Negotiation.TheirTxSignatures = msg
			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record tx_signatures: %v",
			m.cfg.ChanID, err)
		return
	}
	m.update(func(a *active) { a.theirTxSigs = msg })
	if a.phase == phasePending {
		return
	}

	if a.ourTxSigs == nil {
		m.maybeSendTxSignatures()
		return
	}
	m.signed()
}

// checkTheirTxSignatures checks the peer's witnesses for its inputs and its
// signature for the shared input, which must be for the unified digest.
func (m *Machine) checkTheirTxSignatures(msg *lnwire.TxSignatures) error {
	a := m.active
	peer := interactivetx.Initiator
	if a.role == interactivetx.Initiator {
		peer = interactivetx.Acceptor
	}

	err := interactivetx.CheckPeerWitnesses(
		a.res, peer, msg.Witnesses, true,
	)
	if err != nil {
		return err
	}

	sig, err := msg.SharedInputSignature.UnwrapOrErr(
		errors.New("no shared input signature"),
	)
	if err != nil {
		return err
	}
	ecSig, err := sig.ToSignature()
	if err != nil {
		return err
	}
	parsed, ok := ecSig.(*ecdsa.Signature)
	if !ok {
		return errors.New("shared input signature isn't ECDSA")
	}
	_, _, witnessScript, _, err := m.currentFunding()
	if err != nil {
		return err
	}

	return interactivetx.VerifySharedInputSig(
		a.res, witnessScript, parsed,
		m.cfg.Channel.State().RemoteChanCfg.MultiSigKey.PubKey,
		interactivetx.SharedInputHashType,
	)
}

// signedTx assembles the splice transaction with every witness.
func (m *Machine) signedTx() (*wire.MsgTx, error) {
	a := m.active

	return m.signedTxWith(a, a.ourTxSigs, a.theirTxSigs)
}

// signedTxWith is signedTx with the signatures given: ours not yet the
// splice's, the link reading them from another goroutine.
func (m *Machine) signedTxWith(a *active, ours,
	theirs *lnwire.TxSignatures) (*wire.MsgTx, error) {

	// One side's missing: the peer withheld or disowned its own.
	if ours == nil || theirs == nil {
		return nil, errors.New("tx_signatures missing")
	}

	res := a.res
	tx := res.Tx.Copy()

	ourShared, err := ours.SharedInputSignature.UnwrapOrErr(
		errors.New("no shared input signature of ours"),
	)
	if err != nil {
		return nil, err
	}
	theirShared, err := theirs.SharedInputSignature.UnwrapOrErr(
		errors.New("no shared input signature of theirs"),
	)
	if err != nil {
		return nil, err
	}
	ourSig, err := ourShared.ToSignature()
	if err != nil {
		return nil, err
	}
	theirSig, err := theirShared.ToSignature()
	if err != nil {
		return nil, err
	}

	_, _, witnessScript, _, err := m.currentFunding()
	if err != nil {
		return nil, err
	}
	state := m.cfg.Channel.State()
	tx.TxIn[res.SharedIndex].Witness = input.SpendMultiSig(
		witnessScript,
		state.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		ourSig,
		state.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		theirSig, interactivetx.SharedInputHashType,
	)

	ourW, theirW := ours.Witnesses, theirs.Witnesses
	for i, in := range res.Inputs {
		if in.Shared {
			continue
		}
		if ourSerial(a.role, in.SerialID) {
			tx.TxIn[i].Witness, ourW = ourW[0], ourW[1:]
		} else {
			tx.TxIn[i].Witness, theirW = theirW[0], theirW[1:]
		}
	}

	return tx, nil
}

// signed finishes the signing: the transaction is published, quiescence
// ends, and the splice waits for its confirmations.
func (m *Machine) signed() {
	a := m.active
	tx, err := m.signedTx()
	if err != nil {
		log.Errorf("ChannelID(%v): unable to assemble splice %v: %v",
			m.cfg.ChanID, a.txid, err)
		return
	}

	// Recorded before it goes out, so a restart searches the chain from
	// there.
	height, _ := m.cfg.Chain.BestHeight()
	err = m.cfg.Channel.State().UpdateSpliceRecord(a.txid,
		func(r *channeldb.SpliceRecord) error {
			if r.Negotiation.BroadcastHeight == 0 {
				r.Negotiation.BroadcastHeight = height
			}

			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record broadcast: %v",
			m.cfg.ChanID, err)
	}

	status := m.publish(tx)
	if status.State == RelayRejected {
		log.Errorf("ChannelID(%v): splice %v rejected: %v",
			m.cfg.ChanID, a.txid, status.Reason)
	}

	m.update(func(a *active) { a.phase = phasePending })
	m.report(Update{State: StateBroadcast, Txid: a.txid})
	m.resume()
	m.watchConfirmation(height)
	m.watchPending(height)
}

// label returns the splice transaction's wallet label.
func (m *Machine) label() string {
	dir := DirectionIn
	if m.active.ourContribution < 0 {
		dir = DirectionOut
	}

	return Label(m.cfg.Channel.State().ShortChannelID, dir)
}

// handleTxAbort takes the peer's tx_abort. Before our tx_signatures the
// splice is forgotten and the abort echoed back; after, it is kept, since
// the peer could still publish it, and the peer's splices are refused from
// then on.
func (m *Machine) handleTxAbort(msg *lnwire.TxAbort) {
	a := m.active
	reason := printable(msg.Data)

	// The peer's answer to ours, as BOLT 2 asks of it.
	if m.abortSent {
		m.abortSent = false
		log.Debugf("ChannelID(%v): tx_abort echoed: %v", m.cfg.ChanID,
			reason)

		return
	}

	switch {
	// An empty one is an echo: ours carry no reason, nor the Core
	// Lightning port's, while every first tx_abort, ours and the port's,
	// gives one. Not answered, or two nodes echo each other for good.
	// Like any tx_abort it ends the peer's quiescence, so it ends ours
	// too: the link would otherwise hold the peer's updates that follow
	// until the wait for them ran out.
	case a == nil && len(msg.Data) == 0:
		log.Debugf("ChannelID(%v): tx_abort echo without a splice",
			m.cfg.ChanID)
		m.resume()

	// One we no longer have, or never had: echoed, the port waiting for
	// the echo and taking nothing else until it comes. Not counted as
	// ours sent, so a second one on this connection is echoed too.
	case a == nil:
		log.Debugf("ChannelID(%v): tx_abort without a splice: %v",
			m.cfg.ChanID, reason)
		m.send(&lnwire.TxAbort{ChannelID: m.cfg.ChanID})

		// A quiescence the peer won ends with it: it ends its own.
		m.resume()

	// After our tx_signatures, the peer's not in: the Core Lightning port
	// aborts there in its normal course, waiting for the echo and taking
	// nothing else until it comes. Echoed, quiescence ended, nothing held
	// against the peer. The splice is kept either way: the peer holds our
	// tx_signatures and could still publish it, and dropped, the channel
	// moving on would revoke every commitment on its output, a peer then
	// publishing it holding the funds there. Kept, the channel's state on
	// it stays current.
	//
	// Answering the next_funding of our reestablish on this connection
	// (its user's signatures still missing), the port keeps the splice,
	// and it goes on. Otherwise (its user's abort) the port drops it:
	// recorded disowned, before the echo (the port forgets it on the
	// echo), it is no longer being signed. The channel then takes no
	// update: the port's commitments leave the splice out, and ours on
	// it must stay current. Our updates are held before quiescence ends,
	// and the channel closed (closeDisowned).
	case (a.phase == phasePending || a.ourTxSigs != nil) &&
		a.theirTxSigs == nil:

		log.Warnf("ChannelID(%v): peer aborted splice %v after our "+
			"tx_signatures, before its own: %v", m.cfg.ChanID,
			a.txid, reason)

		// Only the first answers our reestablish.
		m.mu.Lock()
		named := m.namedNext == a.txid
		m.namedNext = chainhash.Hash{}
		m.mu.Unlock()
		if !named {
			m.disown(a)
			m.holdUpdates()
		}
		m.send(&lnwire.TxAbort{ChannelID: m.cfg.ChanID})
		m.resume()
		if !named {
			m.closeDisowned()
		}

	// Its tx_signatures in too: an abort it may not send. Marked, and the
	// connection dropped with a warning, which also ends the port's wait
	// for an echo.
	case a.phase == phasePending || a.ourTxSigs != nil:
		log.Errorf("ChannelID(%v): peer aborted splice %v after both "+
			"tx_signatures: %v", m.cfg.ChanID, a.txid, reason)
		m.setMisbehaved()
		m.warnAndDisconnect("tx_abort after both tx_signatures")

	default:
		log.Infof("ChannelID(%v): peer aborted the splice: %v",
			m.cfg.ChanID, reason)
		m.update(func(a *active) {
			a.abortReason = reason
			a.reasonFromPeer = true
		})
		m.send(&lnwire.TxAbort{ChannelID: m.cfg.ChanID})
		m.forget()
	}
}

// disown records a transaction of the splice the peer dropped after our
// tx_signatures as disowned: kept, but no longer being signed.
func (m *Machine) disown(a *active) {
	err := m.cfg.Channel.State().UpdateSpliceRecord(a.txid,
		func(r *channeldb.SpliceRecord) error {
			if r.Negotiation != nil {
				r.Negotiation.Disowned = true
			}

			return nil
		})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record splice %v "+
			"disowned: %v", m.cfg.ChanID, a.txid, err)
	}
	m.update(func(a *active) { a.disowned = true })
}

// holdUpdates holds our updates on a channel with a disowned splice.
func (m *Machine) holdUpdates() {
	select {
	case err := <-m.cfg.Link.HoldUpdates():
		if err != nil {
			log.Errorf("ChannelID(%v): unable to hold updates: %v",
				m.cfg.ChanID, err)
		}
	case <-m.quit:
	}
}

// closeDisowned closes a channel with a disowned splice, which can take no
// update. With an earlier transaction of the splice still live, which the
// peer keeps, nothing is closed: it locks in time, the disowned one with it,
// and the channel goes on. With HTLCs on it, which could never be settled,
// the channel is force closed, our commitment current, as with any update
// pending or commitment unanswered; otherwise it is closed cooperatively, the close spending the funding output the splice
// spends.
func (m *Machine) closeDisowned() {
	a := m.active
	if len(a.replaced) > 0 {
		log.Warnf("ChannelID(%v): the peer disowned splice %v; "+
			"updates held until an earlier transaction of it "+
			"locks", m.cfg.ChanID, a.txid)
		m.watchDisownedDeadline(a)

		return
	}
	if m.cfg.Close == nil {
		return
	}

	force := !m.cfg.Channel.IsChannelClean()
	how := "cooperatively"
	if force {
		how = "by force, its HTLCs or updates never to be settled"
	}
	log.Warnf("ChannelID(%v): the peer disowned splice %v we signed, and "+
		"the channel can take no update; closing it %v", m.cfg.ChanID,
		a.txid, how)
	m.cfg.Close(force)
}

// disownedHoldBlocks is how long a channel waits, held, for an earlier
// transaction of a disowned splice to confirm, from its broadcast.
const disownedHoldBlocks = 144

// watchDisownedDeadline force closes a channel held for a disowned splice
// whose earlier transactions none has confirmed disownedHoldBlocks after the
// latest was published: one paying too little to confirm, which the peer
// can't bump while the disowned one is ours (its tx_init_rbf refused), would
// hold the channel for good. Force closed, as a cooperative close waits for
// a splice not locked; our commitments on every funding are current. Once
// per splice and connection; the deadline is from the record, the same at
// every connection.
func (m *Machine) watchDisownedDeadline(a *active) {
	if a.deadlineWatched || m.cfg.Close == nil {
		return
	}
	a.deadlineWatched = true

	var from uint32
	for _, c := range a.replaced {
		from = max(from, c.broadcastHeight)
	}
	// Not recorded: from now.
	if from == 0 {
		h, err := m.cfg.Chain.BestHeight()
		if err != nil {
			log.Errorf("ChannelID(%v): unable to read the chain's "+
				"height: %v", m.cfg.ChanID, err)
			return
		}
		from = h
	}
	deadline := from + disownedHoldBlocks

	// One confirmed is on its way to its lock depth, which can be past
	// the deadline: not closed for it.
	var confirmed atomic.Bool
	for _, c := range a.replaced {
		txid := c.txid
		conf, err := m.cfg.Chain.RegisterConfirmationsNtfn(
			&txid, c.fundingScript, 1, from,
		)
		if err != nil {
			log.Errorf("ChannelID(%v): unable to watch splice %v: %v",
				m.cfg.ChanID, txid, err)
			continue
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer conf.Cancel()

			// Reorganised out, it no longer spares the channel.
			for {
				select {
				case _, ok := <-conf.Confirmed:
					if !ok {
						return
					}
					confirmed.Store(true)
				case _, ok := <-conf.NegativeConf:
					if !ok {
						return
					}
					confirmed.Store(false)
				case <-m.quit:
					return
				}
			}
		}()
	}

	epochs, err := m.cfg.Chain.RegisterBlockEpochNtfn(nil)
	if err != nil {
		log.Errorf("ChannelID(%v): unable to watch blocks: %v",
			m.cfg.ChanID, err)
		return
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer epochs.Cancel()

		for {
			select {
			case epoch, ok := <-epochs.Epochs:
				if !ok {
					return
				}
				if uint32(epoch.Height) < deadline {
					continue
				}
				// Checked again at each block past the
				// deadline while an earlier one stays
				// confirmed.
				if confirmed.Load() {
					continue
				}
				m.enqueue(func() {
					if m.active != a || !a.disowned ||
						len(a.replaced) == 0 {

						return
					}
					if confirmed.Load() {
						log.Infof("ChannelID(%v): an "+
							"earlier transaction of "+
							"disowned splice %v "+
							"confirmed; waiting for "+
							"its lock", m.cfg.ChanID,
							a.txid)
						return
					}
					log.Warnf("ChannelID(%v): no earlier "+
						"transaction of disowned splice "+
						"%v confirmed by height %v; "+
						"force closing", m.cfg.ChanID,
						a.txid, deadline)
					m.cfg.Close(true)
				})

				return

			case <-m.quit:
				return
			}
		}
	}()
}

// printable returns the peer's text as printable ASCII, at most 160 bytes.
func printable(data []byte) string {
	if len(data) > 160 {
		data = data[:160]
	}
	out := make([]byte, 0, len(data))
	for _, c := range data {
		if c >= 0x20 && c < 0x7f {
			out = append(out, c)
		}
	}

	return string(out)
}
