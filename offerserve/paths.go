package offerserve

import (
	"github.com/lightningnetwork/lnd/lnwire"
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

// PeerChannel is one of this node's channels as an offer invoice's paths see
// it: whom it is with, whether its link is up, and how much the peer could
// send this node over it now (their balance less the reserve they must keep).
type PeerChannel struct {
	Peer    route.Vertex
	Active  bool
	Inbound lnwire.MilliSatoshi
}

// UnreceivablePeers returns the peers through none of whose channels this node
// could be paid amt right now: every channel inactive (the peer is offline)
// or short of inbound. A blinded path whose last hop is such a channel is a
// path a payer will spend time and attempts on for nothing. The graph cannot
// say so: an offline peer's last channel update still reads as enabled, and
// balances are not in it. Only this node knows, so the paths it hands out
// should leave those peers out.
func UnreceivablePeers(chans []PeerChannel,
	amt lnwire.MilliSatoshi) []route.Vertex {

	usable := make(map[route.Vertex]bool)
	var order []route.Vertex
	for _, c := range chans {
		if _, seen := usable[c.Peer]; !seen {
			order = append(order, c.Peer)
			usable[c.Peer] = false
		}
		if c.Active && c.Inbound >= amt {
			usable[c.Peer] = true
		}
	}

	var out []route.Vertex
	for _, p := range order {
		if !usable[p] {
			out = append(out, p)
		}
	}

	return out
}
