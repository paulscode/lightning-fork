package htlcswitch

import (
	"context"
	"errors"
	"fmt"
	"github.com/btcsuite/btcd/btcutil"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// The link's part in splicing. The splice machine negotiates, talks to the
// wallet and persists; everything that touches commitments happens here, on
// the link's goroutine and in the order of the peer's messages: the splice's
// own commitment_signed, batches of commitment_signed while a splice is
// pending, and completing a splice. The machine and the link talk only
// through non-blocking calls.

// SpliceHandler is a channel's splice machine as its link sees it.
type SpliceHandler interface {
	// HandleSpliceMessage takes a splice message from the peer, in the
	// order the peer sent it: splice_init, splice_ack, the interactive
	// transaction messages, tx_signatures, tx_abort, splice_locked. It
	// must not block.
	HandleSpliceMessage(msg lnwire.Message)

	// SpliceCommitReceived reports the peer's commitment_signed for a
	// splice: nil once its signatures checked out and were recorded, the
	// reason otherwise. It must not block.
	SpliceCommitReceived(txid chainhash.Hash, err error)

	// ReestablishTLVs returns this node's splice fields of
	// channel_reestablish: next_funding while a splice's tx_signatures
	// haven't been received, and my_current_funding_locked, which is
	// always sent once splicing is negotiated.
	ReestablishTLVs() (nextFunding,
		currentLocked fn.Option[lnwire.FundingTxidFlags])

	// HandleReestablish takes the peer's channel_reestablish after the
	// link has resent what it owes (commitments, and the splice's
	// commitment_signed if asked for): the machine resends tx_signatures,
	// answers an unknown next_funding with tx_abort, and treats
	// my_current_funding_locked as splice_locked. It must not block.
	HandleReestablish(msg *lnwire.ChannelReestablish)

	// PeerInputHold returns what of the peer's balance it may not spend
	// yet: its coins in splices that locked and aren't deep enough to
	// rule out a reorganisation. Safe from any goroutine.
	PeerInputHold() btcutil.Amount

	// Constructing reports whether a splice's transaction is being
	// negotiated, or agreed and not yet among the channel's fundings.
	// Safe from any goroutine.
	Constructing() bool

	// Signing reports whether a splice is being signed, its
	// tx_signatures not exchanged both ways, and which side started it.
	// Safe from any goroutine.
	Signing() (bool, lntypes.ChannelParty)

	// LockComing reports whether a splice's lock is on its way to the
	// link: both splice_locked are in, or ours is and txid, the one the
	// peer just sent, completes it. The peer, locked on its side, may
	// send updates its new balance covers at once. Safe from any
	// goroutine.
	LockComing(txid chainhash.Hash) bool

	// TxSignaturesFor returns our tx_signatures for the splice
	// transaction the peer's next_funding names, if they went out: the
	// link resends them on reconnecting before anything else, and the
	// machine doesn't again. Safe from any goroutine.
	TxSignaturesFor(txid chainhash.Hash) *lnwire.TxSignatures

	// Disowned reports whether the peer disowned the splice, our updates
	// held (HoldUpdates) from the link's start, and whether the channel
	// is being closed for it. Safe from any goroutine.
	Disowned() (bool, bool)
}

// peerInputHeld reports whether the peer's balance is below what the splice
// machine holds of it: an HTLC the peer just added drew on coins it put into
// a splice that isn't deep enough yet.
func (l *channelLink) peerInputHeld() bool {
	if l.cfg.SpliceHandler == nil {
		return false
	}
	held := l.cfg.SpliceHandler.PeerInputHold()
	if held <= 0 {
		return false
	}
	remote := l.channel.StateSnapshot().RemoteBalance.ToSatoshis()

	return remote < held
}

var (
	// ErrSpliceUnsupported is returned for a splice request on a link
	// without a splice machine.
	ErrSpliceUnsupported = errors.New("splicing not enabled on this " +
		"channel")
)

// isSpliceMessage reports whether msg belongs to the splice machine.
func isSpliceMessage(msg lnwire.Message) bool {
	switch msg.(type) {
	case *lnwire.SpliceInit, *lnwire.SpliceAck, *lnwire.SpliceLocked,
		*lnwire.TxAddInput, *lnwire.TxAddOutput, *lnwire.TxRemoveInput,
		*lnwire.TxRemoveOutput, *lnwire.TxComplete,
		*lnwire.TxSignatures, *lnwire.TxInitRbf, *lnwire.TxAckRbf,
		*lnwire.TxAbort:

		return true
	}

	return false
}

// commitBatch collects the commitment_signed messages announced by a
// start_batch.
type commitBatch struct {
	size int
	sigs []*lnwire.CommitSig
}

// handleStartBatch starts collecting a batch. One that isn't for
// commitment_signed, or of one message, is ignored, as the spec says; one
// above the limit, or one started inside another, is a protocol violation.
func (l *channelLink) handleStartBatch(msg *lnwire.StartBatch) error {
	msgType, ok := msg.BatchMessageType.UnwrapOrErr(errors.New("none"))
	switch {
	case l.batch != nil:
		return fmt.Errorf("start_batch inside a batch")

	case ok != nil || msgType != lnwire.MsgCommitSig:
		l.log.Warnf("ignoring start_batch for message type %v",
			msg.BatchMessageType)

		return nil

	case msg.BatchSize <= 1:
		l.log.Warnf("ignoring start_batch of %d", msg.BatchSize)

		return nil

	case msg.BatchSize > lnwire.MaxBatchSize:
		return fmt.Errorf("start_batch of %d, above %d", msg.BatchSize,
			lnwire.MaxBatchSize)
	}

	l.batch = &commitBatch{size: int(msg.BatchSize)}

	return nil
}

// batchFail fails the link for a batch that breaks the protocol. The peer's
// signatures can't be trusted to cover every funding, so the channel is
// failed, as for any invalid commitment.
func (l *channelLink) batchFail(format string, args ...interface{}) {
	l.failf(LinkFailureError{
		code:          ErrInvalidCommitment,
		FailureAction: LinkFailureForceClose,
	}, format, args...)
}

// processCommitBatch sorts a complete batch by funding: the member for the
// current funding, one per pending splice, and members for fundings dropped
// when a splice locked, which the peer may have signed before it learned of
// the lock and which are ignored. The commitment is then checked on every
// funding at once.
func (l *channelLink) processCommitBatch(ctx context.Context,
	sigs []*lnwire.CommitSig) error {

	current := l.channel.ChannelState().CurrentFundingOutpoint().Hash
	pending := make(map[chainhash.Hash]bool)
	for _, f := range l.channel.PendingFundings() {
		pending[f.OutPoint.Hash] = true
	}

	var (
		main    *lnwire.CommitSig
		members []lnwallet.SpliceCommitSigs
		seen    = make(map[chainhash.Hash]bool)
	)
	for _, sig := range sigs {
		txid, err := sig.FundingTxid.UnwrapOrErr(
			errors.New("no funding_txid"),
		)
		if err != nil {
			return err
		}
		if seen[txid] {
			return fmt.Errorf("funding %v signed twice in a batch",
				txid)
		}
		seen[txid] = true

		switch {
		case txid == current:
			main = sig

		case pending[txid]:
			auxSigBlob, err := sig.CustomRecords.Serialize()
			if err != nil {
				return err
			}
			members = append(members, lnwallet.SpliceCommitSigs{
				FundingTxid: txid,
				CommitSigs: &lnwallet.CommitSigs{
					CommitSig:  sig.CommitSig,
					HtlcSigs:   sig.HtlcSigs,
					PartialSig: sig.PartialSig,
					AuxSigBlob: auxSigBlob,
				},
			})

		case l.channel.IsObsoleteFunding(txid):
			l.log.Debugf("ignoring commitment_signed for "+
				"funding %v, replaced by a splice", txid)

		default:
			return fmt.Errorf("commitment_signed for unknown "+
				"funding %v", txid)
		}
	}
	if main == nil {
		return fmt.Errorf("batch without the current funding %v",
			current)
	}

	return l.processRemoteCommitSig(ctx, main, members)
}

// handleBatchedCommitSig adds a commitment_signed to the batch being
// collected, and processes the batch once complete.
func (l *channelLink) handleBatchedCommitSig(ctx context.Context,
	msg *lnwire.CommitSig) error {

	l.batch.sigs = append(l.batch.sigs, msg)
	if len(l.batch.sigs) < l.batch.size {
		return nil
	}

	sigs := l.batch.sigs
	l.batch = nil

	if err := l.processCommitBatch(ctx, sigs); err != nil {
		l.batchFail("invalid commitment batch: %v", err)

		return err
	}

	return nil
}

// commitSigPlace says what a commitment_signed outside a batch is.
type commitSigPlace uint8

const (
	// commitSigCurrent is a commitment on the current funding.
	commitSigCurrent commitSigPlace = iota

	// commitSigSplice is a pending splice's own commitment_signed.
	commitSigSplice

	// commitSigWait names a funding a splice step on the link's queue
	// is still adding or locking: it waits for the step.
	commitSigWait

	// commitSigStale names a funding no splice will add.
	commitSigStale
)

// placeCommitSig says what a commitment_signed outside a batch is, by the
// funding it names:
//   - none, or the current funding: a commitment on it;
//   - a pending splice the peer said it locked, whose own commitment_signed
//     has come: the peer's first commitment after the lock, which waits
//     for our lock to reach the link;
//   - any other pending splice: its own commitment_signed, also when the
//     peer resends it for next_funding;
//   - a funding we don't have while a splice's transaction is being
//     negotiated or added: the splice's own, which waits for it;
//   - any other: one for a splice given up, ignored.
//
// Taken for a commitment on the current funding, either of the waiting ones
// failed its signature check and the channel was force closed.
func (l *channelLink) placeCommitSig(msg *lnwire.CommitSig) commitSigPlace {
	// Without splicing every commitment is on the current funding, and
	// one naming another is refused by its signature check, as before.
	txid, err := msg.FundingTxid.UnwrapOrErr(errors.New("none"))
	if err != nil || l.cfg.SpliceHandler == nil {
		return commitSigCurrent
	}
	if txid == l.channel.ChannelState().CurrentFundingOutpoint().Hash {
		return commitSigCurrent
	}
	for _, f := range l.channel.PendingFundings() {
		if f.OutPoint.Hash != txid {
			continue
		}
		_, locked := l.peerLocked[txid]
		if locked && l.channel.SpliceCommitReceived(txid) {
			return commitSigWait
		}

		return commitSigSplice
	}
	if l.cfg.SpliceHandler != nil && l.cfg.SpliceHandler.Constructing() {
		return commitSigWait
	}

	return commitSigStale
}

// handleSpliceCommitSig checks the peer's commitment_signed for a splice and
// reports the result to the splice machine, which decides what follows
// (tx_signatures, or tx_abort). No revoke_and_ack answers it.
func (l *channelLink) handleSpliceCommitSig(msg *lnwire.CommitSig) {
	txid := msg.FundingTxid.UnsafeFromSome()
	err := l.channel.ReceiveSpliceCommitment(txid, &lnwallet.CommitSigs{
		CommitSig: msg.CommitSig,
		HtlcSigs:  msg.HtlcSigs,
	})
	if err != nil {
		l.log.Errorf("commitment_signed for splice %v refused: %v",
			txid, err)
	}

	if l.cfg.SpliceHandler != nil {
		l.cfg.SpliceHandler.SpliceCommitReceived(txid, err)
	}
}

// handleSpliceMessage hands a splice message to the splice machine.
func (l *channelLink) handleSpliceMessage(msg lnwire.Message) {
	if l.cfg.SpliceHandler == nil {
		if !l.unknownMessageLogged {
			l.log.Warnf("received %T without splicing enabled",
				msg)
			l.unknownMessageLogged = true
		}

		return
	}

	l.cfg.SpliceHandler.HandleSpliceMessage(msg)
}

// commitSigMessages returns the messages that carry a new commitment: one
// commitment_signed, or while splices are pending a start_batch and one per
// funding, the current one first. With splicing on, each names its funding.
func (l *channelLink) commitSigMessages(main *lnwire.CommitSig,
	members []lnwallet.SpliceCommitSigs) []lnwire.Message {

	if l.cfg.SpliceHandler != nil || len(members) > 0 {
		main.FundingTxid = fn.Some(
			l.channel.ChannelState().CurrentFundingOutpoint().Hash,
		)
	}
	if len(members) == 0 {
		return []lnwire.Message{main}
	}

	msgs := []lnwire.Message{
		&lnwire.StartBatch{
			ChannelID:        l.ChanID(),
			BatchSize:        uint16(1 + len(members)),
			BatchMessageType: fn.Some(lnwire.MessageType(lnwire.MsgCommitSig)),
		},
		main,
	}
	for _, m := range members {
		msgs = append(msgs, &lnwire.CommitSig{
			ChanID:      l.ChanID(),
			CommitSig:   m.CommitSig,
			HtlcSigs:    m.HtlcSigs,
			FundingTxid: fn.Some(m.FundingTxid),
		})
	}

	return msgs
}

// spliceReq is a splice step run on the link's goroutine.
type spliceReq = fn.Req[func(context.Context) error, error]

// runSpliceReq queues f to run on the link's goroutine and returns its
// result.
func (l *channelLink) runSpliceReq(f func(context.Context) error) <-chan error {
	req, out := fn.NewReq[func(context.Context) error, error](f)

	select {
	case l.spliceReqs <- req:
	case <-l.cg.Done():
		req.Resolve(ErrLinkShuttingDown)
	}

	return out
}

// AddSpliceFunding adds a splice whose transaction was agreed to the
// channel's funding set, before its commitments are exchanged.
func (l *channelLink) AddSpliceFunding(f lnwallet.PendingFunding) <-chan error {
	return l.runSpliceReq(func(context.Context) error {
		if l.cfg.SpliceHandler == nil {
			return ErrSpliceUnsupported
		}

		return l.channel.AddPendingFunding(f)
	})
}

// SendSpliceCommitment signs the peer's commitment on a splice's funding and
// sends it: the splice's own commitment_signed. It is sent before the result
// is returned, so the caller's next message follows it.
func (l *channelLink) SendSpliceCommitment(txid chainhash.Hash) <-chan error {
	return l.runSpliceReq(func(ctx context.Context) error {
		sigs, err := l.channel.SignSpliceCommitment(ctx, txid)
		if err != nil {
			return err
		}

		return l.cfg.Peer.SendMessage(false, &lnwire.CommitSig{
			ChanID:      l.ChanID(),
			CommitSig:   sigs.CommitSig,
			HtlcSigs:    sigs.HtlcSigs,
			FundingTxid: fn.Some(txid),
		})
	})
}

// RemoveSpliceFunding drops a splice that will never lock, on the link's
// goroutine so no batch is half processed around it.
func (l *channelLink) RemoveSpliceFunding(txid chainhash.Hash) <-chan error {
	return l.runSpliceReq(func(context.Context) error {
		return l.channel.RemovePendingFunding(txid)
	})
}

// LockSplice completes a splice both sides have locked: the channel moves to
// its funding output. Updates held for a splice the peer disowned go on, an
// earlier transaction of it having locked: the funding the peer left out is
// gone with the lock. Not when the channel is being closed for it (the
// disowned one itself locking): our shutdown may be out.
func (l *channelLink) LockSplice(txid chainhash.Hash,
	scid lnwire.ShortChannelID) <-chan error {

	return l.runSpliceReq(func(context.Context) error {
		err := l.channel.LockPendingFunding(txid, scid)
		if err != nil {
			return err
		}
		if l.cfg.SpliceHandler != nil {
			if _, closing := l.cfg.SpliceHandler.Disowned(); closing {
				return nil
			}
		}
		l.releaseSpliceHold()

		return nil
	})
}

// ErrShutdownReceived refuses our request for quiescence once the peer sent
// shutdown: BOLT 2 allows no splice from then on, the one thing it is for.
var ErrShutdownReceived = errors.New("the peer sent shutdown")

// ShutdownReceived tells the link the peer sent shutdown for the channel, as
// the message is read: our requests for quiescence are refused from then on,
// and one whose stfu is still owed, waiting for our updates to be locked in,
// is withdrawn, so no stfu of ours follows the peer's shutdown. Safe from any
// goroutine.
func (l *channelLink) ShutdownReceived() {
	if l.quiescer.RefuseInit(ErrShutdownReceived) {
		l.log.Infof("withdrew our stfu: %v", ErrShutdownReceived)
	}
}

// HoldUpdates stops our updates on a channel whose peer disowned a splice:
// its commitments leave that funding out, ours on it must stay current, so
// no commitment can be signed. No HTLC is forwarded into the channel, nor an
// update_fee sent, until a splice locks or the channel closes.
func (l *channelLink) HoldUpdates() <-chan error {
	return l.runSpliceReq(func(context.Context) error {
		l.holdSpliceUpdates()

		return nil
	})
}

// holdSpliceUpdates holds our updates, once. Adds are disabled, unless a
// close disabled them already, which the release then leaves as it is.
func (l *channelLink) holdSpliceUpdates() {
	if l.spliceHeld {
		return
	}
	l.spliceHeld = true
	l.spliceHeldAdds = l.DisableAdds(Outgoing)
}

// releaseSpliceHold ends the hold on our updates, adds enabled again only if
// the hold disabled them.
func (l *channelLink) releaseSpliceHold() {
	if !l.spliceHeld {
		return
	}
	l.spliceHeld = false
	if l.spliceHeldAdds {
		l.EnableAdds(Outgoing)
	}
	l.spliceHeldAdds = false
}

// amendChanSync adds the splice machine's fields to our channel_reestablish.
func (l *channelLink) amendChanSync(msg *lnwire.ChannelReestablish) {
	if l.cfg.SpliceHandler == nil {
		return
	}

	msg.NextFunding, msg.MyCurrentFundingLocked =
		l.cfg.SpliceHandler.ReestablishTLVs()
}

// namesSplice reports whether either channel_reestablish names a splice
// transaction: then channel_ready isn't sent again.
func (l *channelLink) namesSplice(msgs ...*lnwire.ChannelReestablish) bool {
	original := l.channel.State().FundingOutpoint.Hash
	for _, m := range msgs {
		for _, f := range []fn.Option[lnwire.FundingTxidFlags]{
			m.NextFunding, m.MyCurrentFundingLocked,
		} {
			named := fn.MapOptionZ(f,
				func(f lnwire.FundingTxidFlags) bool {
					return f.Txid != original
				},
			)
			if named {
				return true
			}
		}
	}

	return false
}

// resendSpliceCommitSig sends our commitment_signed for a splice again when
// the peer's next_funding asks for it and we have one, before the splice
// machine resends anything that follows it.
func (l *channelLink) resendSpliceCommitSig(remote *lnwire.ChannelReestablish) {
	next, err := remote.NextFunding.UnwrapOrErr(errors.New("none"))
	if err != nil || next.RetransmitFlags&lnwire.RetransmitCommitSig == 0 {
		return
	}

	sig, err := l.channel.SpliceCommitSig(next.Txid)
	if err != nil {
		l.log.Debugf("no commitment_signed to resend for splice "+
			"%v: %v", next.Txid, err)

		return
	}
	if err := l.cfg.Peer.SendMessage(false, sig); err != nil {
		l.log.Errorf("failed to resend splice commitment_signed: %v",
			err)
	}
}

// resendTxSignatures resends our tx_signatures for the splice transaction
// the peer's next_funding names, if they went out.
func (l *channelLink) resendTxSignatures(txid chainhash.Hash) {
	sigs := l.cfg.SpliceHandler.TxSignaturesFor(txid)
	if sigs == nil {
		return
	}
	if err := l.cfg.Peer.SendMessage(false, sigs); err != nil {
		l.log.Errorf("failed to resend splice tx_signatures: %v", err)
	}
}

// QuiescenceInitiator returns which side started the channel's current
// quiescence, or an error when it isn't quiescent: only that side may send
// splice_init.
func (l *channelLink) QuiescenceInitiator() fn.Result[lntypes.ChannelParty] {
	return l.quiescer.QuiescenceInitiator()
}

// SpliceLink is a link's part in splicing, as the splice machine uses it.
type SpliceLink interface {
	QuiescenceInitiator() fn.Result[lntypes.ChannelParty]
	InitStfu() <-chan fn.Result[lntypes.ChannelParty]
	ResumeQuiescence() <-chan error
	AddSpliceFunding(f lnwallet.PendingFunding) <-chan error
	SendSpliceCommitment(txid chainhash.Hash) <-chan error
	LockSplice(txid chainhash.Hash, scid lnwire.ShortChannelID) <-chan error
	RemoveSpliceFunding(txid chainhash.Hash) <-chan error
	HoldUpdates() <-chan error
	ShutdownReceived()
}

// A compile time check that the link can splice.
var _ SpliceLink = (*channelLink)(nil)

// notePeerLocked records that the peer said it locked a splice.
func (l *channelLink) notePeerLocked(txid chainhash.Hash) {
	if l.peerLocked == nil {
		l.peerLocked = make(map[chainhash.Hash]struct{})
	}
	l.peerLocked[txid] = struct{}{}

	// Its splice_locked completing ours: the lock is on its way, and the
	// peer's updates wait for it (lockAwaited).
	if l.cfg.SpliceHandler != nil && l.cfg.SpliceHandler.LockComing(txid) {
		l.lockAwaited = txid
	}
}

// awaitingLock reports whether the peer's updates wait for a splice's lock
// to reach the link: the peer, locked on its side, sends updates its new
// balance covers, which the old funding's refused, an error the peer fails
// the channel on.
func (l *channelLink) awaitingLock() bool {
	if l.cfg.SpliceHandler == nil {
		return false
	}
	if l.lockAwaited != (chainhash.Hash{}) {
		current := l.channel.ChannelState().CurrentFundingOutpoint().Hash
		pending := false
		for _, f := range l.channel.PendingFundings() {
			if f.OutPoint.Hash == l.lockAwaited {
				pending = true
			}
		}
		if current == l.lockAwaited || !pending {
			l.lockAwaited = chainhash.Hash{}
		}
	}
	if l.lockAwaited != (chainhash.Hash{}) {
		return true
	}

	// Locked already, nothing pending: the machine's own bookkeeping
	// after the lock may still be under way, and the held messages are
	// replayed now, not at the held timeout.
	if len(l.channel.PendingFundings()) == 0 {
		return false
	}

	return l.cfg.SpliceHandler.LockComing(chainhash.Hash{})
}
