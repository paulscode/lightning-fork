package funding

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
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
