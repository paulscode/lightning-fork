package splice

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/interactivetx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// The splice machine of one channel. It negotiates a splice with the peer,
// talks to the wallet and the chain, and persists what it agreed before
// sending anything that depends on it; everything that touches commitments
// it asks of the channel's link, which runs it in the order of the peer's
// messages. It runs as one goroutine fed by a queue, so the link can hand it
// messages without blocking.

// Link is the channel's link as the machine uses it.
type Link interface {
	// QuiescenceInitiator returns which side started the channel's
	// current quiescence.
	QuiescenceInitiator() fn.Result[lntypes.ChannelParty]

	// InitStfu asks the link to quiesce the channel.
	InitStfu() <-chan fn.Result[lntypes.ChannelParty]

	// ResumeQuiescence ends the channel's quiescence.
	ResumeQuiescence() <-chan error

	// AddSpliceFunding adds an agreed splice to the channel's funding
	// set.
	AddSpliceFunding(f lnwallet.PendingFunding) <-chan error

	// SendSpliceCommitment signs and sends our commitment_signed for a
	// splice.
	SendSpliceCommitment(txid chainhash.Hash) <-chan error

	// LockSplice completes a splice both sides have locked.
	LockSplice(txid chainhash.Hash,
		scid lnwire.ShortChannelID) <-chan error

	// RemoveSpliceFunding drops a splice that will never lock from the
	// channel's funding set.
	RemoveSpliceFunding(txid chainhash.Hash) <-chan error

	// HoldUpdates stops our updates on the channel until a splice locks:
	// no HTLC forwarded into it, no update_fee.
	HoldUpdates() <-chan error
}

// Channel is the channel's state machine as the machine reads it.
type Channel interface {
	// State returns the channel's persisted state.
	State() *channeldb.OpenChannel

	// IsChannelClean reports whether neither commitment has HTLCs, and
	// every update is locked in both ways.
	IsChannelClean() bool

	// StateSnapshot copies the channel's state under its lock: read
	// before quiescence, the link may be changing it.
	StateSnapshot() *channeldb.ChannelSnapshot

	// PendingFundings returns the channel's pending splices.
	PendingFundings() []lnwallet.PendingFunding

	// RemovePendingFunding drops a splice that will never lock.
	RemovePendingFunding(txid chainhash.Hash) error

	// AvailableBalance returns what we can send now, on every funding.
	AvailableBalance() lnwire.MilliSatoshi
}

// Peer sends messages to the channel's peer.
type Peer interface {
	// SendMessage sends messages to the peer, in order.
	SendMessage(sync bool, msgs ...lnwire.Message) error

	// Disconnect ends the connection with the peer.
	Disconnect(reason error)
}

// Chain is what the machine needs from the chain backend.
type Chain interface {
	// RegisterConfirmationsNtfn notifies when a transaction reaches a
	// number of confirmations.
	RegisterConfirmationsNtfn(txid *chainhash.Hash, pkScript []byte,
		numConfs, heightHint uint32,
		opts ...chainntnfs.NotifierOption) (
		*chainntnfs.ConfirmationEvent, error)

	// RegisterSpendNtfn notifies when an output is spent.
	RegisterSpendNtfn(op *wire.OutPoint, pkScript []byte,
		heightHint uint32) (*chainntnfs.SpendEvent, error)

	// RegisterBlockEpochNtfn notifies of each new block.
	RegisterBlockEpochNtfn(*chainntnfs.BlockEpoch) (
		*chainntnfs.BlockEpochEvent, error)

	// BestHeight returns the height of the chain's tip.
	BestHeight() (uint32, error)

	// ConfirmedOutput returns an unspent output with its confirmations
	// and whether a coinbase transaction created it, or an error when
	// the backend doesn't know it as unspent.
	ConfirmedOutput(op wire.OutPoint) (*wire.TxOut, uint32, bool, error)

	// Synced reports whether this node has caught up with the chain: a
	// splice negotiated before would check the peer's coins and fees
	// against an old view of it.
	Synced() bool
}

// Policy is this node's splice policy.
type Policy struct {
	// MaxChannelSize is the largest capacity a peer's splice may give a
	// channel, as for an inbound open.
	MaxChannelSize btcutil.Amount

	// LockDepth returns the confirmations at which a splice locks, given
	// what the peer put into it.
	LockDepth func(peerContribution btcutil.Amount) uint32

	// NegotiationTimeout ends a negotiation that doesn't finish: the
	// channel is quiescent meanwhile.
	NegotiationTimeout time.Duration

	// MaxPeerAddMessages bounds the peer's tx_add_input and
	// tx_add_output messages, each.
	MaxPeerAddMessages int

	// PeerInputConfs is how deep a locked splice must be before the
	// link lets the peer spend what it put in: 0 holds nothing.
	PeerInputConfs uint32
}

// Config is a channel's splice machine's environment.
type Config struct {
	// ChanID is the channel's id.
	ChanID lnwire.ChannelID

	// OurNodeID and TheirNodeID are the two nodes' public keys, which
	// order tx_signatures on a tie.
	OurNodeID, TheirNodeID [33]byte

	// Link, Channel and Peer are the channel's link, state machine and
	// peer.
	Link    Link
	Channel Channel
	Peer    Peer

	// Chain is the chain backend.
	Chain Chain

	// Wallet holds this node's coins, and CoinSource lists them.
	Wallet     Wallet
	CoinSource CoinSource

	// WithCoinSelectLock runs coin selection and leasing under the
	// wallet's coin selection lock. Nil runs it without.
	WithCoinSelectLock func(func() error) error

	// ChangeScript returns a fresh change script of the wallet.
	ChangeScript func() ([]byte, error)

	// CheckMempool tests whether the backend would accept a fully signed
	// transaction, before our tx_signatures leave when we sign last. Nil
	// skips the test.
	CheckMempool func(tx *wire.MsgTx) error

	// Reserve, if set, returns what the wallet keeps for fee-bumping the
	// closes of its anchor channels: our splices don't spend it.
	Reserve func() (btcutil.Amount, error)

	// IsOurOutpoint reports whether an outpoint is one of this node's own:
	// a wallet coin, locked or not, or any channel's funding output. The
	// peer may not add one as its input.
	IsOurOutpoint func(op wire.OutPoint) bool

	// KeyRing derives this node's funding keys.
	KeyRing keychain.KeyRing

	// Signer signs the shared input and our wallet inputs.
	Signer input.Signer

	// Estimator gives fee estimates for the fee floors.
	Estimator chainfee.Estimator

	// DustLimit returns the dust limit of an output with a given
	// script, at our backend's relay rate.
	DustLimit func(pkScript []byte) btcutil.Amount

	// Policy is this node's splice policy.
	Policy Policy

	// Disabled refuses new splices, ours and the peer's, while a splice
	// already started carries on: splicing was turned off with one
	// pending somewhere.
	Disabled bool

	// NotifyEvent, if set, is told of each step of the channel's splices:
	// "started", "signed", "locked", "aborted".
	NotifyEvent func(kind string, txid chainhash.Hash, reason string,
		fromPeer bool)

	// AnnouncementPending, if set, reports whether a public channel's
	// announcement on the funding output a splice moved it to still
	// lacks the peer's half: channel_reestablish then asks for it again.
	AnnouncementPending func() bool

	// ResendAnnouncement, if set, sends our half of that announcement
	// again, as the peer's channel_reestablish asks.
	ResendAnnouncement func()

	// Close, if set, starts closing the channel, by force or
	// cooperatively unless a close is under way, without waiting.
	Close func(force bool)

	// Limits, shared by the node's machines, bounds the splices peers
	// start. Nil for none.
	Limits *Limits
}

// notify tells NotifyEvent of a step.
func (m *Machine) notify(kind string, txid chainhash.Hash, reason string,
	fromPeer bool) {

	if m.cfg.NotifyEvent != nil {
		m.cfg.NotifyEvent(kind, txid, reason, fromPeer)
	}
}

// maxMoney is the most satoshis that can exist.
const maxMoney = btcutil.Amount(21_000_000 * btcutil.SatoshiPerBitcoin)

// Refusal codes, sent as tx_abort's reason and reported to the user.
const (
	codeDisabled          = "disabled"
	codeNoUnifiedSigs     = "no_unified_sigs"
	codeTaproot           = "taproot"
	codeZeroConf          = "zero_conf"
	codePending           = "pending"
	codeClosing           = "closing"
	codeNotQuiescent      = "not_quiescent"
	codeFeeBelowFloor     = "fee_below_floor"
	codeAboveMaxSize      = "above_max_size"
	codeAboveBalance      = "above_balance"
	codeChainNotSynced    = "chain_not_synced"
	codeTimeout           = "timeout"
	codeRbfUnsupported    = "rbf_unsupported"
	codeInvalidCommitment = "invalid_commitment"
	codeInvalidSignatures = "invalid_signatures"
	codeLocktime          = "locktime"
	codeInternal          = "internal_error"
	codePeerContribUnsupp = "peer_contribution_unsupported"
	codePeerMisbehaved    = "peer_misbehaved"
	codeInsufficientFunds = "insufficient_funds"
	codeUnknownCoin       = "unknown_coin"
	codeDisconnected      = "peer_disconnected"
)

// phase is where an active splice is.
type phase uint8

const (
	// phasePreparing: a splice we start, funds chosen, waiting for the
	// channel to be quiescent.
	phasePreparing phase = iota

	// phaseNegotiating: splice_init/splice_ack exchanged, the
	// interactive construction running.
	phaseNegotiating

	// phaseSigning: the transaction is agreed; commitments and
	// tx_signatures are being exchanged.
	phaseSigning

	// phasePending: both tx_signatures exchanged and the transaction
	// published; waiting for the lock depth and splice_locked.
	phasePending
)

// deadSplice is a dead splice's trace in the wallet: the transaction and
// the coins we put in.
type deadSplice struct {
	tx     *wire.MsgTx
	inputs []Coin
}

// active is the splice in progress on the channel, if any.
type active struct {
	phase phase

	// role is our role in the interactive construction.
	role interactivetx.Role

	// lockDone is set once the splice has locked.
	lockDone bool

	// session is the interactive construction, nil once done.
	session *interactivetx.Session

	// res is the agreed transaction.
	res *interactivetx.Result

	// txid names the splice.
	txid chainhash.Hash

	// fundingIndex is the new funding output's index.
	fundingIndex uint32

	// fundingScript and fundingWitness are the new funding output's
	// script and 2-of-2 witness script.
	fundingScript, fundingWitness []byte

	// ourKey is our key in the new funding output; theirKey the peer's.
	ourKey   keychain.KeyDescriptor
	theirKey *btcec.PublicKey

	// ourContribution and theirContribution are each side's change to
	// its own balance.
	ourContribution, theirContribution btcutil.Amount

	// capacity is the new funding output's value.
	capacity btcutil.Amount

	// feeRate is the transaction's fee rate.
	feeRate chainfee.SatPerKWeight

	// ourInputs are the wallet coins we put in.
	ourInputs []Coin

	// disowned is set when the peer aborted the transaction after our
	// tx_signatures, before its own, dropping it on its side: kept, as it
	// holds ours, but no longer being signed (Disowned in the record).
	disowned bool

	// deadlineWatched is set once watchDisownedDeadline runs for it.
	deadlineWatched bool

	// signFailedAt is when we first failed to sign it, the peer's
	// tx_signatures in (SignFailedAt in the record).
	signFailedAt time.Time

	// signFailures counts those failures (SignFailures in the record).
	signFailures uint32

	// inputsWatched is set once the transaction's inputs are watched for
	// a spend by another.
	inputsWatched bool

	// dropAtReestablish marks a splice restored without our
	// commitment_signed, dropped once the peer has reestablished.
	dropAtReestablish bool

	// agreedAt is when the transaction was agreed: the signing deadline
	// runs from it, across reconnections.
	agreedAt time.Time

	// commitSent and commitReceived record the splice's own
	// commitment_signed each way.
	commitSent, commitReceived bool

	// ourTxSigs and theirTxSigs are the tx_signatures each way.
	ourTxSigs, theirTxSigs *lnwire.TxSignatures

	// lockedSent and lockedReceived record splice_locked each way, and
	// scid is the splice's short channel id once it reached our depth.
	lockedSent, lockedReceived bool
	scid                       fn.Option[lnwire.ShortChannelID]

	// watching is set once the splice's confirmations are watched.
	watching bool

	// oursReplaced is set on the peer's replacement of a splice this node
	// started: the splice is still ours to report.
	oursReplaced bool

	// pendingWatched is set once the transaction is republished at every
	// block and its inputs watched (watchPendingOf).
	pendingWatched bool

	// broadcastHeight is when we first published it, from its record
	// after a restart.
	broadcastHeight uint32

	// replaced are the splice's earlier transactions, signed both ways,
	// which a replacement (the peer's tx_init_rbf) may yet lose to:
	// whichever confirms is the splice.
	replaced []*active

	// timeout ends a negotiation that takes too long.
	timeout *time.Timer

	// request is the splice we started, nil for the peer's; updates
	// reports its steps to whoever started it.
	request *Request
	updates chan Update

	// ourOutputs are the outputs we add: change, or the destination of
	// funds moved out.
	ourOutputs []interactivetx.Output

	// lockTime is the transaction's locktime, from our splice_init.
	lockTime uint32

	// abortReason is why the splice ended, for the report.
	abortReason    string
	reasonFromPeer bool
}

// Machine is a channel's splice machine.
type Machine struct {
	cfg Config

	// events is the machine's input, read by its goroutine only.
	events chan func()

	// mu guards what the link reads from its own goroutine: the
	// reestablish fields.
	mu sync.Mutex

	// active is the splice in progress, if any. Only the machine's
	// goroutine changes it, under mu.
	active *active

	// abortSent is set when we send tx_abort, until the peer's echo of
	// it or the next negotiation: the echo is not an abort of its own.
	abortSent bool

	// misbehaved is set when the peer aborted after we signed: its
	// splices are refused from then on.
	misbehaved bool

	// peerShutdown is set once the peer's shutdown for the channel has
	// been read on this connection: BOLT 2 allows no splice from then on,
	// ours or the peer's. Under mu.
	peerShutdown bool

	// reestablished is set once the peer's channel_reestablish has been
	// handled on this connection: splice_locked may be sent from then on.
	// lockedTold is the splice whose splice_locked the peer has had on
	// this connection, named by our channel_reestablish or sent, so it
	// isn't sent again (a peer takes the TLV for it and refuses a
	// duplicate). Both under mu.
	reestablished bool
	lockedTold    chainhash.Hash

	// namedNext is the splice transaction our channel_reestablish named
	// in next_funding on this connection. Under mu.
	namedNext chainhash.Hash

	// sigsResent is the splice transaction whose tx_signatures the link
	// sent again on reconnecting, before anything else: not sent twice.
	// Under mu.
	sigsResent chainhash.Hash

	// dead are splices given up on, waiting to leave the wallet.
	dead map[chainhash.Hash]deadSplice

	// holds are the peer's contributions to locked splices not yet
	// PeerInputConfs deep, by splice. Under mu.
	holds map[chainhash.Hash]btcutil.Amount

	// relay is each splice transaction's last relay status. Under mu.
	relay map[chainhash.Hash]RelayStatus

	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewMachine returns a channel's splice machine.
func NewMachine(cfg Config) *Machine {
	if cfg.Policy.NegotiationTimeout == 0 {
		cfg.Policy.NegotiationTimeout = 30 * time.Second
	}
	if cfg.Policy.MaxPeerAddMessages == 0 {
		cfg.Policy.MaxPeerAddMessages =
			interactivetx.DefaultMaxPeerAddMessages
	}

	return &Machine{
		cfg:    cfg,
		events: make(chan func(), 64),
		dead:   make(map[chainhash.Hash]deadSplice),
		holds:  make(map[chainhash.Hash]btcutil.Amount),
		relay:  make(map[chainhash.Hash]RelayStatus),
		quit:   make(chan struct{}),
	}
}

// Start restores a splice in progress from the channel's records and starts
// the machine.
func (m *Machine) Start() error {
	if err := m.restore(); err != nil {
		return err
	}
	if err := m.restoreHold(); err != nil {
		log.Errorf("ChannelID(%v): unable to hold the peer's coins "+
			"from the last splice: %v", m.cfg.ChanID, err)
	}

	m.wg.Add(1)
	go m.run()

	return nil
}

// Stop stops the machine. Calling it again does nothing.
func (m *Machine) Stop() {
	m.stopOnce.Do(func() {
		close(m.quit)
		m.wg.Wait()

		// The next machine takes the place again for a splice it
		// restores; one that doesn't survive isn't counted.
		m.mu.Lock()
		a := m.active
		m.mu.Unlock()
		// A peer's splice still being negotiated ends with the
		// connection and counts as an abort: else a peer could
		// start one, disconnect, and do it again for good.
		if a != nil && a.role == interactivetx.Acceptor {
			m.cfg.Limits.release(m, m.cfg.ChanID,
				a.phase == phaseNegotiating &&
					len(a.replaced) == 0)
		}
		if a != nil {
			m.endWithConnection(a)
		}
	})
}

// endWithConnection settles what the stopped machine leaves: a splice not
// yet agreed was never recorded and ends with the connection, its coins
// released and its request told; one recorded is carried on by the next
// connection's machine, and only the request's updates end here. Before,
// a peer that disconnected mid-negotiation left our coins leased for 14
// days and the request waiting for good. Called once the machine's
// goroutine is done.
func (m *Machine) endWithConnection(a *active) {
	switch {
	// A replacement being negotiated: the transaction it replaces is
	// still the splice, recorded, its coins in it. Only the request's
	// updates end.
	case len(a.replaced) > 0 && a.res == nil:
		m.mu.Lock()
		if a.updates != nil {
			close(a.updates)
			a.updates = nil
		}
		m.mu.Unlock()

	case a.phase == phasePreparing || a.phase == phaseNegotiating:
		if len(a.ourInputs) > 0 {
			err := ReleaseCoins(m.cfg.Wallet, a.ourInputs)
			if err != nil {
				log.Errorf("ChannelID(%v): unable to release "+
					"coins: %v", m.cfg.ChanID, err)
			}
		}
		if a.updates != nil {
			m.report(Update{
				State: StateAborted, Reason: codeDisconnected,
			})
		}

	default:
		m.mu.Lock()
		if a.updates != nil {
			close(a.updates)
			a.updates = nil
		}
		m.mu.Unlock()
	}
}

// run is the machine's goroutine.
func (m *Machine) run() {
	defer m.wg.Done()

	for {
		select {
		case ev := <-m.events:
			ev()

		case <-m.quit:
			return
		}
	}
}

// enqueue runs f on the machine's goroutine. It never blocks the caller
// beyond the queue's capacity; a full queue means a peer flooding the
// channel, and the connection is dropped.
func (m *Machine) enqueue(f func()) {
	select {
	case m.events <- f:
	case <-m.quit:
	default:
		log.Errorf("ChannelID(%v): splice queue full, disconnecting",
			m.cfg.ChanID)
		m.cfg.Peer.Disconnect(errors.New("splice message flood"))
	}
}

// HandleSpliceMessage takes a splice message from the peer.
func (m *Machine) HandleSpliceMessage(msg lnwire.Message) {
	m.enqueue(func() { m.handleMessage(msg) })
}

// SpliceCommitReceived takes the link's result for the peer's
// commitment_signed for a splice.
func (m *Machine) SpliceCommitReceived(txid chainhash.Hash, err error) {
	m.enqueue(func() { m.handleCommitReceived(txid, err) })
}

// setActive changes the splice in progress, under mu.
func (m *Machine) setActive(a *active) {
	m.mu.Lock()
	defer m.mu.Unlock()

	old := m.active
	if old != nil && old.timeout != nil {
		old.timeout.Stop()
	}
	m.active = a

	// A splice the peer started gives back its place in the limits;
	// ending without locking counts against the channel.
	if old != nil && old != a && old.role == interactivetx.Acceptor &&
		!old.oursReplaced {
		m.cfg.Limits.release(m, m.cfg.ChanID, !old.lockDone)
	}
}

// update runs f on the splice in progress under mu, so the link's reads see
// a consistent state.
func (m *Machine) update(f func(a *active)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.active != nil {
		f(m.active)
	}
}

// send sends messages to the peer, logging a failure: the peer gets them
// again on reconnecting where the protocol needs it.
func (m *Machine) send(msgs ...lnwire.Message) {
	if err := m.cfg.Peer.SendMessage(false, msgs...); err != nil {
		log.Errorf("ChannelID(%v): unable to send %T: %v",
			m.cfg.ChanID, msgs[0], err)
	}
}

// abort sends tx_abort with a reason.
func (m *Machine) abort(code string, format string, args ...interface{}) {
	reason := code
	if format != "" {
		reason = code + ": " + fmt.Sprintf(format, args...)
	}
	log.Infof("ChannelID(%v): splice aborted: %v", m.cfg.ChanID, reason)
	m.update(func(a *active) {
		if a.abortReason == "" {
			a.abortReason = reason
		}
	})

	m.abortSent = true
	m.send(&lnwire.TxAbort{
		ChannelID: m.cfg.ChanID, Data: []byte(reason),
	})
}

// warnAndDisconnect sends a warning and drops the connection, for a peer
// breaking the protocol.
func (m *Machine) warnAndDisconnect(format string, args ...interface{}) {
	text := fmt.Sprintf(format, args...)
	log.Warnf("ChannelID(%v): %v", m.cfg.ChanID, text)

	m.send(&lnwire.Warning{ChanID: m.cfg.ChanID, Data: []byte(text)})
	m.cfg.Peer.Disconnect(errors.New(text))
}

// resume ends the channel's quiescence.
func (m *Machine) resume() {
	select {
	case err := <-m.cfg.Link.ResumeQuiescence():
		if err != nil {
			log.Errorf("ChannelID(%v): unable to end quiescence: %v",
				m.cfg.ChanID, err)
		}
	case <-m.quit:
	}
}

// forget drops a splice that didn't get past negotiation or signing on our
// side: its funding leaves the channel's set, its coins are released, and
// quiescence ends.
func (m *Machine) forget() {
	a := m.active
	if a == nil {
		return
	}

	// A replacement we hadn't signed: the transaction it replaced is
	// the splice again.
	if len(a.replaced) > 0 && a.ourTxSigs == nil {
		m.forgetReplacement()
		return
	}

	// Out of the channel's funding set, which deletes its record; one
	// that never got in has only its record.
	if a.res != nil {
		err := m.cfg.Channel.RemovePendingFunding(a.txid)
		switch {
		case errors.Is(err, lnwallet.ErrUnknownFunding):
			m.forgetRecord(a.txid)

		case err != nil:
			log.Errorf("ChannelID(%v): unable to drop splice %v: %v",
				m.cfg.ChanID, a.txid, err)
		}
	}
	if len(a.ourInputs) > 0 {
		if err := ReleaseCoins(m.cfg.Wallet, a.ourInputs); err != nil {
			log.Errorf("ChannelID(%v): unable to release coins: %v",
				m.cfg.ChanID, err)
		}
	}

	m.recordLast("aborted", a.abortReason, a.reasonFromPeer)
	m.notify("aborted", a.txid, a.abortReason, a.reasonFromPeer)
	m.report(Update{
		State: StateAborted, Txid: a.txid, Reason: a.abortReason,
		ReasonFromPeer: a.reasonFromPeer,
	})
	m.setActive(nil)
	m.resume()
}

// recordLast records how the splice in progress ended, for the channel's
// listing.
func (m *Machine) recordLast(outcome, reason string, fromPeer bool) {
	a := m.active
	if a == nil {
		return
	}

	err := m.cfg.Channel.State().PutLastSplice(&channeldb.LastSplice{
		WeInitiated:    a.role == interactivetx.Initiator || a.oursReplaced,
		LocalDelta:     a.ourContribution,
		RemoteDelta:    a.theirContribution,
		Txid:           a.txid,
		Time:           time.Now().Unix(),
		Outcome:        outcome,
		Reason:         reason,
		ReasonFromPeer: fromPeer,
	})
	if err != nil {
		log.Errorf("ChannelID(%v): unable to record the last splice: "+
			"%v", m.cfg.ChanID, err)
	}
}

// handleMessage dispatches a splice message.
func (m *Machine) handleMessage(msg lnwire.Message) {
	switch msg := msg.(type) {
	case *lnwire.SpliceInit:
		m.handleSpliceInit(msg)

	case *lnwire.SpliceAck:
		m.handleSpliceAck(msg)

	case *lnwire.TxAddInput, *lnwire.TxAddOutput, *lnwire.TxRemoveInput,
		*lnwire.TxRemoveOutput, *lnwire.TxComplete:

		m.handleConstruction(msg)

	case *lnwire.TxSignatures:
		m.handleTxSignatures(msg)

	case *lnwire.TxAbort:
		m.handleTxAbort(msg)

	case *lnwire.TxInitRbf:
		m.handleTxInitRbf(msg)

	case *lnwire.TxAckRbf:
		m.warnAndDisconnect("tx_ack_rbf without our tx_init_rbf")

	case *lnwire.SpliceLocked:
		m.handleSpliceLocked(msg.SpliceTxid)

	default:
		log.Warnf("ChannelID(%v): unexpected splice message %T",
			m.cfg.ChanID, msg)
	}
}

// currentFunding returns the channel's current funding output: outpoint,
// value and the 2-of-2 witness script.
func (m *Machine) currentFunding() (wire.OutPoint, btcutil.Amount, []byte,
	[]byte, error) {

	state := m.cfg.Channel.State()
	witness, out, err := input.GenFundingPkScript(
		state.LocalChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		state.RemoteChanCfg.MultiSigKey.PubKey.SerializeCompressed(),
		int64(state.Capacity),
	)
	if err != nil {
		return wire.OutPoint{}, 0, nil, nil, err
	}

	return state.CurrentFundingOutpoint(), state.Capacity, witness,
		out.PkScript, nil
}

// setMisbehaved refuses the peer's splices on the channel from now on, on
// this connection and the next ones.
func (m *Machine) setMisbehaved() {
	m.mu.Lock()
	m.misbehaved = true
	m.mu.Unlock()
	m.cfg.Limits.markMisbehaved(m.cfg.ChanID)
}

// Refusal returns why a splice of ours would be refused now that the
// channel's own state doesn't show: the peer broke an earlier splice, or the
// chain isn't synced. "" otherwise. Safe from any goroutine: the estimate
// tells it before the user starts one.
func (m *Machine) Refusal() string {
	m.mu.Lock()
	misbehaved := m.misbehaved
	m.mu.Unlock()

	switch {
	case misbehaved || m.cfg.Limits.isMisbehaved(m.cfg.ChanID):
		return codePeerMisbehaved

	case m.cfg.Chain != nil && !m.cfg.Chain.Synced():
		return codeChainNotSynced

	// The peer's shutdown, which the channel's state doesn't show until
	// we answer it.
	case m.shutdownReceived():
		return codeClosing
	}

	return ""
}

// ShutdownReceived tells the machine the peer sent shutdown for the channel:
// no splice starts from then on. A splice of ours still waiting for
// quiescence fails with "closing", its coins released, and reports whether
// it did; one the peer's stfu answers later is ended with tx_abort. Called
// as the message is read, before the close begins: our answer to it, which
// the channel's state shows, comes later. Safe from any goroutine; it waits
// for the machine only with a splice of ours preparing.
func (m *Machine) ShutdownReceived() bool {
	m.mu.Lock()
	m.peerShutdown = true
	m.mu.Unlock()

	if !m.Preparing() {
		return false
	}

	result := make(chan bool, 1)
	m.enqueue(func() {
		a := m.active
		if a == nil || a.request == nil || a.phase != phasePreparing {
			result <- false
			return
		}
		m.failOurs(codeClosing, "the peer sent shutdown")
		result <- true
	})

	select {
	case ok := <-result:
		return ok
	case <-m.quit:
		return false
	}
}

// shutdownReceived reports whether the peer's shutdown has been read on this
// connection.
func (m *Machine) shutdownReceived() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.peerShutdown
}

// closing reports whether either side sent shutdown: BOLT 2 allows no
// splice from then on.
func (m *Machine) closing() bool {
	return m.shutdownReceived() || shutdownSent(m.cfg.Channel.State())
}

// spliceable returns why the channel can't be spliced now, or "".
func (m *Machine) spliceable() string {
	state := m.cfg.Channel.State()
	switch {
	case m.cfg.Disabled:
		return codeDisabled

	case !state.ChanType.HasUnifiedSigs():
		return codeNoUnifiedSigs

	case state.ChanType.IsTaproot():
		return codeTaproot

	case state.ChanType.HasZeroConf():
		return codeZeroConf

	case m.misbehaved || m.cfg.Limits.isMisbehaved(m.cfg.ChanID):
		return codePeerMisbehaved

	case m.cfg.Chain != nil && !m.cfg.Chain.Synced():
		return codeChainNotSynced

	case m.active != nil || len(m.cfg.Channel.PendingFundings()) > 0:
		return codePending

	case state.HasChanStatus(channeldb.ChanStatusCoopBroadcasted),
		state.HasChanStatus(channeldb.ChanStatusLocalCloseInitiator),
		state.HasChanStatus(channeldb.ChanStatusRemoteCloseInitiator),
		state.HasChanStatus(channeldb.ChanStatusFundingSpentUnknown),
		m.closing():

		return codeClosing
	}

	return ""
}

// shutdownSent reports whether we sent shutdown, starting a cooperative
// close or answering the peer's: BOLT 2 allows no splice from then on.
func shutdownSent(state *channeldb.OpenChannel) bool {
	if state.Db == nil {
		return false
	}
	info, err := state.ShutdownInfo()

	return err == nil && info.IsSome()
}
