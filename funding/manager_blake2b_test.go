package funding

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnrpc"
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

	t.Run("funder refuses", func(t *testing.T) {
		t.Parallel()

		alice, bob := setupFundingManagers(t, requireBit)
		t.Cleanup(func() { tearDownFundingManagers(t, alice, bob) })

		// A testNode's remoteFeatures are what its peer sees of it:
		// Bob's init set nothing, so Alice will not start.
		bob.remoteFeatures = nil

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
		bob.remoteFeatures = []lnwire.FeatureBit{
			lnwire.Blake2bRequired,
		}
		alice.remoteFeatures = []lnwire.FeatureBit{
			lnwire.StaticRemoteKeyOptional,
		}

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

			alice.remoteFeatures = []lnwire.FeatureBit{bit}
			bob.remoteFeatures = []lnwire.FeatureBit{bit}

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
