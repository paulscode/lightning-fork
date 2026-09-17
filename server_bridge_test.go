package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// openChannel builds an open channel with the balance and reserve a test
// cares about. Everything else is left zero: nothing below reads it.
func openChannel(index uint32, localMsat lnwire.MilliSatoshi,
	reserveSat btcutil.Amount, pending bool) *channeldb.OpenChannel {

	return &channeldb.OpenChannel{
		IsPending: pending,
		FundingOutpoint: wire.OutPoint{
			Hash: [32]byte{byte(index)}, Index: index,
		},
		LocalChanCfg: channeldb.ChannelConfig{
			ChannelStateBounds: channeldb.ChannelStateBounds{
				ChanReserve: reserveSat,
			},
		},
		LocalCommitment: channeldb.ChannelCommitment{
			LocalBalance: localMsat,
		},
	}
}

// What the bridge can pay with is not what the channels hold. The reserve is
// the operator's own balance and is not spendable, a pending channel carries
// nothing yet, and a peer that is offline holds balance no payment can route
// through. Counting any of them lets the bridge quote a swap it cannot pay.
//
// This is the local half of the rule the Bitcoin side applies to its own
// channels. The two have to agree: a bridge that measures its sides
// differently prices one of them against a quantity the other does not mean.
func TestSpendableOutbound(t *testing.T) {
	t.Parallel()

	const reserve = btcutil.Amount(10_000)
	reserveMsat := lnwire.NewMSatFromSatoshis(reserve)

	always := func(lnwire.ChannelID) bool { return true }

	tests := []struct {
		name     string
		channels []*channeldb.OpenChannel
		active   func(lnwire.ChannelID) bool
		want     uint64
	}{
		{
			name:   "nothing at all",
			active: always,
		},
		{
			name: "one channel, less its reserve",
			channels: []*channeldb.OpenChannel{
				openChannel(1, reserveMsat+5_000, reserve, false),
			},
			active: always,
			want:   5_000,
		},
		{
			name: "a channel that is all reserve carries nothing",
			channels: []*channeldb.OpenChannel{
				openChannel(1, reserveMsat, reserve, false),
			},
			active: always,
		},
		{
			// Must not go negative and wrap into an enormous
			// balance, which would price the side as though it
			// were flush.
			name: "a channel below its reserve does not wrap",
			channels: []*channeldb.OpenChannel{
				openChannel(1, reserveMsat/2, reserve, false),
			},
			active: always,
		},
		{
			name: "a pending channel carries nothing yet",
			channels: []*channeldb.OpenChannel{
				openChannel(1, reserveMsat+5_000, reserve, true),
			},
			active: always,
			want:   0,
		},
		{
			name: "a channel whose peer is offline is skipped",
			channels: []*channeldb.OpenChannel{
				openChannel(1, reserveMsat+5_000, reserve, false),
			},
			active: func(lnwire.ChannelID) bool { return false },
		},
		{
			name: "several add up",
			channels: []*channeldb.OpenChannel{
				openChannel(1, reserveMsat+5_000, reserve, false),
				openChannel(2, reserveMsat+7_000, reserve, false),
				openChannel(3, reserveMsat, reserve, false),
			},
			active: always,
			want:   12_000,
		},
		{
			// This runs while quoting, so it must not panic.
			name:     "a nil channel is skipped",
			channels: []*channeldb.OpenChannel{nil},
			active:   always,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := spendableOutbound(test.channels, test.active)
			if got != test.want {
				t.Errorf("spendable %d msat, wanted %d", got,
					test.want)
			}
		})
	}
}

// A nil liveness check must not be read as "no channel is usable", which would
// report nothing to pay with and refuse every swap.
func TestSpendableOutboundWithoutALivenessCheck(t *testing.T) {
	t.Parallel()

	const reserve = btcutil.Amount(10_000)

	got := spendableOutbound([]*channeldb.OpenChannel{
		openChannel(1, lnwire.NewMSatFromSatoshis(reserve)+5_000,
			reserve, false),
	}, nil)

	if got != 5_000 {
		t.Errorf("spendable %d msat, wanted 5000", got)
	}
}
