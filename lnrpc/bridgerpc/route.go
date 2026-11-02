//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/paulscode/lightning-fork-bridge/node"
)

// routeQuery is the route search a quote and the pay gate ask a paying node
// for: to the invoice's destination, for its amount and final CLTV, through
// its route hints (a route to a private destination exists only through
// them), for its features, within the routing budget and the CLTV limit, with
// mission control's view of which channels have been failing. The same for
// both nodes, so the two directions find routes by one rule.
//
// An invoice with blinded paths is searched for through them, as a payment to
// it is made: lnd takes the destination, its features and the final CLTV from
// the paths then, and refuses a request that names them as well.
func routeQuery(dest string, amtMsat int64, finalCLTV int32,
	hints []*lnrpc.RouteHint, features []lnrpc.FeatureBit,
	blinded []*lnrpc.BlindedPaymentPath, maxFeeMsat uint64,
	cltvLimit uint32) *lnrpc.QueryRoutesRequest {

	if len(blinded) > 0 {
		dest, finalCLTV, hints, features = "", 0, nil, nil
	}

	return &lnrpc.QueryRoutesRequest{
		PubKey:         dest,
		AmtMsat:        amtMsat,
		FinalCltvDelta: finalCLTV,
		FeeLimit: &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_FixedMsat{
				FixedMsat: int64(maxFeeMsat),
			},
		},
		CltvLimit:           cltvLimit,
		RouteHints:          hints,
		DestFeatures:        features,
		BlindedPaymentPaths: blinded,
		UseMissionControl:   true,
	}
}

// errNoSinglePath is a route search that found no single path to the
// destination: lnd's own "unable to find a path", or no route listed. Unlike
// its other refusals, it does not mean the payment cannot go, since a payment
// may be split over several paths and the search finds one.
var errNoSinglePath = fmt.Errorf("%w: no single path carries it",
	node.ErrNoRoute)

// splitRoute is the answer for an invoice with no single path that a payment
// to it may still carry by splitting it, as the payout does (up to lnd's
// default sixteen parts) when the invoice allows it.
//
// The route search finds single paths only, so it cannot say more than two
// things. What a split payment needs first is outbound to send it from: the
// paying node's spendable balance, over its active channels and less their
// reserves (the figure its Balance reads), has to cover the amount and the
// routing budget. And its first part has to find a path: the router searches
// for the whole amount, then halves it while no path is found, and gives up
// when a part would fall under its smallest (see smallestPart). A search for
// that smallest part, with mission control as the payout will see it, is the
// router's own last try; without a path for it the payout fails at once with
// no route, as it did for a quote taken on the balance alone after mission
// control had written off the only way to the destination.
//
// When both hold, the payment is taken as routable, and its CLTV as the whole
// limit, since the parts may take longer paths than the one found: the payer
// is asked to hold for the longest the payout may lock, which is the safe
// side.
func splitRoute(ctx context.Context, noPath error,
	balance func(context.Context) (uint64, error),
	search func(context.Context, uint64) (node.Route, error), amtMsat,
	maxFeeMsat uint64, cltvLimit uint32) (node.Route, error) {

	part, ok := smallestPart(amtMsat)
	if !ok {
		return node.Route{}, fmt.Errorf("%w; lnd does not split a "+
			"payment under %d msat", noPath,
			2*uint64(routing.DefaultShardMinAmt))
	}

	held, err := balance(ctx)
	if err != nil {
		return node.Route{}, fmt.Errorf("no single path (%v), and "+
			"reading what a split payment could leave from: %w",
			noPath, err)
	}
	need := amtMsat + maxFeeMsat
	if need < amtMsat || held < need {
		return node.Route{}, fmt.Errorf("%w; %d msat to send over "+
			"several, with %d msat spendable", noPath, need, held)
	}

	_, err = search(ctx, part)
	switch {
	case errors.Is(err, node.ErrNoRoute):
		return node.Route{}, fmt.Errorf("%w; nor for a part of %d "+
			"msat, the smallest a split payment sends: %w", noPath,
			part, err)

	case err != nil:
		return node.Route{}, fmt.Errorf("no single path (%v), and "+
			"searching for a part of %d msat: %w", noPath, part,
			err)
	}

	return node.Route{CLTV: cltvLimit, FeeMsat: maxFeeMsat}, nil
}

// smallestPart is the least lnd's router sends over one path when it splits a
// payment of amtMsat, or false when it does not split one that small. It
// halves a part that finds no path and searches again, until the half would
// fall under routing.DefaultShardMinAmt (10,000 sat), so a payment under twice
// that is never split. The bridge's payouts set no shard size of their own on
// either node, and the SHA256 node is a stock lnd, which splits by the same
// rule.
func smallestPart(amtMsat uint64) (uint64, bool) {
	least := uint64(routing.DefaultShardMinAmt)
	part := amtMsat / 2
	if part < least {
		return 0, false
	}
	for part/2 >= least {
		part /= 2
	}

	return part, true
}

// routeFrom reads a route search's answer: the route's CLTV counted from the
// paying node's height, as the payment's limit is, and its fee; or
// node.ErrNoRoute when there is none.
func routeFrom(res *lnrpc.QueryRoutesResponse, err error,
	height int32) (node.Route, error) {

	if err != nil {
		// lnd answers a search that found nothing with an error, not
		// an empty list: "unable to find a path to destination",
		// which is about one path (see errNoSinglePath). The other
		// ways lnd says the payment cannot go (too little to send, a
		// destination needing a feature it lacks) are as final for
		// this quote.
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "unable to find a path") {
			return node.Route{}, fmt.Errorf("%w: %v",
				errNoSinglePath, err)
		}
		for _, no := range []string{"no route", "insufficient",
			"not enough", "feature"} {

			if strings.Contains(msg, no) {
				return node.Route{}, fmt.Errorf("%w: %v",
					node.ErrNoRoute, err)
			}
		}

		return node.Route{}, fmt.Errorf("finding a route: %w", err)
	}
	if len(res.GetRoutes()) == 0 {
		return node.Route{}, errNoSinglePath
	}

	route := res.GetRoutes()[0]
	lock := int64(route.GetTotalTimeLock()) - int64(height)
	if lock < 0 || lock > math.MaxInt32 {
		return node.Route{}, fmt.Errorf("a route locking until %d at "+
			"height %d", route.GetTotalTimeLock(), height)
	}

	return node.Route{
		CLTV: uint32(lock), FeeMsat: uint64(route.GetTotalFeesMsat()),
	}, nil
}
