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
// returns it, with the funding script and the channel's capacity.
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

	pkScript, err := MakeFundingScript(ch)
	require.NoError(t, err)

	require.True(t, check(&utxoChainIO{
		txOut: wire.NewTxOut(int64(ch.Capacity), pkScript),
	}))
	require.False(t, check(&utxoChainIO{err: errors.New("not found")}))

	// Some other unspent output at that outpoint is not the funding: a
	// bitcoind backend answers by outpoint alone.
	require.False(t, check(&utxoChainIO{
		txOut: wire.NewTxOut(int64(ch.Capacity), []byte{0x51}),
	}))
	require.False(t, check(&utxoChainIO{
		txOut: wire.NewTxOut(1, pkScript),
	}))

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

// TestAcceptChannelUnpromptedUnifiedSigs checks that a funder refuses an
// accept_channel whose type differs from the one it proposed in the unified
// bit: a reservation made for a type without option_unified_sigs signs the
// ordinary way, and a peer signing under the unified hash could never produce
// a signature it accepts. Every open carries an explicit type (BOLT 2), and
// the reply must echo it exactly, so a reply that adds the bit, even or odd,
// or that names no type, is refused. Only a node that does not sign under the
// unified hash proposes a type without the bit, so that is the node here.
func TestAcceptChannelUnpromptedUnifiedSigs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		extra   []lnwire.FeatureBit
		noType  bool
		refused bool
	}{
		{
			name:    "the type echoed",
			refused: false,
		},
		{
			name:    "no type echoed",
			noType:  true,
			refused: true,
		},
		{
			name:    "type with unified sigs",
			extra:   []lnwire.FeatureBit{lnwire.UnifiedSigsRequired},
			refused: true,
		},
		{
			name:    "type with the odd unified bit",
			extra:   []lnwire.FeatureBit{lnwire.UnifiedSigsOptional},
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})

			// Neither side signs under the unified hash, so Alice
			// proposes a type without the bit and her reservation
			// signs the ordinary way.
			featureBits := []lnwire.FeatureBit{
				lnwire.ExplicitChannelTypeOptional,
				lnwire.StaticRemoteKeyOptional,
				lnwire.AnchorsZeroFeeHtlcTxOptional,
				lnwire.Blake2bRequired,
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
			require.NotNil(t, open.ChannelType)
			proposed := lnwire.RawFeatureVector(*open.ChannelType)
			require.False(
				t, proposed.IsSet(lnwire.UnifiedSigsRequired),
			)

			bob.fundingMgr.ProcessFundingMsg(open, alice)
			accept, ok := assertFundingMsgSent(
				t, bob.msgChan, "AcceptChannel",
			).(*lnwire.AcceptChannel)
			require.True(t, ok)

			// Bob's reply: the proposed type, with a bit added, or
			// none.
			reply := proposed.Clone()
			for _, bit := range tc.extra {
				reply.Set(bit)
			}
			accept.ChannelType = (*lnwire.ChannelType)(reply)
			if tc.noType {
				accept.ChannelType = nil
			}
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

// TestRequireUnifiedSigs checks the rule itself: on a node that signs under the
// unified hash a new channel's type must carry option_unified_sigs, and a node
// that does not is not held to it.
func TestRequireUnifiedSigs(t *testing.T) {
	t.Parallel()

	unified := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.UnifiedSigsOptional),
		lnwire.Features,
	)
	plain := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(), lnwire.Features,
	)
	withBit := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
		lnwire.UnifiedSigsRequired,
	))
	without := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.StaticRemoteKeyRequired,
		lnwire.AnchorsZeroFeeHtlcTxRequired,
	))

	require.NoError(t, requireUnifiedSigs(&withBit, unified))
	require.ErrorIs(
		t, requireUnifiedSigs(&without, unified),
		errUnifiedSigsRequired,
	)
	require.ErrorIs(
		t, requireUnifiedSigs(nil, unified), errUnifiedSigsRequired,
	)

	require.NoError(t, requireUnifiedSigs(&without, plain))
	require.NoError(t, requireUnifiedSigs(nil, plain))
}

// TestRequireUnifiedSigsOnOpen checks BOLT 2's open_channel requirements on a
// node that signs under the unified hash: as funder it opens no channel whose
// type lacks option_unified_sigs, and as fundee it fails an open that names
// such a type, or none.
func TestRequireUnifiedSigsOnOpen(t *testing.T) {
	t.Parallel()

	withUnified := []lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.SimpleTaprootChannelsOptionalFinal,
		lnwire.Blake2bRequired,
		lnwire.UnifiedSigsOptional,
	}
	withoutUnified := []lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.Blake2bRequired,
	}
	noExplicit := []lnwire.FeatureBit{
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.Blake2bRequired,
		lnwire.UnifiedSigsOptional,
	}
	taproot := lnwire.ChannelType(*lnwire.NewRawFeatureVector(
		lnwire.SimpleTaprootChannelsRequiredFinal,
	))

	// The funder's side: Alice signs under the unified hash.
	funderCases := []struct {
		name     string
		remote   []lnwire.FeatureBit
		local    []lnwire.FeatureBit
		chanType *lnwire.ChannelType
		opens    bool
	}{{
		name:   "peer with the bit",
		local:  withUnified,
		remote: withUnified,
		opens:  true,
	}, {
		name:   "peer without the bit",
		local:  withUnified,
		remote: withoutUnified,
		opens:  false,
	}, {
		// A peer that does not advertise option_channel_type still
		// gets an explicit type, which BOLT 2 now requires of every
		// open, and so the bit.
		name:   "no option_channel_type",
		local:  noExplicit,
		remote: noExplicit,
		opens:  true,
	}, {
		name:     "a taproot type",
		local:    withUnified,
		remote:   withUnified,
		chanType: &taproot,
		opens:    false,
	}}
	for _, tc := range funderCases {
		t.Run("funder: "+tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})
			// The harness reads a node's own features from the
			// peer it handles: Alice opening to Bob reads Bob's
			// fields.
			bob.localFeatures = tc.local
			bob.remoteFeatures = tc.remote

			errChan := make(chan error, 1)
			alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
				Peer:            bob,
				TargetPubkey:    bob.privKey.PubKey(),
				ChainHash:       *fundingNetParams.GenesisHash,
				LocalFundingAmt: 500000,
				Private:         true,
				ChannelType:     tc.chanType,
				Updates: make(
					chan *lnrpc.OpenStatusUpdate,
				),
				Err: errChan,
			})

			if tc.opens {
				open := expectOpenChannelMsg(t, alice.msgChan)
				require.NotNil(t, open.ChannelType)
				sent := lnwire.RawFeatureVector(
					*open.ChannelType,
				)
				require.True(
					t, sent.IsSet(lnwire.UnifiedSigsRequired),
				)

				return
			}

			select {
			case err := <-errChan:
				require.Error(t, err)
			case msg := <-alice.msgChan:
				t.Fatalf("expected a local error, got %T", msg)
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out waiting for the refusal")
			}
		})
	}

	// The fundee's side: Bob signs under the unified hash, and Alice, who
	// does not, proposes a type without the bit, or none at all.
	fundeeCases := []struct {
		name     string
		features []lnwire.FeatureBit
	}{{
		name:     "a type without the bit",
		features: withoutUnified,
	}, {
		name: "no type",
		features: []lnwire.FeatureBit{
			lnwire.StaticRemoteKeyOptional,
			lnwire.AnchorsZeroFeeHtlcTxOptional,
			lnwire.Blake2bRequired,
		},
	}}
	for _, tc := range fundeeCases {
		t.Run("fundee: "+tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})
			// Alice's features, read while she opens to Bob, and
			// Bob's, read while he handles her open.
			bob.localFeatures = tc.features
			bob.remoteFeatures = withUnified
			alice.localFeatures = withUnified
			alice.remoteFeatures = tc.features

			alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
				Peer:            bob,
				TargetPubkey:    bob.privKey.PubKey(),
				ChainHash:       *fundingNetParams.GenesisHash,
				LocalFundingAmt: 500000,
				Private:         true,
				Updates: make(
					chan *lnrpc.OpenStatusUpdate,
				),
				Err: make(chan error, 1),
			})
			open := expectOpenChannelMsg(t, alice.msgChan)
			if open.ChannelType != nil {
				sent := lnwire.RawFeatureVector(
					*open.ChannelType,
				)
				require.False(
					t, sent.IsSet(lnwire.UnifiedSigsRequired),
				)
			}

			bob.fundingMgr.ProcessFundingMsg(open, alice)
			assertFundingMsgSent(t, bob.msgChan, "Error")
		})
	}
}

// TestRequireUnifiedSigsWithoutChannelTypeFeature checks the fundee against a
// peer that does not advertise option_channel_type. Before BOLT 2 made the
// channel type mandatory, such a peer's proposal was negotiated implicitly,
// with no type, so a reservation for a type carrying option_unified_sigs
// would have signed the ordinary way; that was refused. Every open now
// carries an explicit type, negotiated as proposed: with the bit, the channel
// is accepted and its reservation signs under the unified hash, as the type
// says; without it, the open is refused.
func TestRequireUnifiedSigsWithoutChannelTypeFeature(t *testing.T) {
	t.Parallel()

	// Alice's features: no option_channel_type, but the unified bit.
	attacker := []lnwire.FeatureBit{
		lnwire.StaticRemoteKeyOptional,
		lnwire.AnchorsZeroFeeHtlcTxOptional,
		lnwire.Blake2bRequired,
		lnwire.UnifiedSigsOptional,
	}
	// Bob's: the full set.
	honest := append([]lnwire.FeatureBit{
		lnwire.ExplicitChannelTypeOptional,
	}, attacker...)

	for _, tc := range []struct {
		name    string
		bits    []lnwire.FeatureBit
		accepts bool
	}{{
		name: "with the unified bit",
		bits: []lnwire.FeatureBit{
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
			lnwire.UnifiedSigsRequired,
		},
		accepts: true,
	}, {
		name: "without it",
		bits: []lnwire.FeatureBit{
			lnwire.StaticRemoteKeyRequired,
			lnwire.AnchorsZeroFeeHtlcTxRequired,
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			alice, bob := setupFundingManagers(t)
			t.Cleanup(func() {
				tearDownFundingManagers(t, alice, bob)
			})

			// The harness reads a node's own features from the
			// peer it handles. Alice's funding manager is run
			// without the unified bit so that it sends the open at
			// all; the message is then shaped as such a peer would
			// shape it, and Bob sees her advertise the bit.
			bob.localFeatures = attacker[:3]
			bob.remoteFeatures = honest
			alice.localFeatures = honest
			alice.remoteFeatures = attacker

			alice.fundingMgr.InitFundingWorkflow(&InitFundingMsg{
				Peer:            bob,
				TargetPubkey:    bob.privKey.PubKey(),
				ChainHash:       *fundingNetParams.GenesisHash,
				LocalFundingAmt: 500000,
				Private:         true,
				Updates: make(
					chan *lnrpc.OpenStatusUpdate,
				),
				Err: make(chan error, 1),
			})
			open := expectOpenChannelMsg(t, alice.msgChan)

			proposed := lnwire.ChannelType(
				*lnwire.NewRawFeatureVector(tc.bits...),
			)
			open.ChannelType = &proposed

			bob.fundingMgr.ProcessFundingMsg(open, alice)
			if !tc.accepts {
				assertFundingMsgSent(t, bob.msgChan, "Error")
				return
			}

			// Accepted with exactly the proposed type, the bit
			// included, so both sides sign under the unified hash.
			accept, ok := assertFundingMsgSent(
				t, bob.msgChan, "AcceptChannel",
			).(*lnwire.AcceptChannel)
			require.True(t, ok)
			require.NotNil(t, accept.ChannelType)
			echoed := lnwire.RawFeatureVector(*accept.ChannelType)
			want := lnwire.RawFeatureVector(proposed)
			require.True(t, echoed.Equals(&want))
		})
	}
}
