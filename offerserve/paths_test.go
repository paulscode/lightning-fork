package offerserve

import (
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestFindPathsWithFallback checks that a path starting at this node is only
// asked for when a path through a peer was asked for and none was found.
func TestFindPathsWithFallback(t *testing.T) {
	t.Parallel()

	restrictions := &routing.BlindedPathRestrictions{
		MinDistanceFromIntroNode: 1,
		NumHops:                  2,
		MaxNumPaths:              3,
		NodeOmissionSet:          fn.NewSet[route.Vertex](),
	}
	peerRoute := []*route.Route{{}}

	// A path through a peer: used as is.
	var asked []routing.BlindedPathRestrictions
	find := func(r *routing.BlindedPathRestrictions) ([]*route.Route,
		error) {

		asked = append(asked, *r)
		if r.MinDistanceFromIntroNode > 0 {
			return peerRoute, nil
		}

		return []*route.Route{{}, {}}, nil
	}
	routes, fellBack, err := FindPathsWithFallback(find, restrictions)
	require.NoError(t, err)
	require.False(t, fellBack)
	require.Len(t, routes, 1)
	require.Len(t, asked, 1)

	// No peer can start a path: asked again for one starting here, with
	// the other restrictions kept.
	asked = nil
	find = func(r *routing.BlindedPathRestrictions) ([]*route.Route,
		error) {

		asked = append(asked, *r)
		if r.MinDistanceFromIntroNode > 0 {
			return nil, nil
		}

		return []*route.Route{{}, {}}, nil
	}
	routes, fellBack, err = FindPathsWithFallback(find, restrictions)
	require.NoError(t, err)
	require.True(t, fellBack)
	require.Len(t, routes, 2)
	require.Len(t, asked, 2)
	require.Equal(t, uint8(0), asked[1].MinDistanceFromIntroNode)
	require.Equal(t, uint8(2), asked[1].NumHops)
	require.Equal(t, uint8(3), asked[1].MaxNumPaths)
	require.Equal(t, uint8(1), restrictions.MinDistanceFromIntroNode,
		"the caller's restrictions are left alone")

	// Path finding failing is reported, not retried.
	asked = nil
	boom := errors.New("graph on fire")
	find = func(r *routing.BlindedPathRestrictions) ([]*route.Route,
		error) {

		asked = append(asked, *r)

		return nil, boom
	}
	_, fellBack, err = FindPathsWithFallback(find, restrictions)
	require.ErrorIs(t, err, boom)
	require.False(t, fellBack)
	require.Len(t, asked, 1)

	// Already asking for a path starting here: nothing to fall back to.
	asked = nil
	direct := *restrictions
	direct.MinDistanceFromIntroNode = 0
	find = func(r *routing.BlindedPathRestrictions) ([]*route.Route,
		error) {

		asked = append(asked, *r)

		return nil, nil
	}
	routes, fellBack, err = FindPathsWithFallback(find, &direct)
	require.NoError(t, err)
	require.False(t, fellBack)
	require.Empty(t, routes)
	require.Len(t, asked, 1)
}

// TestUnreceivablePeers: a peer is left out of an invoice's paths when none of
// its channels with this node is up with enough inbound for the amount, as on
// mainnet, where a path through a peer offline for days was handed out and
// payers failed on it.
func TestUnreceivablePeers(t *testing.T) {
	t.Parallel()

	var offline, drained, good, mixed route.Vertex
	offline[0], drained[0], good[0], mixed[0] = 1, 2, 3, 4
	chans := []PeerChannel{
		{Peer: offline, Active: false, Inbound: 5_000_000_000},
		{Peer: offline, Active: false, Inbound: 1_000_000_000},
		{Peer: drained, Active: true, Inbound: 200_000},
		{Peer: good, Active: true, Inbound: 7_000_000_000},
		// One channel of two can take it: the peer is usable.
		{Peer: mixed, Active: true, Inbound: 0},
		{Peer: mixed, Active: true, Inbound: 2_000_000},
	}

	require.Equal(t, []route.Vertex{offline, drained},
		UnreceivablePeers(chans, 1_000_000))
	require.Equal(t, []route.Vertex{offline},
		UnreceivablePeers(chans, 100_000), "a small amount fits")
	require.Empty(t, UnreceivablePeers(nil, 1_000_000))
}
