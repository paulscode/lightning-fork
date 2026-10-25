package peer

import (
	"bytes"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/splice"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestPeerNoCloseDuringSplice checks that a channel with a splice that
// hasn't locked can't be closed cooperatively: our close request is refused,
// and the peer's shutdown gets a warning and ends the connection.
func TestPeerNoCloseDuringSplice(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	peer := harness.peer
	chanPoint := harness.channel.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	ch, ok := peer.activeChannels.Load(chanID)
	require.True(t, ok)
	local, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	remote, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	require.NoError(t, ch.AddPendingFunding(lnwallet.PendingFunding{
		OutPoint:   wire.OutPoint{Hash: chainhash.Hash{1}},
		Capacity:   ch.State().Capacity + 1_000,
		LocalKey:   keychain.KeyDescriptor{PubKey: local.PubKey()},
		RemoteKey:  remote.PubKey(),
		LocalDelta: 1_000,
	}))
	require.True(t, peer.spliceUnlocked(chanID))

	// Dead, it can never confirm: no bar to closing, which nothing else
	// would ever lift.
	dead := &channeldb.SpliceRecord{
		OutPoint:    wire.OutPoint{Hash: chainhash.Hash{1}},
		Capacity:    ch.State().Capacity + 1_000,
		LocalKey:    keychain.KeyDescriptor{PubKey: local.PubKey()},
		RemoteKey:   remote.PubKey(),
		LocalDelta:  1_000,
		Negotiation: &channeldb.SpliceNegotiation{Dead: true},
	}
	require.NoError(t, ch.State().PutSpliceRecord(dead))
	require.False(t, peer.spliceUnlocked(chanID))
	dead.Negotiation.Dead = false
	require.NoError(t, ch.State().PutSpliceRecord(dead))
	require.True(t, peer.spliceUnlocked(chanID))

	// Disowned by the peer: a close, spending the output it spends, ends
	// it, and is the way out.
	dead.Negotiation.Disowned = true
	require.NoError(t, ch.State().PutSpliceRecord(dead))
	require.False(t, peer.spliceUnlocked(chanID))
	dead.Negotiation.Disowned = false
	require.NoError(t, ch.State().PutSpliceRecord(dead))
	require.True(t, peer.spliceUnlocked(chanID))

	// Ours.
	req := &htlcswitch.ChanClose{
		CloseType: contractcourt.CloseRegular,
		ChanPoint: &chanPoint,
		Updates:   make(chan interface{}, 1),
		Err:       make(chan error, 1),
	}
	peer.handleLocalCloseReq(req)
	require.ErrorIs(t, <-req.Err, errSpliceUnlocked)

	// The peer's.
	peer.remoteFeatures = lnwire.EmptyFeatureVector()
	peer.outgoingQueue = make(chan outgoingMsg, 4)
	var shutdown bytes.Buffer
	_, err = lnwire.WriteMessage(&shutdown, lnwire.NewShutdown(
		chanID, genScript(t, p2wshAddress),
	), 0)
	require.NoError(t, err)

	peer.cg.WgAdd(1)
	go peer.readHandler()
	harness.mockConn.readMessages <- shutdown.Bytes()

	queued, err := fn.RecvOrTimeout(peer.outgoingQueue, timeout)
	require.NoError(t, err)
	warning, ok := queued.msg.(*lnwire.Warning)
	require.True(t, ok, "%T", queued.msg)
	require.Equal(t, chanID, warning.ChanID)
}

// TestPeerClosesDisowned checks the close the splice machine asks for when
// the peer disowns a splice: a cooperative one starts the channel's closer,
// and another request then is refused, never replacing the closer.
func TestPeerClosesDisowned(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	peer := harness.peer
	chanPoint := harness.channel.ChannelPoint()
	chanID := lnwire.NewChanIDFromOutPoint(chanPoint)

	go peer.closeDisowned(chanID, false)
	require.Eventually(t, func() bool {
		_, ok := peer.activeChanCloses.Load(chanID)
		return ok
	}, timeout, 10*time.Millisecond)

	// Asked again, as at the next connection: the closer stays.
	first, _ := peer.activeChanCloses.Load(chanID)
	go peer.closeDisowned(chanID, false)
	time.Sleep(300 * time.Millisecond)
	again, _ := peer.activeChanCloses.Load(chanID)
	require.Same(t, first.UnwrapLeftOr(nil), again.UnwrapLeftOr(nil))

	// A close under way: refused.
	req := &htlcswitch.ChanClose{
		CloseType:    contractcourt.CloseRegular,
		ChanPoint:    &chanPoint,
		Updates:      make(chan interface{}, 1),
		Err:          make(chan error, 1),
		NotIfClosing: true,
	}
	peer.HandleLocalCloseChanReqs(req)
	closeErr, err := fn.RecvOrTimeout(req.Err, timeout)
	require.NoError(t, err)
	require.ErrorIs(t, closeErr, errAlreadyClosing)
}

// TestSpliceMachineForHolds checks that a channel holding coins the peer put
// into a splice gets a machine, disabled, when splicing doesn't apply to the
// connection: the peer left the feature out, or splicing was turned off. The
// machine is what enforces the hold.
func TestSpliceMachineForHolds(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	peer := harness.peer
	chanID := lnwire.NewChanIDFromOutPoint(harness.channel.ChannelPoint())
	ch, ok := peer.activeChannels.Load(chanID)
	require.True(t, ok)

	peer.cfg.SpliceMachine = func(splice.Config,
		*btcec.PublicKey) *splice.Machine {

		return nil
	}
	peer.cfg.SpliceEnabled = true

	// The peer supports splicing: a machine, enabled.
	peer.remoteFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.SpliceOptional),
		lnwire.Features,
	)
	start, disabled := peer.spliceMachineMode(ch)
	require.True(t, start)
	require.False(t, disabled)

	// It left the feature out: no machine while nothing is held.
	peer.remoteFeatures = lnwire.EmptyFeatureVector()
	start, _ = peer.spliceMachineMode(ch)
	require.False(t, start)

	// With a hold, a disabled machine.
	require.NoError(t, ch.State().PutSpliceHold(channeldb.SpliceHold{
		Txid:   chainhash.Hash{2},
		Amount: 500_000,
	}))
	start, disabled = peer.spliceMachineMode(ch)
	require.True(t, start)
	require.True(t, disabled)

	// The same with splicing turned off here and the peer supporting it.
	peer.cfg.SpliceEnabled = false
	peer.remoteFeatures = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.SpliceOptional),
		lnwire.Features,
	)
	start, disabled = peer.spliceMachineMode(ch)
	require.True(t, start)
	require.True(t, disabled)
}

// TestPeerShutdownEndsSplicing checks the peer's shutdown, with no splice
// under way: as it is read, the channel's splice machine refuses splices
// with "closing", which the estimate and the channel's splice_status report,
// before we answer it and the channel's state shows the close. lnd started a
// splice then, which the Core Lightning port refused with a warning.
func TestPeerShutdownEndsSplicing(t *testing.T) {
	t.Parallel()

	harness, err := createTestPeerWithChannel(t, noUpdate)
	require.NoError(t, err)
	peer := harness.peer
	chanID := lnwire.NewChanIDFromOutPoint(harness.channel.ChannelPoint())
	ch, ok := peer.activeChannels.Load(chanID)
	require.True(t, ok)

	m := splice.NewMachine(splice.Config{ChanID: chanID, Channel: ch})
	peer.spliceMtx.Lock()
	peer.spliceMachines[chanID] = m
	peer.spliceMtx.Unlock()
	require.Empty(t, m.Refusal())
	require.False(t, peer.spliceUnlocked(chanID))

	peer.remoteFeatures = lnwire.EmptyFeatureVector()
	var shutdown bytes.Buffer
	_, err = lnwire.WriteMessage(&shutdown, lnwire.NewShutdown(
		chanID, genScript(t, p2wshAddress),
	), 0)
	require.NoError(t, err)

	peer.cg.WgAdd(1)
	go peer.readHandler()
	harness.mockConn.readMessages <- shutdown.Bytes()

	require.Eventually(t, func() bool {
		return m.Refusal() == "closing"
	}, timeout, 10*time.Millisecond)
}
