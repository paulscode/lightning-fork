//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// routedInvoice is a BLAKE2b invoice to a private destination behind one route
// hint, with a small final CLTV delta.
func routedInvoice(t *testing.T, amt lnwire.MilliSatoshi,
	opts ...func(*zpay32.Invoice)) (*zpay32.Invoice, *btcec.PublicKey) {

	t.Helper()

	dest, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hop, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	all := append([]func(*zpay32.Invoice){
		zpay32.Amount(amt),
		zpay32.Description("payout"),
		zpay32.Destination(dest.PubKey()),
		zpay32.CLTVExpiry(18),
		zpay32.RouteHint([]zpay32.HopHint{{
			NodeID: hop.PubKey(), ChannelID: 42,
			FeeBaseMSat: 1000, CLTVExpiryDelta: 40,
		}}),
		zpay32.Features(lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.Blake2bRequired,
				lnwire.TLVOnionPayloadRequired,
				lnwire.PaymentAddrRequired,
			), lnwire.Features,
		)),
	}, opts...)
	inv, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, hash8(5), time.Now(), all...,
	)
	require.NoError(t, err)

	return inv, hop.PubKey()
}

// localRouting is this node at height 1000 decoding inv, with a route search
// that answers as search does and records what it was asked.
func localRouting(inv *zpay32.Invoice,
	search func(*lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse,
		error)) (*Local, *[]*lnrpc.QueryRoutesRequest) {

	asked := &[]*lnrpc.QueryRoutesRequest{}
	f := &fakeNode{synced: true, height: 1000}
	deps := f.deps()
	deps.DecodeInvoice = func(context.Context, string) (*zpay32.Invoice,
		error) {

		return inv, nil
	}
	deps.QueryRoutes = func(_ context.Context,
		req *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse,
		error) {

		*asked = append(*asked, req)

		return search(req)
	}

	return NewLocal(deps), asked
}

func TestLocalFindRoute(t *testing.T) {
	t.Parallel()

	inv, _ := routedInvoice(t, 250_000)

	t.Run("found", func(t *testing.T) {
		t.Parallel()

		l, asked := localRouting(inv, func(*lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error) {

			return &lnrpc.QueryRoutesResponse{Routes: []*lnrpc.Route{{
				TotalTimeLock: 1000 + 98, TotalFeesMsat: 1_250,
			}}}, nil
		})
		got, err := l.FindRoute(context.Background(), "lnbcrt1x",
			3_000, 120)
		require.NoError(t, err)
		require.Equal(t, node.Route{CLTV: 98, FeeMsat: 1_250}, got)

		// What was asked is the invoice's own search, within the
		// budgets, with mission control.
		require.Len(t, *asked, 1)
		req := (*asked)[0]
		require.Equal(t, hexKey(inv.Destination), req.PubKey)
		require.EqualValues(t, 250_000, req.AmtMsat)
		require.EqualValues(t, 18, req.FinalCltvDelta)
		require.EqualValues(t, 120, req.CltvLimit)
		require.EqualValues(t, 3_000, req.FeeLimit.GetFixedMsat())
		require.True(t, req.UseMissionControl)
		require.Len(t, req.RouteHints, 1)
		require.EqualValues(t, 42, req.RouteHints[0].HopHints[0].ChanId)
		require.Contains(t, req.DestFeatures,
			lnrpc.FeatureBit(lnwire.Blake2bRequired))
	})

	t.Run("none", func(t *testing.T) {
		t.Parallel()

		for _, answer := range []error{
			status.Error(codes.Unknown, "unable to find a path to "+
				"destination"),
			errors.New("insufficient local balance"),
			errors.New("destination does not support feature 42"),
		} {
			l, _ := localRouting(inv, func(*lnrpc.QueryRoutesRequest) (
				*lnrpc.QueryRoutesResponse, error) {

				return nil, answer
			})
			_, err := l.FindRoute(context.Background(), "x", 3_000,
				120)
			require.ErrorIs(t, err, node.ErrNoRoute, "%v", answer)
		}

		l, _ := localRouting(inv, func(*lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error) {

			return &lnrpc.QueryRoutesResponse{}, nil
		})
		_, err := l.FindRoute(context.Background(), "x", 3_000, 120)
		require.ErrorIs(t, err, node.ErrNoRoute)
	})

	t.Run("an error that is not an answer", func(t *testing.T) {
		t.Parallel()

		l, _ := localRouting(inv, func(*lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error) {

			return nil, errors.New("database is closed")
		})
		_, err := l.FindRoute(context.Background(), "x", 3_000, 120)
		require.Error(t, err)
		require.NotErrorIs(t, err, node.ErrNoRoute)
	})

	t.Run("no route search", func(t *testing.T) {
		t.Parallel()

		l, _ := localRouting(inv, nil)
		l.deps.QueryRoutes = nil
		_, err := l.FindRoute(context.Background(), "x", 3_000, 120)
		require.ErrorIs(t, err, ErrNoDeps)
	})

	t.Run("a node that has not caught up", func(t *testing.T) {
		t.Parallel()

		f := &fakeNode{synced: false, height: 1000}
		deps := f.deps()
		deps.DecodeInvoice = func(context.Context, string) (
			*zpay32.Invoice, error) {

			return inv, nil
		}
		deps.QueryRoutes = func(context.Context,
			*lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse,
			error) {

			t.Error("searched for a route from a stale height")

			return nil, nil
		}
		_, err := NewLocal(deps).FindRoute(context.Background(), "x",
			3_000, 120)
		require.ErrorIs(t, err, ErrNotSynced)
	})
}

func hexKey(k *btcec.PublicKey) string {
	return hex.EncodeToString(k.SerializeCompressed())
}

// The request Local builds is one lnd's own QueryRoutes reads as meant: the
// destination, the amount, the hint's channel, the final CLTV taken out of
// the limit, and the fee limit.
func TestLocalFindRouteThroughLndsParser(t *testing.T) {
	t.Parallel()

	inv, hop := routedInvoice(t, 250_000)
	self, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var got *routing.RouteRequest
	backend := &routerrpc.RouterBackend{
		SelfNode:              route.NewVertex(self.PubKey()),
		MaxTotalTimelock:      2016,
		DefaultFinalCltvDelta: 40,
		ActiveNetParams:       &chaincfg.RegressionNetParams,
		FetchChannelCapacity: func(uint64) (btcutil.Amount, error) {
			return 1_000_000, nil
		},
		FindRoute: func(req *routing.RouteRequest) (*route.Route,
			float64, error) {

			got = req

			return nil, 0, errors.New("unable to find a path to " +
				"destination")
		},
	}

	l, _ := localRouting(inv, func(req *lnrpc.QueryRoutesRequest) (
		*lnrpc.QueryRoutesResponse, error) {

		return backend.QueryRoutes(context.Background(), req)
	})
	_, err = l.FindRoute(context.Background(), "x", 3_000, 120)
	require.ErrorIs(t, err, node.ErrNoRoute)

	require.NotNil(t, got, "lnd refused the request before searching")
	require.Equal(t, route.NewVertex(inv.Destination), got.Target)
	require.EqualValues(t, 250_000, got.Amount)
	require.EqualValues(t, 18, got.FinalExpiry)
	require.EqualValues(t, 120-18, got.Restrictions.CltvLimit)
	require.EqualValues(t, 3_000, got.Restrictions.FeeLimit)
	require.True(t, got.Restrictions.DestFeatures.HasFeature(
		lnwire.Blake2bRequired))
	require.Contains(t, got.RouteHints, route.NewVertex(hop))
}

// A toBLAKE2b quote asks this node for a route before creating the hold
// invoice on the SHA256 node, as toSHA256 asks that node: with none, it is
// refused as no_route and nothing is held.
func TestAToBLAKE2bQuoteWithNoRouteHoldsNothing(t *testing.T) {
	t.Parallel()

	srv, svc := measuredServer(t, 100_000_000_000)
	svc.ctx = context.Background()

	searched := 0
	for _, sd := range svc.sides {
		if sd.name != "toBLAKE2b" {
			continue
		}
		p := sd.quoter.Policy
		require.Less(t, p.MinSwapMsat, p.MaxSwapMsat)
		inv, _ := routedInvoice(t, lnwire.MilliSatoshi(
			p.MinSwapMsat+(p.MaxSwapMsat-p.MinSwapMsat)/2))
		l := sd.out.(*Local)
		l.deps.DecodeInvoice = func(context.Context, string) (
			*zpay32.Invoice, error) {

			return inv, nil
		}
		l.deps.QueryRoutes = func(context.Context,
			*lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse,
			error) {

			searched++

			return nil, errors.New("unable to find a path to " +
				"destination")
		}
	}
	held := &fakeInvoices{}
	svc.remote.invoices = held
	for _, sd := range svc.sides {
		if r, ok := sd.in.(*Remote); ok {
			r.invoices = held
		}
	}

	_, err := srv.Quote(context.Background(), &QuoteRequest{
		Invoice: "lnbcrt1toblake2b",
	})
	require.True(t, strings.HasPrefix(status.Convert(err).Message(),
		string(quote.CodeNoRoute)+": "), "%v", err)
	require.Equal(t, 1, searched)
	pending, err := svc.journal.Pending(context.Background())
	require.NoError(t, err)
	require.Empty(t, pending, "a quote with no route recorded a swap")
}

// splittableInvoice is a BLAKE2b invoice a payment may be split for: it
// declares multi-part payments and has a payment address. With blinded set,
// the destination is reached through one blinded path of two hops instead of
// a route hint.
func splittableInvoice(t *testing.T, amt lnwire.MilliSatoshi,
	blinded bool) *zpay32.Invoice {

	t.Helper()

	key := func() *btcec.PublicKey {
		k, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		return k.PubKey()
	}
	opts := []func(*zpay32.Invoice){
		zpay32.Amount(amt),
		zpay32.Description("payout"),
		zpay32.Destination(key()),
		zpay32.PaymentAddr(hash8(9)),
		zpay32.Features(lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.Blake2bRequired,
				lnwire.TLVOnionPayloadRequired,
				lnwire.PaymentAddrRequired,
				lnwire.MPPOptional,
			), lnwire.Features,
		)),
	}
	if blinded {
		noFeatures := lnwire.EmptyFeatureVector()
		opts = append(opts, zpay32.WithBlindedPaymentPath(
			&zpay32.BlindedPaymentPath{
				FeeBaseMsat:                 1000,
				CltvExpiryDelta:             80,
				HTLCMinMsat:                 1,
				HTLCMaxMsat:                 uint64(amt),
				Features:                    noFeatures,
				FirstEphemeralBlindingPoint: key(),
				Hops: []*sphinx.BlindedHopInfo{
					{
						BlindedNodePub: key(),
						CipherText:     []byte{1, 2, 3},
					},
					{
						BlindedNodePub: key(),
						CipherText:     []byte{4, 5, 6},
					},
				},
			},
		))
	} else {
		opts = append(opts, zpay32.CLTVExpiry(18))
	}

	inv, err := zpay32.NewInvoice(
		&chaincfg.RegressionNetParams, hash8(6), time.Now(), opts...,
	)
	require.NoError(t, err)

	return inv
}

// A payout the route search finds no single path for, but which the payment
// may split (two 100k channels and a 150k payout), is routable when this
// node's balance can send it and its fee budget, with the whole CLTV limit as
// its route's CLTV; it was refused as no_route, though the payment could go.
func TestLocalFindRouteSplits(t *testing.T) {
	t.Parallel()

	const amt, fee, limit = 150_000_000, 3_000, 120
	noPath := func(*lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse,
		error) {

		return nil, status.Error(codes.Unknown, "unable to find a "+
			"path to destination")
	}
	// A path for the smallest part lnd's router would split 150k into
	// (halved three times, 18,750 sat), and none for more.
	parts := func(req *lnrpc.QueryRoutesRequest) (
		*lnrpc.QueryRoutesResponse, error) {

		if req.AmtMsat > 18_750_000 {
			return noPath(req)
		}

		return &lnrpc.QueryRoutesResponse{Routes: []*lnrpc.Route{{
			TotalTimeLock: 1000 + 60, TotalFeesMsat: 100,
		}}}, nil
	}
	split := splittableInvoice(t, amt, false)
	single, _ := routedInvoice(t, amt)
	small := splittableInvoice(t, 19_999_999, false)

	for _, tc := range []struct {
		name   string
		inv    *zpay32.Invoice
		search func(*lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error)
		balance uint64
		balErr  error
		want    node.Route
		noRoute bool
		asked   int
	}{{
		name: "two channels carry it", inv: split, search: parts,
		balance: 200_000_000, asked: 2,
		want: node.Route{CLTV: limit, FeeMsat: fee},
	}, {
		name: "exactly enough", inv: split, search: parts,
		balance: amt + fee,
		want:    node.Route{CLTV: limit, FeeMsat: fee},
	}, {
		name: "not enough with the fee budget", inv: split,
		search: parts, balance: amt + fee - 1, noRoute: true,
	}, {
		// Mission control has written off the only way there: the
		// balance would carry a split, but no part finds a path, and
		// the payout would fail at once.
		name: "no path for any part", inv: split, search: noPath,
		balance: 200_000_000, noRoute: true, asked: 2,
	}, {
		// lnd's router does not split under 20,000 sat.
		name: "too small to split", inv: small, search: parts,
		balance: 200_000_000, noRoute: true, asked: 1,
	}, {
		name: "an invoice that cannot be split", inv: single,
		search: parts, balance: 200_000_000, noRoute: true,
	}, {
		name: "empty answer", inv: split, balance: 200_000_000,
		search: func(req *lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error) {

			if req.AmtMsat == amt {
				return &lnrpc.QueryRoutesResponse{}, nil
			}

			return parts(req)
		},
		want: node.Route{CLTV: limit, FeeMsat: fee},
	}, {
		name: "a refusal splitting does not help", inv: split,
		balance: 200_000_000, noRoute: true,
		search: func(*lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error) {

			return nil, errors.New("destination does not support " +
				"feature 42")
		},
	}, {
		name: "balance unread", inv: split, search: parts,
		balErr: errors.New("database is closed"),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l, asked := localRouting(tc.inv, tc.search)
			l.deps.ChannelBalance = func(context.Context) (uint64,
				error) {

				return tc.balance, tc.balErr
			}
			got, err := l.FindRoute(context.Background(), "x", fee,
				limit)
			switch {
			case tc.noRoute:
				require.ErrorIs(t, err, node.ErrNoRoute)

			case tc.balErr != nil:
				require.ErrorIs(t, err, tc.balErr)
				require.NotErrorIs(t, err, node.ErrNoRoute)

			default:
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
			if tc.asked > 0 {
				require.Len(t, *asked, tc.asked)
			}
		})
	}
}

// An invoice with blinded paths is searched for through them, in the form
// lnd's QueryRoutes takes: the paths, and no destination key, final CLTV,
// hints or features of its own. Without them the search went to the
// invoice's signing key, which a blinded destination does not route as.
func TestLocalFindRouteBlindedPaths(t *testing.T) {
	t.Parallel()

	inv := splittableInvoice(t, 250_000, true)
	self, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	var got *routing.RouteRequest
	backend := &routerrpc.RouterBackend{
		SelfNode:              route.NewVertex(self.PubKey()),
		MaxTotalTimelock:      2016,
		DefaultFinalCltvDelta: 40,
		ActiveNetParams:       &chaincfg.RegressionNetParams,
		FetchChannelCapacity: func(uint64) (btcutil.Amount, error) {
			return 1_000_000, nil
		},
		FindRoute: func(req *routing.RouteRequest) (*route.Route,
			float64, error) {

			got = req

			return nil, 0, errors.New("unable to find a path to " +
				"destination")
		},
	}

	l, asked := localRouting(inv, func(req *lnrpc.QueryRoutesRequest) (
		*lnrpc.QueryRoutesResponse, error) {

		return backend.QueryRoutes(context.Background(), req)
	})
	_, err = l.FindRoute(context.Background(), "x", 3_000, 200)
	require.ErrorIs(t, err, node.ErrNoRoute)

	require.Len(t, *asked, 1)
	req := (*asked)[0]
	require.Len(t, req.BlindedPaymentPaths, 1)
	require.Empty(t, req.PubKey)
	require.Empty(t, req.RouteHints)
	require.Zero(t, req.FinalCltvDelta)

	require.NotNil(t, got, "lnd refused the request before searching")
	require.NotNil(t, got.BlindedPathSet)
	require.EqualValues(t, 250_000, got.Amount)
	require.EqualValues(t, 3_000, got.Restrictions.FeeLimit)
}

// A blinded invoice splits by its paths' features when they have any, as
// lnd's router reads them in place of the invoice's.
func TestLocalSplitsByBlindedPathFeatures(t *testing.T) {
	t.Parallel()

	inv := splittableInvoice(t, 250_000, true)
	require.True(t, localSplits(inv), "paths without features of their "+
		"own split by the invoice's")

	inv.BlindedPaymentPaths[0].Features = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.TLVOnionPayloadRequired),
		lnwire.Features,
	)
	require.False(t, localSplits(inv))

	inv.BlindedPaymentPaths[0].Features = lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(lnwire.MPPOptional),
		lnwire.Features,
	)
	inv.Features = lnwire.EmptyFeatureVector()
	require.True(t, localSplits(inv))
}
