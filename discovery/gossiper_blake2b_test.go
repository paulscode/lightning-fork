package discovery

import (
	"sync/atomic"
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// A funding output from before the proof of work changed exists for nodes that
// did not upgrade too, and this node cannot see where it is spent, so a
// channel announced against one would sit in the graph forever.
//
// This is the second half of how the two chains are kept apart. They share a
// chain_hash by design, so the chain check in the announcement handler does
// not separate them: channel_type does that for channels, and this does it for
// gossip. Together they are what replaced giving this chain a chain_hash of its
// own.
func TestChannelAnnouncementBelowActivationIsIgnored(t *testing.T) {
	t.Parallel()

	const activation = 961_640

	ctx, _ := createTestCtx(t, activation+10, false)
	ctx.gossiper.cfg.MinAnnouncementHeight = activation

	ann, err := ctx.createRemoteChannelAnnouncement(
		0, withFundingTxPrep(fundingTxPrepTypeNone),
	)
	require.NoError(t, err)

	// Announced against an output funded one block before the change.
	ann.ShortChannelID = lnwire.ShortChannelID{
		BlockHeight: activation - 1, TxIndex: 0, TxPosition: 0,
	}

	err = mustProcess(t, ctx.gossiper.ProcessRemoteAnnouncement(
		t.Context(), ann, &mockPeer{remoteKeyPriv1.PubKey(), nil, nil, atomic.Bool{}},
	))

	require.Error(t, err, "a channel funded before the proof of work "+
		"changed was accepted into the graph; its spend may happen "+
		"where this node cannot see it, so it would never leave")
	require.Contains(t, err.Error(), "before the proof of work changed")
}

// And one funded after it is ordinary, so the rule must not reject everything.
func TestChannelAnnouncementAtOrAboveActivationIsProcessed(t *testing.T) {
	t.Parallel()

	const activation = 961_640

	ctx, _ := createTestCtx(t, activation+10, false)
	ctx.gossiper.cfg.MinAnnouncementHeight = activation

	ann, err := ctx.createRemoteChannelAnnouncement(
		0, withFundingTxPrep(fundingTxPrepTypeNone),
	)
	require.NoError(t, err)

	ann.ShortChannelID = lnwire.ShortChannelID{
		BlockHeight: activation, TxIndex: 0, TxPosition: 0,
	}

	err = mustProcess(t, ctx.gossiper.ProcessRemoteAnnouncement(
		t.Context(), ann, &mockPeer{remoteKeyPriv1.PubKey(), nil, nil, atomic.Bool{}},
	))

	// It may still be rejected for unrelated reasons in this harness; what
	// must not happen is a rejection by the height rule.
	if err != nil {
		require.NotContains(t, err.Error(),
			"before the proof of work changed",
			"a channel funded at the activation height was "+
				"rejected by the height rule")
	}
}

// Zero disables the rule, so a build for a network without an activation
// height does not reject every announcement.
func TestZeroActivationHeightDisablesTheRule(t *testing.T) {
	t.Parallel()

	ctx, _ := createTestCtx(t, 100, false)
	ctx.gossiper.cfg.MinAnnouncementHeight = 0

	ann, err := ctx.createRemoteChannelAnnouncement(
		0, withFundingTxPrep(fundingTxPrepTypeNone),
	)
	require.NoError(t, err)

	ann.ShortChannelID = lnwire.ShortChannelID{
		BlockHeight: 1, TxIndex: 0, TxPosition: 0,
	}

	err = mustProcess(t, ctx.gossiper.ProcessRemoteAnnouncement(
		t.Context(), ann, &mockPeer{remoteKeyPriv1.PubKey(), nil, nil, atomic.Bool{}},
	))

	if err != nil {
		require.NotContains(t, err.Error(),
			"before the proof of work changed",
			"the height rule fired with no activation height set")
	}
}
