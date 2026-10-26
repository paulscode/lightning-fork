package htlcswitch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// recordingSpliceHandler records what the link hands the splice machine,
// and gives it the reestablish fields to send.
type recordingSpliceHandler struct {
	mu            sync.Mutex
	messages      []lnwire.Message
	commits       []error
	reestablish   []*lnwire.ChannelReestablish
	nextFunding   fn.Option[lnwire.FundingTxidFlags]
	fundingLocked fn.Option[lnwire.FundingTxidFlags]
	held          btcutil.Amount
	constructing  bool
	signing       bool
	txSigs        *lnwire.TxSignatures
	lockComing    bool
	disowned      bool
	closing       bool
}

func (h *recordingSpliceHandler) Disowned() (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.disowned, h.disowned && h.closing
}

func (h *recordingSpliceHandler) LockComing(chainhash.Hash) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.lockComing
}

func (h *recordingSpliceHandler) TxSignaturesFor(
	txid chainhash.Hash) *lnwire.TxSignatures {

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.txSigs == nil || h.txSigs.TxID != txid {
		return nil
	}

	return h.txSigs
}

func (h *recordingSpliceHandler) Signing() (bool, lntypes.ChannelParty) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.signing, lntypes.Local
}

func (h *recordingSpliceHandler) Constructing() bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.constructing
}

func (h *recordingSpliceHandler) ReestablishTLVs() (
	fn.Option[lnwire.FundingTxidFlags],
	fn.Option[lnwire.FundingTxidFlags]) {

	h.mu.Lock()
	defer h.mu.Unlock()

	return h.nextFunding, h.fundingLocked
}

func (h *recordingSpliceHandler) HandleReestablish(
	msg *lnwire.ChannelReestablish) {

	h.mu.Lock()
	defer h.mu.Unlock()

	h.reestablish = append(h.reestablish, msg)
}

func (h *recordingSpliceHandler) PeerInputHold() btcutil.Amount {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.held
}

func (h *recordingSpliceHandler) HandleSpliceMessage(msg lnwire.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.messages = append(h.messages, msg)
}

func (h *recordingSpliceHandler) SpliceCommitReceived(_ chainhash.Hash,
	err error) {

	h.mu.Lock()
	defer h.mu.Unlock()

	h.commits = append(h.commits, err)
}

// messageCounter counts the messages a server receives, by type.
type messageCounter struct {
	mu     sync.Mutex
	counts map[lnwire.MessageType]int
	sigs   []*lnwire.CommitSig
}

func newMessageCounter(s *mockServer) *messageCounter {
	c := &messageCounter{counts: make(map[lnwire.MessageType]int)}
	s.intersect(func(m lnwire.Message) (bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()

		c.counts[m.MsgType()]++
		if sig, ok := m.(*lnwire.CommitSig); ok {
			c.sigs = append(c.sigs, sig)
		}

		return false, nil
	})

	return c
}

func (c *messageCounter) count(t lnwire.MessageType) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.counts[t]
}

// commitSigs returns the commitment_signed messages received so far.
func (c *messageCounter) commitSigs() []*lnwire.CommitSig {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]*lnwire.CommitSig(nil), c.sigs...)
}

// pendSplice gives both channels the same pending splice, Alice adding
// funds, and exchanges its own commitments, as the splice machine would
// before the links carry on.
func pendSplice(t *testing.T, alice, bob *lnwallet.LightningChannel,
	added btcutil.Amount) chainhash.Hash {

	t.Helper()

	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	as := alice.Signer.(*input.MockSigner)
	as.Privkeys = append(as.Privkeys, aliceKey)
	bs := bob.Signer.(*input.MockSigner)
	bs.Privkeys = append(bs.Privkeys, bobKey)

	txid := chainhash.Hash{0x5e, 0x11}
	op := wire.OutPoint{Hash: txid, Index: 0}
	capacity := alice.State().Capacity + added

	require.NoError(t, alice.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:   keychain.KeyDescriptor{PubKey: aliceKey.PubKey()},
		RemoteKey:  bobKey.PubKey(),
		LocalDelta: added,
	}))
	require.NoError(t, bob.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:    keychain.KeyDescriptor{PubKey: bobKey.PubKey()},
		RemoteKey:   aliceKey.PubKey(),
		RemoteDelta: added,
	}))

	sigs, err := alice.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	require.NoError(t, bob.ReceiveSpliceCommitment(txid, sigs))
	sigs, err = bob.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	require.NoError(t, alice.ReceiveSpliceCommitment(txid, sigs))

	return txid
}

// pay sends amount from Alice to Bob over the two hop network and waits for
// it to settle.
func pay(t *testing.T, n *twoHopNetwork, amount lnwire.MilliSatoshi) {
	t.Helper()

	htlcAmt, totalTimelock, hops := generateHops(
		amount, testStartingHeight, n.bobChannelLink,
	)
	_, err := makePayment(
		n.aliceServer, n.bobServer, n.bobChannelLink.ShortChanID(),
		hops, amount, htlcAmt, totalTimelock,
	).Wait(30 * time.Second)
	require.NoError(t, err)
}

// waitClean waits until both channels have nothing in flight.
func waitClean(t *testing.T, n *twoHopNetwork) {
	t.Helper()

	require.Eventually(t, func() bool {
		return n.aliceChannelLink.channel.IsChannelClean() &&
			n.bobChannelLink.channel.IsChannelClean()
	}, 10*time.Second, 50*time.Millisecond)
}

// TestLinkSpliceBatches checks the link while a splice is pending: a payment
// goes through with every commitment sent as a batch, one commitment_signed
// per funding, each naming its funding; both sides end with a commitment on
// the splice's funding they can publish. Once the splice locks, through the
// links, commitments go out alone, on the new funding.
func TestLinkSpliceBatches(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)

	const added = btcutil.SatoshiPerBitcoin
	txid := pendSplice(t, alice.channel, bob.channel, added)
	current := alice.channel.State().CurrentFundingOutpoint().Hash

	aliceHandler := &recordingSpliceHandler{}
	bobHandler := &recordingSpliceHandler{}
	n := newTwoHopNetwork(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) { c.SpliceHandler = aliceHandler },
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)
	atBob := newMessageCounter(n.bobServer)
	atAlice := newMessageCounter(n.aliceServer)

	pay(t, n, lnwire.NewMSatFromSatoshis(10_000_000))
	waitClean(t, n)

	// Every commitment went out as a batch: a start_batch and two
	// commitment_signed messages, one per funding.
	for _, c := range []*messageCounter{atBob, atAlice} {
		batches := c.count(lnwire.MsgStartBatch)
		require.Positive(t, batches)
		require.Equal(t, 2*batches, c.count(lnwire.MsgCommitSig))

		var onCurrent, onSplice int
		for _, sig := range c.commitSigs() {
			switch sig.FundingTxid.UnwrapOr(chainhash.Hash{}) {
			case current:
				onCurrent++
			case txid:
				onSplice++
			default:
				t.Fatalf("commitment_signed names %v",
					sig.FundingTxid)
			}
		}
		require.Equal(t, batches, onCurrent)
		require.Equal(t, batches, onSplice)
	}

	for _, link := range []*channelLink{
		n.aliceChannelLink, n.bobChannelLink,
	} {
		tx, err := link.channel.SignedSpliceCommitment(txid)
		require.NoError(t, err)
		require.Equal(t, txid, tx.TxIn[0].PreviousOutPoint.Hash)
	}

	// Lock through both links.
	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-n.aliceChannelLink.LockSplice(txid, scid))
	require.NoError(t, <-n.bobChannelLink.LockSplice(txid, scid))

	batchesBefore := atBob.count(lnwire.MsgStartBatch)
	sigsBefore := len(atBob.commitSigs())

	pay(t, n, lnwire.NewMSatFromSatoshis(5_000_000))
	waitClean(t, n)

	require.Equal(t, batchesBefore, atBob.count(lnwire.MsgStartBatch))
	after := atBob.commitSigs()[sigsBefore:]
	require.NotEmpty(t, after)
	for _, sig := range after {
		require.Equal(t, fn.Some(txid), sig.FundingTxid)
	}
	require.Equal(t, txid,
		n.aliceChannelLink.channel.State().CurrentFundingOutpoint().Hash)
	require.Equal(t,
		alice.channel.State().Capacity,
		btcutil.Amount(8*btcutil.SatoshiPerBitcoin+added),
	)
}

// spliceLinkHarness is a single link with splicing on, the peer's channel
// beside it, and what the link reports.
type spliceLinkHarness struct {
	link     *channelLink
	bob      *lnwallet.LightningChannel
	handler  *recordingSpliceHandler
	failures chan LinkFailureError
	sent     chan lnwire.Message
}

func newSpliceLinkHarness(t *testing.T) *spliceLinkHarness {
	t.Helper()

	return newSpliceLinkHarnessWith(t, nil)
}

// newSpliceLinkHarnessWith is newSpliceLinkHarness with the link's config
// changed by tweak before it starts.
func newSpliceLinkHarnessWith(t *testing.T,
	tweak func(*ChannelLinkConfig)) *spliceLinkHarness {

	t.Helper()

	return newSpliceLinkHarnessReserve(t, 0, tweak)
}

// newSpliceLinkHarnessReserve is newSpliceLinkHarnessWith with each side
// held to reserve.
func newSpliceLinkHarnessReserve(t *testing.T, reserve btcutil.Amount,
	tweak func(*ChannelLinkConfig)) *spliceLinkHarness {

	t.Helper()

	h, err := newSingleLinkTestHarness(
		t, 5*btcutil.SatoshiPerBitcoin, reserve,
	)
	require.NoError(t, err)

	link := h.aliceLink.(*channelLink)
	s := &spliceLinkHarness{
		link:     link,
		bob:      h.bobChannel,
		handler:  &recordingSpliceHandler{},
		failures: make(chan LinkFailureError, 1),
		sent:     link.cfg.Peer.(*mockPeer).sentMsgs,
	}
	link.cfg.SpliceHandler = s.handler
	link.cfg.OnChannelFailure = func(_ lnwire.ChannelID,
		_ lnwire.ShortChannelID, e LinkFailureError) {

		select {
		case s.failures <- e:
		default:
		}
	}
	if tweak != nil {
		tweak(&link.cfg)
	}
	require.NoError(t, h.start())

	return s
}

// requireFailure waits for the link to fail with the given action.
func (s *spliceLinkHarness) requireFailure(t *testing.T,
	action LinkFailureAction) {

	t.Helper()

	select {
	case e := <-s.failures:
		require.Equal(t, action, e.FailureAction, e.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("link didn't fail")
	}
}

// requireNoFailure checks the link hasn't failed.
func (s *spliceLinkHarness) requireNoFailure(t *testing.T) {
	t.Helper()

	select {
	case e := <-s.failures:
		t.Fatalf("link failed: %v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

// nextSent returns the next message of type T the link sends.
func nextSent[T lnwire.Message](t *testing.T, s *spliceLinkHarness) T {
	t.Helper()

	timeout := time.After(5 * time.Second)
	for {
		select {
		case msg := <-s.sent:
			if m, ok := msg.(T); ok {
				return m
			}
		case <-timeout:
			var zero T
			t.Fatalf("link sent no %T", zero)
			return zero
		}
	}
}

// TestLinkStartBatchRefused checks start_batch handling: one above 20
// messages or with another message inside is a protocol violation that ends
// the connection; one of a single message, or for another message type, is
// ignored.
func TestLinkStartBatchRefused(t *testing.T) {
	t.Parallel()

	commitType := fn.Some(lnwire.MessageType(lnwire.MsgCommitSig))

	t.Run("too big", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		s.link.HandleChannelUpdate(&lnwire.StartBatch{
			ChannelID: s.link.ChanID(), BatchSize: 21,
			BatchMessageType: commitType,
		})
		s.requireFailure(t, LinkFailureDisconnect)
	})

	t.Run("other message inside", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		s.link.HandleChannelUpdate(&lnwire.StartBatch{
			ChannelID: s.link.ChanID(), BatchSize: 2,
			BatchMessageType: commitType,
		})
		s.link.HandleChannelUpdate(&lnwire.UpdateFee{
			ChanID: s.link.ChanID(), FeePerKw: 1000,
		})
		s.requireFailure(t, LinkFailureDisconnect)
	})

	t.Run("ignored", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		s.link.HandleChannelUpdate(&lnwire.StartBatch{
			ChannelID: s.link.ChanID(), BatchSize: 1,
			BatchMessageType: commitType,
		})
		s.link.HandleChannelUpdate(&lnwire.StartBatch{
			ChannelID: s.link.ChanID(), BatchSize: 3,
			BatchMessageType: fn.Some(
				lnwire.MessageType(lnwire.MsgUpdateFee),
			),
		})
		s.link.HandleChannelUpdate(&lnwire.StartBatch{
			ChannelID: s.link.ChanID(), BatchSize: 3,
		})
		s.requireNoFailure(t)
		require.Nil(t, s.link.batch)
	})
}

// bobBatch has Bob add an HTLC and sign, returning his batch: the main
// commitment_signed and one per pending splice.
func bobBatch(t *testing.T, s *spliceLinkHarness) (*lnwire.CommitSig,
	[]*lnwire.CommitSig) {

	t.Helper()

	amt := lnwire.NewMSatFromSatoshis(100_000)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	_, err = s.bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	s.link.HandleChannelUpdate(htlc)

	commit, err := s.bob.SignNextCommitment(t.Context())
	require.NoError(t, err)

	main := &lnwire.CommitSig{
		ChanID:    s.link.ChanID(),
		CommitSig: commit.CommitSig,
		HtlcSigs:  commit.HtlcSigs,
		FundingTxid: fn.Some(
			s.bob.State().CurrentFundingOutpoint().Hash,
		),
	}
	var members []*lnwire.CommitSig
	for _, m := range commit.SpliceSigs {
		members = append(members, &lnwire.CommitSig{
			ChanID:      s.link.ChanID(),
			CommitSig:   m.CommitSig,
			HtlcSigs:    m.HtlcSigs,
			FundingTxid: fn.Some(m.FundingTxid),
		})
	}

	return main, members
}

// sendBatch hands the link a start_batch and its members.
func sendBatch(s *spliceLinkHarness, sigs ...*lnwire.CommitSig) {
	s.link.HandleChannelUpdate(&lnwire.StartBatch{
		ChannelID: s.link.ChanID(), BatchSize: uint16(len(sigs)),
		BatchMessageType: fn.Some(
			lnwire.MessageType(lnwire.MsgCommitSig),
		),
	})
	for _, sig := range sigs {
		s.link.HandleChannelUpdate(sig)
	}
}

// TestLinkBatchRefused checks batches that don't cover every funding. One
// for fewer splices than ours, the two sides disagreeing on the set (one
// dropped a splice before the other), drops the connection with a warning,
// nothing having been accepted; the others fail the channel, as any invalid
// commitment does.
func TestLinkBatchRefused(t *testing.T) {
	t.Parallel()

	t.Run("main alone while a splice is pending", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		pendSplice(t, s.link.channel, s.bob, 50_000_000)
		main, _ := bobBatch(t, s)
		s.link.HandleChannelUpdate(main)
		s.requireFailure(t, LinkFailureDisconnect)
	})

	// The peer left out a splice it disowned, the channel being closed
	// for it: an update of the peer's no close could get past.
	t.Run("main alone, a disowned splice", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarnessWith(t, func(cfg *ChannelLinkConfig) {
			h := cfg.SpliceHandler.(*recordingSpliceHandler)
			h.disowned, h.closing = true, true
		})
		pendSplice(t, s.link.channel, s.bob, 50_000_000)
		main, _ := bobBatch(t, s)
		s.link.HandleChannelUpdate(main)
		s.requireFailure(t, LinkFailureForceClose)
	})

	// With an earlier transaction live, which locks in time: dropped,
	// not closed.
	t.Run("main alone, a disowned replacement", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarnessWith(t, func(cfg *ChannelLinkConfig) {
			cfg.SpliceHandler.(*recordingSpliceHandler).disowned =
				true
		})
		pendSplice(t, s.link.channel, s.bob, 50_000_000)
		main, _ := bobBatch(t, s)
		s.link.HandleChannelUpdate(main)
		s.requireFailure(t, LinkFailureDisconnect)
	})

	t.Run("splice member twice", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		pendSplice(t, s.link.channel, s.bob, 50_000_000)
		_, members := bobBatch(t, s)
		sendBatch(s, members[0], members[0])
		s.requireFailure(t, LinkFailureForceClose)
	})

	t.Run("unknown funding", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		pendSplice(t, s.link.channel, s.bob, 50_000_000)
		main, members := bobBatch(t, s)
		unknown := *members[0]
		unknown.FundingTxid = fn.Some(chainhash.Hash{0xbb})
		sendBatch(s, main, members[0], &unknown)
		s.requireFailure(t, LinkFailureForceClose)
	})

	t.Run("member without funding_txid", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		pendSplice(t, s.link.channel, s.bob, 50_000_000)
		main, members := bobBatch(t, s)
		bare := *members[0]
		bare.FundingTxid = fn.None[chainhash.Hash]()
		sendBatch(s, main, &bare)
		s.requireFailure(t, LinkFailureForceClose)
	})
}

// TestLinkBatchAccepted checks a good batch, in any order, and one carrying
// a member for a splice already dropped, which is ignored: the link answers
// with a revocation.
func TestLinkBatchAccepted(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)

	// A second splice, which Alice then drops: Bob still signs for it.
	second := chainhash.Hash{0x5e, 0x22}
	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	s.link.channel.Signer.(*input.MockSigner).Privkeys = append(
		s.link.channel.Signer.(*input.MockSigner).Privkeys, aliceKey,
	)
	s.bob.Signer.(*input.MockSigner).Privkeys = append(
		s.bob.Signer.(*input.MockSigner).Privkeys, bobKey,
	)
	capacity := s.bob.State().Capacity + 1_000
	op := wire.OutPoint{Hash: second}
	require.NoError(t, s.link.channel.AddPendingFunding(
		lnwallet.PendingFunding{
			OutPoint: op, Capacity: capacity,
			LocalKey: keychain.KeyDescriptor{
				PubKey: aliceKey.PubKey(),
			},
			RemoteKey: bobKey.PubKey(), RemoteDelta: 1_000,
		},
	))
	require.NoError(t, s.bob.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:  keychain.KeyDescriptor{PubKey: bobKey.PubKey()},
		RemoteKey: aliceKey.PubKey(), LocalDelta: 1_000,
	}))
	sigs, err := s.link.channel.SignSpliceCommitment(t.Context(), second)
	require.NoError(t, err)
	require.NoError(t, s.bob.ReceiveSpliceCommitment(second, sigs))
	sigs, err = s.bob.SignSpliceCommitment(t.Context(), second)
	require.NoError(t, err)
	require.NoError(t, s.link.channel.ReceiveSpliceCommitment(second, sigs))
	require.NoError(t, s.link.channel.RemovePendingFunding(second))

	main, members := bobBatch(t, s)
	require.Len(t, members, 2)

	// Members out of order, the dropped one included.
	sendBatch(s, members[1], main, members[0])
	nextSent[*lnwire.RevokeAndAck](t, s)
	s.requireNoFailure(t)

	tx, err := s.link.channel.SignedSpliceCommitment(txid)
	require.NoError(t, err)
	require.Equal(t, txid, tx.TxIn[0].PreviousOutPoint.Hash)
}

// TestLinkSpliceCommitSig checks the splice's own commitment_signed while
// the channel is quiescent: checked, recorded and reported to the splice
// machine, not answered with a revocation; and the link sending its own on
// request. Splice messages reach the machine.
func TestLinkSpliceCommitSig(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)

	// Quiesce: Alice asks, Bob answers.
	res := s.link.InitStfu()
	nextSent[*lnwire.Stfu](t, s)
	s.link.HandleChannelUpdate(&lnwire.Stfu{
		ChanID: s.link.ChanID(), Initiator: false,
	})
	select {
	case r := <-res:
		require.NoError(t, r.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("no quiescence")
	}

	// Both add the splice, as their machines would after tx_complete.
	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	s.link.channel.Signer.(*input.MockSigner).Privkeys = append(
		s.link.channel.Signer.(*input.MockSigner).Privkeys, aliceKey,
	)
	s.bob.Signer.(*input.MockSigner).Privkeys = append(
		s.bob.Signer.(*input.MockSigner).Privkeys, bobKey,
	)
	txid := chainhash.Hash{0x5e, 0x33}
	op := wire.OutPoint{Hash: txid}
	capacity := s.bob.State().Capacity + 20_000_000
	require.NoError(t, <-s.link.AddSpliceFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:   keychain.KeyDescriptor{PubKey: aliceKey.PubKey()},
		RemoteKey:  bobKey.PubKey(),
		LocalDelta: 20_000_000,
	}))
	require.NoError(t, s.bob.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:    keychain.KeyDescriptor{PubKey: bobKey.PubKey()},
		RemoteKey:   aliceKey.PubKey(),
		RemoteDelta: 20_000_000,
	}))

	// Splice messages go to the machine.
	s.link.HandleChannelUpdate(&lnwire.TxComplete{
		ChannelID: s.link.ChanID(),
	})

	// Alice's commitment_signed, on request.
	require.NoError(t, <-s.link.SendSpliceCommitment(txid))
	sent := nextSent[*lnwire.CommitSig](t, s)
	require.Equal(t, fn.Some(txid), sent.FundingTxid)
	require.NoError(t, s.bob.ReceiveSpliceCommitment(
		txid, &lnwallet.CommitSigs{
			CommitSig: sent.CommitSig, HtlcSigs: sent.HtlcSigs,
		},
	))

	// Bob's: checked and reported, no revocation.
	sigs, err := s.bob.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	s.link.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID: s.link.ChanID(), CommitSig: sigs.CommitSig,
		HtlcSigs: sigs.HtlcSigs, FundingTxid: fn.Some(txid),
	})

	require.Eventually(t, func() bool {
		s.handler.mu.Lock()
		defer s.handler.mu.Unlock()

		return len(s.handler.commits) == 1 &&
			len(s.handler.messages) == 1
	}, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, s.handler.commits[0])
	require.IsType(t, &lnwire.TxComplete{}, s.handler.messages[0])

	_, err = s.link.channel.SignedSpliceCommitment(txid)
	require.NoError(t, err)

	select {
	case msg := <-s.sent:
		t.Fatalf("link answered with %T", msg)
	case <-time.After(200 * time.Millisecond):
	}
	s.requireNoFailure(t)

	// A forged one is reported as refused.
	s.link.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID: s.link.ChanID(), CommitSig: sent.CommitSig,
		HtlcSigs: sigs.HtlcSigs, FundingTxid: fn.Some(txid),
	})
	require.Eventually(t, func() bool {
		s.handler.mu.Lock()
		defer s.handler.mu.Unlock()

		return len(s.handler.commits) == 2
	}, 5*time.Second, 20*time.Millisecond)
	require.Error(t, s.handler.commits[1])
}

// TestLinkReestablishSplice checks reconnecting in the middle of a splice:
// Bob's commitment_signed for it was lost, so Alice's next_funding asks for
// it again. Bob's link resends it, Alice's link records it, each machine gets
// the other's channel_reestablish with its splice fields, channel_ready isn't
// resent because a splice is named, and the channel then updates in batches.
func TestLinkReestablishSplice(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)

	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	as := alice.channel.Signer.(*input.MockSigner)
	as.Privkeys = append(as.Privkeys, aliceKey)
	bs := bob.channel.Signer.(*input.MockSigner)
	bs.Privkeys = append(bs.Privkeys, bobKey)

	const added = btcutil.Amount(30_000_000)
	txid := chainhash.Hash{0x5e, 0x44}
	op := wire.OutPoint{Hash: txid}
	capacity := alice.channel.State().Capacity + added
	require.NoError(t, alice.channel.AddPendingFunding(
		lnwallet.PendingFunding{
			OutPoint: op, Capacity: capacity,
			LocalKey: keychain.KeyDescriptor{
				PubKey: aliceKey.PubKey(),
			},
			RemoteKey: bobKey.PubKey(), LocalDelta: added,
		},
	))
	require.NoError(t, bob.channel.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:    keychain.KeyDescriptor{PubKey: bobKey.PubKey()},
		RemoteKey:   aliceKey.PubKey(),
		RemoteDelta: added,
	}))

	// Alice's reached Bob; Bob's was signed and sent, then lost.
	sigs, err := alice.channel.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	require.NoError(t, bob.channel.ReceiveSpliceCommitment(txid, sigs))
	_, err = bob.channel.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)

	original := alice.channel.State().FundingOutpoint.Hash
	aliceHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{
			Txid:            txid,
			RetransmitFlags: lnwire.RetransmitCommitSig,
		}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}
	bobHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: txid}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}
	n := newTwoHopNetwork(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) { c.SpliceHandler = aliceHandler },
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)

	// Alice gets Bob's commitment_signed for the splice.
	require.Eventually(t, func() bool {
		aliceHandler.mu.Lock()
		defer aliceHandler.mu.Unlock()

		return len(aliceHandler.commits) == 1
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, aliceHandler.commits[0])

	// Each machine saw the other's splice fields.
	for _, h := range []*recordingSpliceHandler{aliceHandler, bobHandler} {
		h.mu.Lock()
		require.Len(t, h.reestablish, 1)
		got := h.reestablish[0]
		h.mu.Unlock()

		require.Equal(t, txid,
			got.NextFunding.UnsafeFromSome().Txid)
		require.Equal(t, original,
			got.MyCurrentFundingLocked.UnsafeFromSome().Txid)
	}

	// No channel_ready resent: a splice was named.
	for _, s := range []*mockServer{n.aliceServer, n.bobServer} {
		s.protocolTraceMtx.Lock()
		for _, m := range s.protocolTrace {
			require.NotEqual(t,
				lnwire.MessageType(lnwire.MsgChannelReady),
				m.MsgType())
		}
		s.protocolTraceMtx.Unlock()
	}

	// The channel carries on, in batches.
	pay(t, n, lnwire.NewMSatFromSatoshis(2_000_000))
	waitClean(t, n)
	for _, link := range []*channelLink{
		n.aliceChannelLink, n.bobChannelLink,
	} {
		_, err := link.channel.SignedSpliceCommitment(txid)
		require.NoError(t, err)
	}
}

// TestLinkReestablishSigningQuiescent checks reconnecting while a splice is
// still being signed: both links are quiescent again, as BOLT 2 keeps the
// channel until both tx_signatures are exchanged and the peer assumes,
// rather than sending an update the peer refuses, or signing a commitment
// the splice's unexchanged one makes fail; the machines end it.
func TestLinkReestablishSigningQuiescent(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)

	aliceKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	as := alice.channel.Signer.(*input.MockSigner)
	as.Privkeys = append(as.Privkeys, aliceKey)
	bs := bob.channel.Signer.(*input.MockSigner)
	bs.Privkeys = append(bs.Privkeys, bobKey)

	const added = btcutil.Amount(30_000_000)
	txid := chainhash.Hash{0x5e, 0x44}
	op := wire.OutPoint{Hash: txid}
	capacity := alice.channel.State().Capacity + added
	require.NoError(t, alice.channel.AddPendingFunding(
		lnwallet.PendingFunding{
			OutPoint: op, Capacity: capacity,
			LocalKey: keychain.KeyDescriptor{
				PubKey: aliceKey.PubKey(),
			},
			RemoteKey: bobKey.PubKey(), LocalDelta: added,
		},
	))
	require.NoError(t, bob.channel.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint: op, Capacity: capacity,
		LocalKey:    keychain.KeyDescriptor{PubKey: bobKey.PubKey()},
		RemoteKey:   aliceKey.PubKey(),
		RemoteDelta: added,
	}))

	// Alice's reached Bob; Bob's was signed and sent, then lost.
	sigs, err := alice.channel.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)
	require.NoError(t, bob.channel.ReceiveSpliceCommitment(txid, sigs))
	_, err = bob.channel.SignSpliceCommitment(t.Context(), txid)
	require.NoError(t, err)

	original := alice.channel.State().FundingOutpoint.Hash
	aliceHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{
			Txid:            txid,
			RetransmitFlags: lnwire.RetransmitCommitSig,
		}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}
	bobHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: txid}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}

	// Both machines still signing the splice.
	aliceHandler.signing = true
	bobHandler.signing = true
	n := newTwoHopNetworkStopped(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) { c.SpliceHandler = aliceHandler },
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)
	require.NoError(t, n.aliceServer.Start())
	require.NoError(t, n.bobServer.Start())

	// Alice gets Bob's commitment_signed for the splice.
	require.Eventually(t, func() bool {
		aliceHandler.mu.Lock()
		defer aliceHandler.mu.Unlock()

		return len(aliceHandler.commits) == 1
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, aliceHandler.commits[0])

	// Quiescent again on both sides, as the peer assumes: no update
	// before the tx_signatures are exchanged.
	for _, link := range []*channelLink{
		n.aliceChannelLink, n.bobChannelLink,
	} {
		require.Eventually(t, link.quiescer.IsQuiescent,
			5*time.Second, 20*time.Millisecond)
		require.False(t, link.quiescer.CanSendUpdates())
	}

	// The machines, done signing, end it: the channel carries on.
	for _, link := range []*channelLink{
		n.aliceChannelLink, n.bobChannelLink,
	} {
		require.NoError(t, <-link.ResumeQuiescence())
	}
	pay(t, n, lnwire.NewMSatFromSatoshis(2_000_000))
	waitClean(t, n)
}

// TestLinkNoSyncStillReestablishes checks a link of a channel just opened,
// which has no channel_reestablish to exchange, tells the splice machine it
// may send all the same: without it a splice's splice_locked would wait for
// a reestablish that never comes.
func TestLinkNoSyncStillReestablishes(t *testing.T) {
	t.Parallel()

	h, err := newSingleLinkTestHarness(t, btcutil.SatoshiPerBitcoin, 0)
	require.NoError(t, err)
	link := h.aliceLink.(*channelLink)
	handler := &recordingSpliceHandler{}
	link.cfg.SpliceHandler = handler
	link.cfg.SyncStates = false
	require.NoError(t, h.start())

	require.Eventually(t, func() bool {
		handler.mu.Lock()
		defer handler.mu.Unlock()

		return len(handler.reestablish) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

// TestLinkPeerInputHold checks that an HTLC from the peer drawing on coins
// it put into a splice not yet deep enough is failed back with
// temporary_channel_failure, and that one leaving them alone goes through.
func TestLinkPeerInputHold(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)

	bobHandler := &recordingSpliceHandler{}
	n := newTwoHopNetwork(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) {
			c.SpliceHandler = &recordingSpliceHandler{}
		},
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)

	// Everything Alice has is held: her payment to Bob is failed back.
	aliceBalance := bob.channel.StateSnapshot().RemoteBalance.ToSatoshis()
	bobHandler.mu.Lock()
	bobHandler.held = aliceBalance
	bobHandler.mu.Unlock()

	amount := lnwire.NewMSatFromSatoshis(10_000_000)
	htlcAmt, totalTimelock, hops := generateHops(
		amount, testStartingHeight, n.bobChannelLink,
	)
	_, err = makePayment(
		n.aliceServer, n.bobServer, n.bobChannelLink.ShortChanID(),
		hops, amount, htlcAmt, totalTimelock,
	).Wait(30 * time.Second)
	require.Error(t, err)
	var clearErr ClearTextError
	require.ErrorAs(t, err, &clearErr)
	require.IsType(t, &lnwire.FailTemporaryChannelFailure{},
		clearErr.WireMessage())
	waitClean(t, n)
	require.Equal(t, aliceBalance,
		bob.channel.StateSnapshot().RemoteBalance.ToSatoshis())

	// A third held: the same payment leaves it alone and goes through.
	bobHandler.mu.Lock()
	bobHandler.held = aliceBalance / 3
	bobHandler.mu.Unlock()
	pay(t, n, amount)
	waitClean(t, n)
}

// TestLinkBatchAcrossLock checks a batch the peer signed before it learned
// that the splice locked: its member for the funding the lock replaced is
// ignored, the one for the splice's funding, now the channel's, is the
// commitment, and the link revokes as usual.
func TestLinkBatchAcrossLock(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	old := s.link.channel.State().CurrentFundingOutpoint().Hash
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)

	main, members := bobBatch(t, s)
	require.Len(t, members, 1)
	require.Equal(t, fn.Some(old), main.FundingTxid)

	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-s.link.LockSplice(txid, scid))
	require.Equal(t, txid,
		s.link.channel.State().CurrentFundingOutpoint().Hash)

	sendBatch(s, main, members[0])
	nextSent[*lnwire.RevokeAndAck](t, s)
	s.requireNoFailure(t)
}

// TestLinkHoldUpdates checks the hold on our updates for a splice the peer
// disowned, whose commitments leave it out: no HTLC is forwarded into the
// channel nor an update_fee sent, until a splice locks.
func TestLinkHoldUpdates(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarnessWith(t, func(cfg *ChannelLinkConfig) {
		cfg.FeeEstimator = chainfee.NewStaticEstimator(50_000, 253)
	})
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)
	require.Eventually(t, s.link.EligibleToForward, 5*time.Second,
		10*time.Millisecond)

	updateFee := func() {
		t.Helper()

		require.NoError(t, <-s.link.runSpliceReq(
			func(ctx context.Context) error {
				return s.link.handleUpdateFee(ctx)
			},
		))
	}

	// Twice: the second changes nothing.
	require.NoError(t, <-s.link.HoldUpdates())
	require.NoError(t, <-s.link.HoldUpdates())
	require.False(t, s.link.EligibleToForward())
	updateFee()
	timeout := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case msg := <-s.sent:
			_, fee := msg.(*lnwire.UpdateFee)
			require.False(t, fee, "update_fee while held")
		case <-timeout:
			done = true
		}
	}

	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-s.link.LockSplice(txid, scid))
	require.True(t, s.link.EligibleToForward())
	updateFee()
	nextSent[*lnwire.UpdateFee](t, s)
}

// TestLinkHoldFromStart checks a link started with a splice the peer
// disowned, restored by its machine: our updates are held before the link
// can forward any, until a splice locks.
func TestLinkHoldFromStart(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarnessWith(t, func(cfg *ChannelLinkConfig) {
		cfg.SpliceHandler.(*recordingSpliceHandler).disowned = true
	})
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)
	require.Never(t, s.link.EligibleToForward, 300*time.Millisecond,
		10*time.Millisecond)

	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-s.link.LockSplice(txid, scid))
	require.True(t, s.link.EligibleToForward())
}

// TestLinkHoldKeptWhileClosing checks a lock on a channel being closed for a
// splice the peer disowned, the disowned one itself locking: the hold stays,
// our shutdown possibly out.
func TestLinkHoldKeptWhileClosing(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarnessWith(t, func(cfg *ChannelLinkConfig) {
		h := cfg.SpliceHandler.(*recordingSpliceHandler)
		h.disowned, h.closing = true, true
	})
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)
	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-s.link.LockSplice(txid, scid))
	require.True(t, s.link.IsFlushing(Outgoing))
	require.False(t, s.link.EligibleToForward())
}

// TestLinkHoldKeepsCloseDisable checks the hold's release leaves our adds
// disabled when a close disabled them before the hold.
func TestLinkHoldKeepsCloseDisable(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)
	require.Eventually(t, s.link.EligibleToForward, 5*time.Second,
		10*time.Millisecond)

	require.True(t, s.link.DisableAdds(Outgoing))
	require.NoError(t, <-s.link.HoldUpdates())
	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-s.link.LockSplice(txid, scid))
	require.True(t, s.link.IsFlushing(Outgoing))
	require.False(t, s.link.EligibleToForward())
}

// TestLinkTxAbortKeepsReserves checks a tx_abort leaves a spliced channel's
// reserves as they were: they change at the next connection, when the Core
// Lightning port takes them up too (bolts#15). Switching at the tx_abort
// left the two disagreeing after an aborted RBF, and the channel force
// closed.
func TestLinkTxAbortKeepsReserves(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)
	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, <-s.link.LockSplice(txid, scid))

	reserves := func() (btcutil.Amount, btcutil.Amount) {
		var local, remote btcutil.Amount
		require.NoError(t, <-s.link.runSpliceReq(
			func(context.Context) error {
				state := s.link.channel.State()
				local = state.LocalChanCfg.ChanReserve
				remote = state.RemoteChanCfg.ChanReserve

				return nil
			},
		))

		return local, remote
	}
	splice, ok := s.link.channel.State().SpliceReserve()
	require.True(t, ok)
	local, remote := reserves()
	require.NotEqual(t, splice, local, "the lock alone changes them")

	s.link.HandleChannelUpdate(&lnwire.TxAbort{ChannelID: s.link.ChanID()})
	// The abort is handled on the link's goroutine, which hands it to the
	// splice handler. Once the handler has it, the link is handling the
	// abort, and a request queued now runs once it is done.
	require.Eventually(t, func() bool {
		s.handler.mu.Lock()
		defer s.handler.mu.Unlock()

		for _, m := range s.handler.messages {
			if _, ok := m.(*lnwire.TxAbort); ok {
				return true
			}
		}

		return false
	}, 5*time.Second, 10*time.Millisecond)
	gotLocal, gotRemote := reserves()
	require.Equal(t, local, gotLocal)
	require.Equal(t, remote, gotRemote)
}

// quiesce makes the harness's link quiescent, Alice asking.
func quiesce(t *testing.T, s *spliceLinkHarness) {
	t.Helper()

	res := s.link.InitStfu()
	nextSent[*lnwire.Stfu](t, s)
	s.link.HandleChannelUpdate(&lnwire.Stfu{
		ChanID: s.link.ChanID(), Initiator: false,
	})
	select {
	case r := <-res:
		require.NoError(t, r.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("no quiescence")
	}
}

// TestLinkSpliceCommitSigEarly checks the peer's commitment_signed for a
// splice reaching the link before our machine has added the splice to the
// channel's fundings: it waits for that, then is the splice's own, rather
// than taken for a commitment on the current funding, which failed its
// check and force closed the channel. With no splice being built, one
// naming an unknown funding is ignored.
func TestLinkSpliceCommitSigEarly(t *testing.T) {
	t.Parallel()

	for _, building := range []bool{true, false} {
		s := newSpliceLinkHarness(t)
		quiesce(t, s)
		s.handler.mu.Lock()
		s.handler.constructing = building
		s.handler.mu.Unlock()

		aliceKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		bobKey, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		s.link.channel.Signer.(*input.MockSigner).Privkeys = append(
			s.link.channel.Signer.(*input.MockSigner).Privkeys,
			aliceKey,
		)
		s.bob.Signer.(*input.MockSigner).Privkeys = append(
			s.bob.Signer.(*input.MockSigner).Privkeys, bobKey,
		)
		txid := chainhash.Hash{0x5e, 0x44}
		op := wire.OutPoint{Hash: txid}
		capacity := s.bob.State().Capacity + 20_000_000
		require.NoError(t, s.bob.AddPendingFunding(
			lnwallet.PendingFunding{
				OutPoint: op, Capacity: capacity,
				LocalKey: keychain.KeyDescriptor{
					PubKey: bobKey.PubKey(),
				},
				RemoteKey:   aliceKey.PubKey(),
				RemoteDelta: 20_000_000,
			},
		))

		// Bob's, ahead of our machine.
		sigs, err := s.bob.SignSpliceCommitment(t.Context(), txid)
		require.NoError(t, err)
		s.link.HandleChannelUpdate(&lnwire.CommitSig{
			ChanID: s.link.ChanID(), CommitSig: sigs.CommitSig,
			HtlcSigs: sigs.HtlcSigs, FundingTxid: fn.Some(txid),
		})
		time.Sleep(100 * time.Millisecond)
		s.requireNoFailure(t)

		reported := func() int {
			s.handler.mu.Lock()
			defer s.handler.mu.Unlock()

			return len(s.handler.commits)
		}
		require.Zero(t, reported(), "building %v", building)
		if !building {
			continue
		}

		// The machine adds it: the commitment_signed is taken up.
		require.NoError(t, <-s.link.AddSpliceFunding(
			lnwallet.PendingFunding{
				OutPoint: op, Capacity: capacity,
				LocalKey: keychain.KeyDescriptor{
					PubKey: aliceKey.PubKey(),
				},
				RemoteKey:  bobKey.PubKey(),
				LocalDelta: 20_000_000,
			},
		))
		require.Eventually(t, func() bool {
			return reported() == 1
		}, 5*time.Second, 20*time.Millisecond)
		s.handler.mu.Lock()
		require.NoError(t, s.handler.commits[0])
		s.handler.mu.Unlock()
		s.requireNoFailure(t)
	}
}

// TestLinkUpdateAfterTxSignatures checks an update the peer sends once it
// ended quiescence on the exchange of tx_signatures, before our splice
// machine has: it waits for the end of quiescence, rather than failing the
// link, and is then taken up.
func TestLinkUpdateAfterTxSignatures(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	quiesce(t, s)
	s.link.HandleChannelUpdate(&lnwire.TxSignatures{
		ChannelID: s.link.ChanID(),
	})

	amt := lnwire.NewMSatFromSatoshis(100_000)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	_, err = s.bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	s.link.HandleChannelUpdate(htlc)
	commit, err := s.bob.SignNextCommitment(t.Context())
	require.NoError(t, err)
	s.link.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID: s.link.ChanID(), CommitSig: commit.CommitSig,
		HtlcSigs: commit.HtlcSigs,
	})
	time.Sleep(100 * time.Millisecond)
	s.requireNoFailure(t)

	require.NoError(t, <-s.link.ResumeQuiescence())
	nextSent[*lnwire.RevokeAndAck](t, s)
	s.requireNoFailure(t)
}

// TestLinkCommitSigAfterPeerLock checks the peer's first commitment after a
// splice locked on its side, naming the splice's funding alone, reaching the
// link before our own lock has: it waits for our lock, rather than being
// taken for the splice's own commitment_signed and left unanswered.
func TestLinkCommitSigAfterPeerLock(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	txid := pendSplice(t, s.link.channel, s.bob, 50_000_000)
	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, s.bob.LockPendingFunding(txid, scid))

	s.link.HandleChannelUpdate(&lnwire.SpliceLocked{
		ChannelID: s.link.ChanID(), SpliceTxid: txid,
	})
	amt := lnwire.NewMSatFromSatoshis(100_000)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	_, err = s.bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	s.link.HandleChannelUpdate(htlc)
	commit, err := s.bob.SignNextCommitment(t.Context())
	require.NoError(t, err)
	require.Empty(t, commit.SpliceSigs)
	s.link.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID: s.link.ChanID(), CommitSig: commit.CommitSig,
		HtlcSigs: commit.HtlcSigs, FundingTxid: fn.Some(txid),
	})
	time.Sleep(100 * time.Millisecond)
	s.requireNoFailure(t)

	require.NoError(t, <-s.link.LockSplice(txid, scid))
	nextSent[*lnwire.RevokeAndAck](t, s)
	s.requireNoFailure(t)
}

// TestLinkHeldBounded checks the peer's messages held for a splice step are
// bounded: the step not coming in time, or too many or too large held, the
// connection drops, rather than the link holding them for good.
func TestLinkHeldBounded(t *testing.T) {
	t.Parallel()

	// heldUpdate has the peer send an update the link holds: quiescence
	// ended on its side with tx_signatures, not yet on ours.
	heldUpdate := func(s *spliceLinkHarness) *lnwire.UpdateAddHTLC {
		quiesce(t, s)
		s.link.HandleChannelUpdate(&lnwire.TxSignatures{
			ChannelID: s.link.ChanID(),
		})
		amt := lnwire.NewMSatFromSatoshis(100_000)
		var blob [lnwire.OnionPacketSize]byte
		_, htlc, _, err := generatePayment(amt, amt, 5, blob)
		require.NoError(t, err)
		_, err = s.bob.AddHTLC(htlc, nil)
		require.NoError(t, err)
		s.link.HandleChannelUpdate(htlc)

		return htlc
	}

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarnessWith(t, func(c *ChannelLinkConfig) {
			c.HeldMessageTimeout = 300 * time.Millisecond
		})
		heldUpdate(s)
		s.requireFailure(t, LinkFailureDisconnect)
	})

	t.Run("too many", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		peer := s.link.cfg.Peer.(*mockPeer)
		peer.disconnects = make(chan error, 16)
		htlc := heldUpdate(s)
		for range maxHeldMessages {
			s.link.HandleChannelUpdate(htlc)
		}

		// The mailbox holds as many messages as the link does, so when
		// the link drains it more slowly than they come, the mailbox's
		// bound disconnects the peer first. Either way the connection
		// drops. Other disconnects (the harness never ends quiescence)
		// are what a no-op Disconnect ignored before.
		timeout := time.After(5 * time.Second)
		for {
			select {
			case e := <-s.failures:
				require.Equal(t, LinkFailureDisconnect,
					e.FailureAction, e.Error())

				return

			case err := <-peer.disconnects:
				if errors.Is(err, errWireMessageQueueFull) {
					return
				}

			case <-timeout:
				t.Fatal("neither the link nor the mailbox " +
					"dropped the connection")
			}
		}
	})

	// Few but large, each handed over before the next: the mailbox stays
	// near empty, and the link's byte bound is what drops the peer.
	t.Run("too large", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		htlc := heldUpdate(s)

		// An odd record pads each to about 61 KB.
		const pad = 60_000
		big := *htlc
		big.ExtraData = append(
			lnwire.ExtraOpaqueData{101, 0xfd, pad >> 8, pad & 0xff},
			make([]byte, pad)...,
		)
		size, err := wireMessageSize(&big)
		require.NoError(t, err)
		require.Greater(t, size, uint32(pad))

		mb, ok := s.link.mailBox.(*memoryMailBox)
		require.True(t, ok)
		drained := func() bool {
			mb.wireCond.L.Lock()
			defer mb.wireCond.L.Unlock()

			return mb.wireMessages.Len() == 0
		}
		for range maxWireBytes/size + 2 {
			s.link.HandleChannelUpdate(&big)
			require.Eventually(t, drained, 5*time.Second,
				time.Millisecond)
		}
		s.requireFailure(t, LinkFailureDisconnect)
	})

	// Below both, nothing fails.
	t.Run("within", func(t *testing.T) {
		t.Parallel()

		s := newSpliceLinkHarness(t)
		htlc := heldUpdate(s)
		for range maxHeldMessages - 2 {
			s.link.HandleChannelUpdate(htlc)
		}
		s.requireNoFailure(t)
	})
}

// TestLinkReestablishPeerDoneSigning checks a reconnection on which we are
// still signing a splice and the peer is done (its reestablish names no
// splice: it has both tx_signatures, ours reached it and its own were lost).
// It ends quiescence and sends its owed updates, which reach us before its
// tx_signatures sent again: they wait for our machine to end quiescence,
// rather than disconnecting for an update after stfu, on every reconnection
// until the splice locked.
func TestLinkReestablishPeerDoneSigning(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)

	const added = btcutil.SatoshiPerBitcoin
	txid := pendSplice(t, alice.channel, bob.channel, added)
	original := alice.channel.State().FundingOutpoint.Hash

	aliceHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: txid}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
		signing: true,
	}
	bobHandler := &recordingSpliceHandler{
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}
	failures := make(chan LinkFailureError, 1)
	n := newTwoHopNetworkStopped(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) {
			c.SpliceHandler = aliceHandler
			c.OnChannelFailure = func(_ lnwire.ChannelID,
				_ lnwire.ShortChannelID, e LinkFailureError) {

				select {
				case failures <- e:
				default:
				}
			}
		},
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)
	require.NoError(t, n.aliceServer.Start())
	require.NoError(t, n.bobServer.Start())
	require.Eventually(t, n.aliceChannelLink.quiescer.IsQuiescent,
		5*time.Second, 20*time.Millisecond)
	require.Eventually(t, n.bobChannelLink.EligibleToForward,
		5*time.Second, 20*time.Millisecond)

	// Bob pays Alice at once.
	amount := lnwire.NewMSatFromSatoshis(2_000_000)
	htlcAmt, totalTimelock, hops := generateHops(
		amount, testStartingHeight, n.aliceChannelLink,
	)
	paid := makePayment(
		n.bobServer, n.aliceServer, n.aliceChannelLink.ShortChanID(),
		hops, amount, htlcAmt, totalTimelock,
	)
	select {
	case e := <-failures:
		t.Fatalf("Alice's link failed: %v", e)
	case <-time.After(time.Second):
	}

	// Bob's tx_signatures in, Alice's machine ends quiescence: the
	// payment goes through.
	require.NoError(t, <-n.aliceChannelLink.ResumeQuiescence())
	_, err = paid.Wait(30 * time.Second)
	require.NoError(t, err)
}

// TestLinkAddBelowSpliceReserve checks an add the peer can afford on the
// current funding but not on a pending splice's, where it moved funds out:
// refused with a warning and a disconnection, as a commitment would be,
// rather than an error a peer fails the channel on.
func TestLinkAddBelowSpliceReserve(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarnessReserve(t, btcutil.SatoshiPerBitcoin, nil)
	pendSplice(t, s.bob, s.link.channel, -3*btcutil.SatoshiPerBitcoin)

	// Within Bob's balance on the current funding, 5 BTC, not above his
	// 1 BTC reserve on the splice's, 2 BTC.
	amt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin * 3 / 2)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	htlc.ChanID = s.link.ChanID()
	s.link.HandleChannelUpdate(htlc)

	select {
	case e := <-s.failures:
		require.Equal(t, LinkFailureDisconnect, e.FailureAction,
			e.Error())
		require.True(t, e.Warning)
	case <-time.After(5 * time.Second):
		t.Fatal("link didn't fail")
	}
}

// TestLinkUpdateAfterTxAbort checks an update the peer sends right after its
// tx_abort, its quiescence ended, before our splice machine has ended ours:
// it waits, rather than failing the link for an update after stfu.
func TestLinkUpdateAfterTxAbort(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	quiesce(t, s)
	s.link.HandleChannelUpdate(&lnwire.TxAbort{
		ChannelID: s.link.ChanID(),
	})

	amt := lnwire.NewMSatFromSatoshis(100_000)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	_, err = s.bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	s.link.HandleChannelUpdate(htlc)
	commit, err := s.bob.SignNextCommitment(t.Context())
	require.NoError(t, err)
	s.link.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID: s.link.ChanID(), CommitSig: commit.CommitSig,
		HtlcSigs: commit.HtlcSigs,
	})
	time.Sleep(100 * time.Millisecond)
	s.requireNoFailure(t)

	require.NoError(t, <-s.link.ResumeQuiescence())
	nextSent[*lnwire.RevokeAndAck](t, s)
	s.requireNoFailure(t)
}

// TestLinkReestablishTxSignaturesFirst checks a reconnection on which the
// peer names a splice in next_funding, our tx_signatures for it out and an
// update of ours owed: the tx_signatures go before the update. The Core
// Lightning port takes nothing else until it has them and drops the
// connection on anything else, at every reconnection.
func TestLinkReestablishTxSignaturesFirst(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)

	txid := pendSplice(
		t, alice.channel, bob.channel, btcutil.SatoshiPerBitcoin,
	)
	original := alice.channel.State().FundingOutpoint.Hash

	// Bob's update and commitment, lost on the way to Alice.
	amt := lnwire.NewMSatFromSatoshis(100_000)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	_, err = bob.channel.AddHTLC(htlc, nil)
	require.NoError(t, err)
	_, err = bob.channel.SignNextCommitment(t.Context())
	require.NoError(t, err)

	aliceHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: txid}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}
	bobHandler := &recordingSpliceHandler{
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
		txSigs: &lnwire.TxSignatures{TxID: txid},
	}
	n := newTwoHopNetworkStopped(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) { c.SpliceHandler = aliceHandler },
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)
	bobHandler.txSigs.ChannelID = n.bobChannelLink.ChanID()
	require.NoError(t, n.aliceServer.Start())
	require.NoError(t, n.bobServer.Start())

	// Alice gets Bob's tx_signatures before his update.
	order := func() (int, int) {
		n.aliceServer.protocolTraceMtx.Lock()
		defer n.aliceServer.protocolTraceMtx.Unlock()

		sigs, add := -1, -1
		for i, m := range n.aliceServer.protocolTrace {
			switch m.(type) {
			case *lnwire.TxSignatures:
				if sigs < 0 {
					sigs = i
				}
			case *lnwire.UpdateAddHTLC:
				if add < 0 {
					add = i
				}
			}
		}

		return sigs, add
	}
	require.Eventually(t, func() bool {
		sigs, add := order()
		return sigs >= 0 && add >= 0
	}, 10*time.Second, 20*time.Millisecond)
	sigs, add := order()
	require.Less(t, sigs, add)
}

// TestLinkUpdateAfterPeerLockBeforeOurs checks the peer's add right after
// its splice_locked, which completed our lock still on its way to the link:
// covered by the peer's balance on the splice's funding, not on the old one.
// It waits for our lock, rather than being refused with an error the Core
// Lightning port fails the channel on.
func TestLinkUpdateAfterPeerLockBeforeOurs(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarness(t)
	s.handler.mu.Lock()
	s.handler.lockComing = true
	s.handler.mu.Unlock()

	// Bob splices in 3 BTC and has locked on his side.
	txid := pendSplice(
		t, s.bob, s.link.channel, 3*btcutil.SatoshiPerBitcoin,
	)
	scid := lnwire.ShortChannelID{BlockHeight: 900_000, TxIndex: 1}
	require.NoError(t, s.bob.LockPendingFunding(txid, scid))
	s.link.HandleChannelUpdate(&lnwire.SpliceLocked{
		ChannelID: s.link.ChanID(), SpliceTxid: txid,
	})

	// More than his 5 BTC before the splice.
	amt := lnwire.NewMSatFromSatoshis(6 * btcutil.SatoshiPerBitcoin)
	var blob [lnwire.OnionPacketSize]byte
	_, htlc, _, err := generatePayment(amt, amt, 5, blob)
	require.NoError(t, err)
	_, err = s.bob.AddHTLC(htlc, nil)
	require.NoError(t, err)
	htlc.ChanID = s.link.ChanID()
	s.link.HandleChannelUpdate(htlc)
	time.Sleep(100 * time.Millisecond)
	s.requireNoFailure(t)

	require.NoError(t, <-s.link.LockSplice(txid, scid))
	s.requireNoFailure(t)

	// Taken up once our lock is in, the machine's say aside: Bob's
	// commitment gets our revocation.
	commit, err := s.bob.SignNextCommitment(t.Context())
	require.NoError(t, err)
	s.link.HandleChannelUpdate(&lnwire.CommitSig{
		ChanID: s.link.ChanID(), CommitSig: commit.CommitSig,
		HtlcSigs: commit.HtlcSigs,
	})
	nextSent[*lnwire.RevokeAndAck](t, s)
	s.requireNoFailure(t)
}

// TestLinkTxSignaturesPastHeldUpdates checks a reconnection on which we are
// still signing and the peer, done, sends its updates before its
// tx_signatures, sent again: the updates wait for our quiescence to end,
// and the tx_signatures, which end it, reach the machine rather than
// waiting behind them for good.
func TestLinkTxSignaturesPastHeldUpdates(t *testing.T) {
	t.Parallel()

	alice, bob, err := createMirroredChannel(
		t, btcutil.SatoshiPerBitcoin*3, btcutil.SatoshiPerBitcoin*5,
	)
	require.NoError(t, err)
	const added = btcutil.SatoshiPerBitcoin
	txid := pendSplice(t, alice.channel, bob.channel, added)
	original := alice.channel.State().FundingOutpoint.Hash

	aliceHandler := &recordingSpliceHandler{
		nextFunding: fn.Some(lnwire.FundingTxidFlags{Txid: txid}),
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
		signing: true,
	}
	bobHandler := &recordingSpliceHandler{
		fundingLocked: fn.Some(lnwire.FundingTxidFlags{
			Txid: original,
		}),
	}
	n := newTwoHopNetworkStopped(
		t, alice.channel, bob.channel, testStartingHeight,
		func(c *ChannelLinkConfig) { c.SpliceHandler = aliceHandler },
		func(c *ChannelLinkConfig) { c.SpliceHandler = bobHandler },
	)
	require.NoError(t, n.aliceServer.Start())
	require.NoError(t, n.bobServer.Start())
	require.Eventually(t, n.aliceChannelLink.quiescer.IsQuiescent,
		5*time.Second, 20*time.Millisecond)
	require.Eventually(t, n.bobChannelLink.EligibleToForward,
		5*time.Second, 20*time.Millisecond)

	// Bob pays Alice: his update and commitment reach her first.
	amount := lnwire.NewMSatFromSatoshis(2_000_000)
	htlcAmt, totalTimelock, hops := generateHops(
		amount, testStartingHeight, n.aliceChannelLink,
	)
	_ = makePayment(
		n.bobServer, n.aliceServer, n.aliceChannelLink.ShortChanID(),
		hops, amount, htlcAmt, totalTimelock,
	)
	time.Sleep(500 * time.Millisecond)

	// Then Bob's tx_signatures, sent again by his machine.
	n.aliceChannelLink.HandleChannelUpdate(&lnwire.TxSignatures{
		ChannelID: n.aliceChannelLink.ChanID(), TxID: txid,
	})
	got := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !got {
		aliceHandler.mu.Lock()
		for _, m := range aliceHandler.messages {
			if _, ok := m.(*lnwire.TxSignatures); ok {
				got = true
			}
		}
		aliceHandler.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	require.True(t, got, "tx_signatures stuck behind held updates")
}

// TestLinkShutdownWithdrawsStfu checks the peer's shutdown read while our
// stfu is owed, our update_fee not yet locked in: the request for quiescence
// is answered with ErrShutdownReceived and no stfu follows the shutdown, as
// BOLT 2 allows no splice once it is sent. Later requests are refused.
func TestLinkShutdownWithdrawsStfu(t *testing.T) {
	t.Parallel()

	s := newSpliceLinkHarnessWith(t, func(cfg *ChannelLinkConfig) {
		cfg.FeeEstimator = chainfee.NewStaticEstimator(50_000, 253)
	})
	require.Eventually(t, s.link.EligibleToForward, 5*time.Second,
		10*time.Millisecond)
	require.NoError(t, <-s.link.runSpliceReq(
		func(ctx context.Context) error {
			return s.link.handleUpdateFee(ctx)
		},
	))
	nextSent[*lnwire.UpdateFee](t, s)

	refused := func(res <-chan fn.Result[lntypes.ChannelParty]) {
		t.Helper()

		select {
		case r := <-res:
			require.ErrorIs(t, r.Err(), ErrShutdownReceived)
		case <-time.After(5 * time.Second):
			t.Fatal("quiescence request not answered")
		}
	}

	res := s.link.InitStfu()
	s.link.ShutdownReceived()
	refused(res)
	refused(s.link.InitStfu())

	timeout := time.After(300 * time.Millisecond)
	for done := false; !done; {
		select {
		case msg := <-s.sent:
			_, stfu := msg.(*lnwire.Stfu)
			require.False(t, stfu, "stfu after the peer's shutdown")
		case <-timeout:
			done = true
		}
	}
}
