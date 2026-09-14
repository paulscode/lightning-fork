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
