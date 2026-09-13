package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestPreForkChannels: only channels confirmed below the activation height
// are reported, unconfirmed zero-conf channels are skipped, and a zero-conf
// channel is judged by its real short channel id.
func TestPreForkChannels(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := key.PubKey()

	chanAt := func(height uint32) *channeldb.OpenChannel {
		return &channeldb.OpenChannel{
			ShortChannelID:  lnwire.ShortChannelID{BlockHeight: height},
			FundingOutpoint: wire.OutPoint{Index: height},
			IdentityPub:     pub,
		}
	}

	channels := []*channeldb.OpenChannel{
		chanAt(961639), chanAt(961640), chanAt(1000000), chanAt(0),
	}
	found := preForkChannels(channels, 961640)
	require.Len(t, found, 1)
	require.Contains(t, found[0], "confirmed at 961639")

	// No activation height (a network that never forked) reports nothing.
	require.Empty(t, preForkChannels(channels, 0))

	// An unconfirmed zero-conf channel carries an alias, not a height,
	// and is skipped rather than judged by it.
	zc := chanAt(0)
	zc.ChanType = channeldb.ZeroConfBit | channeldb.ScidAliasChanBit
	zc.ShortChannelID = lnwire.ShortChannelID{BlockHeight: 16000000}
	require.Empty(t, preForkChannels([]*channeldb.OpenChannel{zc}, 961640))
}
