package funding

import (
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

// TestFundingTimeoutApplies pins who gives up on a pending channel: only a
// fundee, not on a zero-conf channel, and not once the funding transaction
// has confirmed, which a channel funded by a coinbase stays pending well past.
func TestFundingTimeoutApplies(t *testing.T) {
	t.Parallel()

	ch := func(initiator, zeroConf bool,
		confHeight uint32) *channeldb.OpenChannel {

		c := &channeldb.OpenChannel{
			IsInitiator:        initiator,
			ConfirmationHeight: confHeight,
		}
		if zeroConf {
			c.ChanType |= channeldb.ZeroConfBit
		}

		return c
	}

	require.True(t, fundingTimeoutApplies(ch(false, false, 0)))

	require.False(t, fundingTimeoutApplies(ch(true, false, 0)),
		"the funder has funds at stake and never gives up")
	require.False(t, fundingTimeoutApplies(ch(false, true, 0)),
		"a zero-conf channel is usable before it confirms")
	require.False(t, fundingTimeoutApplies(ch(false, false, 961650)),
		"the funding transaction has confirmed; a restart while the "+
			"channel waits out a coinbase maturity must not forget it")
}
