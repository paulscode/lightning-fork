package offerserve

import (
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

// FindPathsWithFallback asks find for the blinded routes an invoice's paths
// are built from, under the node's restrictions. When those ask for a path
// through a peer and no peer can start one, because every peer's only channel
// is the one to this node or none of them signals route blinding, it asks
// again for a path that starts at this node, which always exists, so that the
// invoice can be issued. The second result is whether that fallback was used.
func FindPathsWithFallback(
	find func(*routing.BlindedPathRestrictions) ([]*route.Route, error),
	restrictions *routing.BlindedPathRestrictions) ([]*route.Route, bool,
	error) {

	routes, err := find(restrictions)
	if err != nil || len(routes) > 0 ||
		restrictions.MinDistanceFromIntroNode == 0 {

		return routes, false, err
	}

	direct := *restrictions
	direct.MinDistanceFromIntroNode = 0
	routes, err = find(&direct)

	return routes, err == nil, err
}
