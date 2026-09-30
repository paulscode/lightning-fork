package funding

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestFundingTimeoutApplies pins who gives up on a pending channel: only a
// fundee, and not on a zero-conf channel. Whether the funding has confirmed is
// asked of the chain when the timeout falls due; see TestFundingInChain.
func TestFundingTimeoutApplies(t *testing.T) {
	t.Parallel()

	ch := func(initiator, zeroConf bool) *channeldb.OpenChannel {
		c := &channeldb.OpenChannel{IsInitiator: initiator}
		if zeroConf {
			c.ChanType |= channeldb.ZeroConfBit
		}

		return c
	}

	require.True(t, fundingTimeoutApplies(ch(false, false)))
	require.False(t, fundingTimeoutApplies(ch(true, false)),
		"the funder has funds at stake and never gives up")
	require.False(t, fundingTimeoutApplies(ch(false, true)),
		"a zero-conf channel is usable before it confirms")
}

// utxoChainIO answers GetUtxo with a canned result, for fundingInChain.
type utxoChainIO struct {
	mock.ChainIO
	txOut *wire.TxOut
	err   error
}

func (c *utxoChainIO) GetUtxo(*wire.OutPoint, []byte, uint32,
	<-chan struct{}) (*wire.TxOut, error) {

	return c.txOut, c.err
}

// TestFundingInChain checks the question a fundee asks when its funding
// timeout falls due: the funding output is in the chain only if the backend
// returns it.
func TestFundingInChain(t *testing.T) {
	t.Parallel()

	key := func() *btcec.PublicKey {
		k, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return k.PubKey()
	}
	ch := &channeldb.OpenChannel{
		ChanType: channeldb.SingleFunderTweaklessBit |
			channeldb.AnchorOutputsBit,
		Capacity:        1_000_000,
		FundingOutpoint: wire.OutPoint{Index: 1},
	}
	ch.LocalChanCfg.MultiSigKey.PubKey = key()
	ch.RemoteChanCfg.MultiSigKey.PubKey = key()

	check := func(io *utxoChainIO) bool {
		f := &Manager{
			cfg: &Config{Wallet: &lnwallet.LightningWallet{
				Cfg: lnwallet.Config{ChainIO: io},
			}},
			quit: make(chan struct{}),
		}

		return f.fundingInChain(ch)
	}

	require.True(t, check(&utxoChainIO{txOut: wire.NewTxOut(1, nil)}))
	require.False(t, check(&utxoChainIO{err: errors.New("not found")}))

	// The mock backends answer (nil, nil); that is not an output.
	require.False(t, check(&utxoChainIO{}))
}

// TestRequireBlake2bPeer checks that a channel is refused, in both
// directions, with a peer whose init message does not set option_blake2b,
// and opens as usual with one that sets either form.
func TestRequireBlake2bPeer(t *testing.T) {
	t.Parallel()

	requireBit := func(c *Config) { c.RequireBlake2bPeer = true }

	// What both sides support besides the bit under test: an explicit
	// channel type, as upstream from v0.21.4 requires, and anchors.
	common := []lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
	}
	with := func(bits ...lnwire.FeatureBit) []lnwire.FeatureBit {
		return append(append([]lnwire.FeatureBit{}, common...), bits...)
	}

	t.Run("funder refuses", func(t *testing.T) {
		t.Parallel()

		alice, bob := setupFundingManagers(t, requireBit)
		t.Cleanup(func() { tearDownFundingManagers(t, alice, bob) })

		// A testNode's remoteFeatures are what its peer sees of it:
		// Bob's init sets everything but the bit, so Alice will not
		// start.
		alice.localFeatures = with(lnwire.Blake2bRequired)
		bob.remoteFeatures = with()

		errChan := make(chan error, 1)
		alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
			Peer:            bob,
			TargetPubkey:    bob.privKey.PubKey(),
			ChainHash:       *fundingNetParams.GenesisHash,
			LocalFundingAmt: 500000,
			FundingFeePerKw: 1000,
			Updates:         make(chan *lnrpc.OpenStatusUpdate),
			Err:             errChan,
		})

		select {
		case err := <-errChan:
			require.ErrorIs(t, err, lnwire.ErrPeerNotBlake2b)
		case <-time.After(5 * time.Second):
			t.Fatal("funding was not refused")
		}

		// Nothing reached Bob and nothing is reserved.
		assertErrorNotSent(t, alice.msgChan)
		assertNumPendingReservations(t, alice, bobPubKey, 0)
	})

	t.Run("fundee refuses", func(t *testing.T) {
		t.Parallel()

		alice, bob := setupFundingManagers(t, requireBit)
		t.Cleanup(func() { tearDownFundingManagers(t, alice, bob) })

		// Alice sees the bit from Bob and starts; Bob sees an init
		// from her without it. A testNode's remoteFeatures are what its
		// peer sees of it.
		alice.localFeatures = with(lnwire.Blake2bRequired)
		bob.localFeatures = with(lnwire.Blake2bRequired)
		bob.remoteFeatures = with(lnwire.Blake2bRequired)
		alice.remoteFeatures = with()

		errChan := make(chan error, 1)
		alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
			Peer:            bob,
			TargetPubkey:    bob.privKey.PubKey(),
			ChainHash:       *fundingNetParams.GenesisHash,
			LocalFundingAmt: 500000,
			FundingFeePerKw: 1000,
			Updates:         make(chan *lnrpc.OpenStatusUpdate),
			Err:             errChan,
		})

		var open lnwire.Message
		select {
		case open = <-alice.msgChan:
		case err := <-errChan:
			t.Fatalf("alice did not start: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("alice sent no open_channel")
		}
		require.IsType(t, &lnwire.OpenChannel{}, open)

		bob.fundingMgr.ProcessFundingMsg(open, alice)

		var reply lnwire.Message
		select {
		case reply = <-bob.msgChan:
		case <-time.After(5 * time.Second):
			t.Fatal("bob did not answer")
		}
		errMsg, ok := reply.(*lnwire.Error)
		require.True(t, ok, "expected an error, got %T", reply)
		require.Contains(t, string(errMsg.Data),
			lnwire.ErrPeerNotBlake2b.Error(),
			"the peer is told why, not sent a generic error")
		assertNumPendingReservations(t, bob, alicePubKey, 0)
	})

	for _, bit := range []lnwire.FeatureBit{
		lnwire.Blake2bRequired, lnwire.Blake2bOptional,
	} {
		t.Run(fmt.Sprintf("opens with bit %d", bit), func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t, requireBit)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})

			alice.localFeatures = with(lnwire.Blake2bRequired)
			bob.localFeatures = with(lnwire.Blake2bRequired)
			alice.remoteFeatures = with(bit)
			bob.remoteFeatures = with(bit)

			updateChan := make(chan *lnrpc.OpenStatusUpdate)
			openChannel(
				t, alice, bob, 500000, 0, 1, updateChan, true,
				nil,
			)
			assertErrorNotSent(t, alice.msgChan)
			assertErrorNotSent(t, bob.msgChan)
		})
	}
}

// TestRequireBlake2bPeerOffByDefault pins that the check is opt-in for the
// funding manager, so upstream's funding tests, whose mock peers set no
// bits, keep running; the daemon turns it on in server.go.
func TestRequireBlake2bPeerOffByDefault(t *testing.T) {
	t.Parallel()

	f := Manager{cfg: &Config{}}
	require.NoError(t, f.checkPeerBlake2b(&testNode{}))

	f.cfg.RequireBlake2bPeer = true
	require.ErrorIs(
		t, f.checkPeerBlake2b(&testNode{}), lnwire.ErrPeerNotBlake2b,
	)
}

// TestAcceptChannelUnpromptedUnifiedSigs checks that a funder which proposed
// no channel type refuses an accept_channel whose type carries
// option_unified_sigs. Without explicit negotiation the reservation signs the
// ordinary way; a peer signing under the unified hash could never produce a
// signature it accepts.
func TestAcceptChannelUnpromptedUnifiedSigs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		reply   *lnwire.RawFeatureVector
		refused bool
	}{
		{
			// BOLT 2: no type proposed, none echoed.
			name:    "no type",
			refused: false,
		},
		{
			name: "type with unified sigs",
			reply: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.UnifiedSigsRequired,
			),
			refused: true,
		},
		{
			name: "type with the odd unified bit",
			reply: lnwire.NewRawFeatureVector(
				lnwire.StaticRemoteKeyRequired,
				lnwire.AnchorsZeroFeeHtlcTxRequired,
				lnwire.UnifiedSigsOptional,
			),
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})

			// Both sides support unified signatures but not
			// explicit channel type negotiation, so Alice proposes
			// no type and her reservation signs the ordinary way.
			featureBits := []lnwire.FeatureBit{
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.Blake2bRequired,
				lnwire.UnifiedSigsOptional,
			}
			alice.localFeatures = featureBits
			alice.remoteFeatures = featureBits
			bob.localFeatures = featureBits
			bob.remoteFeatures = featureBits

			alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
				Peer:            bob,
				TargetPubkey:    bob.privKey.PubKey(),
				ChainHash:       *fundingNetParams.GenesisHash,
				LocalFundingAmt: 500000,
				Updates: make(
					chan *lnrpc.OpenStatusUpdate,
				),
				Err: make(chan error, 1),
			})
			open := expectOpenChannelMsg(t, alice.msgChan)
			require.Nil(t, open.ChannelType)

			bob.fundingMgr.ProcessFundingMsg(open, alice)
			accept, ok := assertFundingMsgSent(
				t, bob.msgChan, "AcceptChannel",
			).(*lnwire.AcceptChannel)
			require.True(t, ok)

			// Bob's reply names a type anyway, or none.
			accept.ChannelType = (*lnwire.ChannelType)(tc.reply)
			alice.fundingMgr.ProcessFundingMsg(accept, bob)

			if tc.refused {
				assertFundingMsgSent(t, alice.msgChan, "Error")
			} else {
				assertFundingMsgSent(
					t, alice.msgChan, "FundingCreated",
				)
			}
		})
	}
}

// TestResignChannelProof checks that this node's half of a channel's proof
// can be signed again, as the gossiper asks for a channel whose stored proof
// was made under the withdrawn chain_hash. The new half covers the message as
// it is sent now, so for a channel announced by this release it is the very
// half sent the first time: the signatures are deterministic.
func TestResignChannelProof(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() { tearDownFundingManagers(t, alice, bob) })

	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, 500000, 0, 1, updateChan, true, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)

	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	readyAlice, ok := assertFundingMsgSent(
		t, alice.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)
	readyBob, ok := assertFundingMsgSent(
		t, bob.msgChan, "ChannelReady",
	).(*lnwire.ChannelReady)
	require.True(t, ok)
	assertChannelReadySent(t, alice, bob, fundingOutPoint)
	alice.fundingMgr.ProcessFundingMsg(readyBob, bob)
	bob.fundingMgr.ProcessFundingMsg(readyAlice, alice)
	assertHandleChannelReady(t, alice, bob)
	assertChannelAnnouncements(t, alice, bob, 500000, nil, nil, nil, nil)
	assertAddedToGraph(t, alice, bob, fundingOutPoint)
	waitForOpenUpdate(t, updateChan)

	alice.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}
	bob.mockNotifier.sixConfChannel <- &chainntnfs.TxConfirmation{
		Tx: fundingTx,
	}

	// Alice's first half, among what she announces at six confirmations.
	var first *lnwire.AnnounceSignatures1
	for i := 0; i < 2; i++ {
		select {
		case msg := <-alice.announceChan:
			if sig, ok := msg.(*lnwire.AnnounceSignatures1); ok {
				first = sig
			}
		case <-time.After(5 * time.Second):
			t.Fatal("alice did not announce the channel")
		}
	}
	require.NotNil(t, first)

	channels, err := alice.fundingMgr.cfg.ChannelDB.FetchAllOpenChannels()
	require.NoError(t, err)
	require.Len(t, channels, 1)
	scid := channels[0].ShortChanID()
	require.Equal(t, first.ShortChannelID, scid)

	// Handing the half over waits for the gossiper to take it, which is
	// why the gossiper calls this from a goroutine of its own.
	errChan := make(chan error, 1)
	go func() { errChan <- alice.fundingMgr.ResignChannelProof(scid) }()

	select {
	case msg := <-alice.announceChan:
		again, ok := msg.(*lnwire.AnnounceSignatures1)
		require.True(t, ok, "sent %T", msg)
		require.Equal(t, first, again)

	case <-time.After(5 * time.Second):
		t.Fatal("no new half was handed to the gossiper")
	}
	require.NoError(t, <-errChan)

	// A channel it does not have is refused.
	unknown := scid
	unknown.TxPosition++
	require.ErrorContains(t,
		alice.fundingMgr.ResignChannelProof(unknown), "no open channel")

}

// TestResignChannelProofPrivate: a private channel has no proof to replace,
// and is refused.
func TestResignChannelProofPrivate(t *testing.T) {
	t.Parallel()

	alice, bob := setupFundingManagers(t)
	t.Cleanup(func() { tearDownFundingManagers(t, alice, bob) })

	updateChan := make(chan *lnrpc.OpenStatusUpdate)
	fundingOutPoint, fundingTx := openChannel(
		t, alice, bob, 500000, 0, 1, updateChan, false, nil,
	)
	chanID := lnwire.NewChanIDFromOutPoint(*fundingOutPoint)
	sendAndCheckFirstConfirmation(t, alice, chanID, fundingTx)
	sendAndCheckFirstConfirmation(t, bob, chanID, fundingTx)
	assertMarkedOpen(t, alice, bob, fundingOutPoint)

	channels, err := alice.fundingMgr.cfg.ChannelDB.FetchAllOpenChannels()
	require.NoError(t, err)
	require.Len(t, channels, 1)

	require.ErrorContains(t, alice.fundingMgr.ResignChannelProof(
		channels[0].ShortChanID(),
	), "not public")
}
