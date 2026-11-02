//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// remoteRouting is the SHA256 node at height 1000 decoding the invoice as req,
// with two active channels of 100k sat spendable each.
func remoteRouting(req *lnrpc.PayReq) (*Remote, *fakeMain) {
	m := &fakeMain{
		info: &lnrpc.GetInfoResponse{
			SyncedToChain: true, BlockHeight: 1000,
		},
		payReq: req,
		channels: &lnrpc.ListChannelsResponse{
			Channels: []*lnrpc.Channel{{
				LocalBalance:        101_000,
				LocalChanReserveSat: 1_000,
			}, {
				LocalBalance:        101_000,
				LocalChanReserveSat: 1_000,
			}},
		},
	}

	return remote(m, nil, nil), m
}

// splittablePayReq is the SHA256 node's reading of a 150k sat invoice that
// declares multi-part payments and has a payment address.
func splittablePayReq() *lnrpc.PayReq {
	return &lnrpc.PayReq{
		Destination: "02aa",
		NumMsat:     150_000_000,
		CltvExpiry:  18,
		PaymentAddr: make([]byte, 32),
		Features: map[uint32]*lnrpc.Feature{
			uint32(lnwire.PaymentAddrRequired): {IsRequired: true},
			uint32(lnwire.MPPOptional):         {},
		},
	}
}

// partsOnly is a route search that finds a path for amounts up to most msat
// and answers noPath above it, as a destination reachable only by parts.
func partsOnly(most int64, noPath error) func(*lnrpc.QueryRoutesRequest) (
	*lnrpc.QueryRoutesResponse, error) {

	return func(in *lnrpc.QueryRoutesRequest) (*lnrpc.QueryRoutesResponse,
		error) {

		if in.AmtMsat > most {
			return nil, noPath
		}

		return &lnrpc.QueryRoutesResponse{Routes: []*lnrpc.Route{{
			TotalTimeLock: 1000 + 60, TotalFeesMsat: 100,
		}}}, nil
	}
}

func TestRemoteFindRoute(t *testing.T) {
	t.Parallel()

	const fee, limit = 3_000, 120
	noPath := status.Error(codes.Unknown, "unable to find a path to "+
		"destination")

	t.Run("found", func(t *testing.T) {
		t.Parallel()

		r, m := remoteRouting(splittablePayReq())
		m.routes = &lnrpc.QueryRoutesResponse{Routes: []*lnrpc.Route{{
			TotalTimeLock: 1000 + 98, TotalFeesMsat: 1_250,
		}}}
		got, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.NoError(t, err)
		require.Equal(t, node.Route{CLTV: 98, FeeMsat: 1_250}, got)
		require.Equal(t, "02aa", m.routesReq.PubKey)
		require.EqualValues(t, 150_000_000, m.routesReq.AmtMsat)
		require.EqualValues(t, 18, m.routesReq.FinalCltvDelta)
	})

	// Two 100k channels carry a 150k payout split, which no single path
	// does: routable, for the whole CLTV limit, once the smallest part
	// lnd's router would split it into (150k halved three times, 18,750
	// sat) finds a path, as the payout's first part then will.
	t.Run("split over two channels", func(t *testing.T) {
		t.Parallel()

		r, m := remoteRouting(splittablePayReq())
		m.routesFor = partsOnly(18_750_000, noPath)
		got, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.NoError(t, err)
		require.Equal(t, node.Route{CLTV: limit, FeeMsat: fee}, got)
		require.True(t, m.lastChannelsReq.GetActiveOnly())
		require.Len(t, m.routesAsked, 2)
		part := m.routesAsked[1]
		require.EqualValues(t, 18_750_000, part.AmtMsat)
		require.Equal(t, "02aa", part.PubKey)
		require.EqualValues(t, 18, part.FinalCltvDelta)
		require.EqualValues(t, fee, part.FeeLimit.GetFixedMsat())
		require.EqualValues(t, limit, part.CltvLimit)
		require.True(t, part.UseMissionControl)
	})

	// The balance would carry a split, but not even the smallest part
	// finds a path, as when mission control has written off the only
	// channel to the destination: the payout would fail at once.
	t.Run("no path for any part", func(t *testing.T) {
		t.Parallel()

		r, m := remoteRouting(splittablePayReq())
		m.routesErr = noPath
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.ErrorIs(t, err, node.ErrNoRoute)
		require.Len(t, m.routesAsked, 2)
	})

	t.Run("part search failed", func(t *testing.T) {
		t.Parallel()

		down := status.Error(codes.Unavailable, "connection refused")
		r, m := remoteRouting(splittablePayReq())
		m.routesFor = func(in *lnrpc.QueryRoutesRequest) (
			*lnrpc.QueryRoutesResponse, error) {

			if in.AmtMsat == 150_000_000 {
				return nil, noPath
			}

			return nil, down
		}
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.ErrorIs(t, err, down)
		require.NotErrorIs(t, err, node.ErrNoRoute)
	})

	// lnd's router does not split a payment under twice its smallest
	// part (20,000 sat), so no single path is final there.
	t.Run("too small to split", func(t *testing.T) {
		t.Parallel()

		req := splittablePayReq()
		req.NumMsat = 19_999_999
		r, m := remoteRouting(req)
		m.routesFor = partsOnly(0, noPath)
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.ErrorIs(t, err, node.ErrNoRoute)
		require.Len(t, m.routesAsked, 1)
		require.Nil(t, m.lastChannelsReq)

		req = splittablePayReq()
		req.NumMsat = 20_000_000
		r, m = remoteRouting(req)
		m.routesFor = partsOnly(10_000_000, noPath)
		_, err = r.FindRoute(context.Background(), "x", fee, limit)
		require.NoError(t, err)
	})

	t.Run("too little to split", func(t *testing.T) {
		t.Parallel()

		r, m := remoteRouting(splittablePayReq())
		m.routesFor = partsOnly(18_750_000, noPath)
		m.channels.Channels = m.channels.Channels[:1]
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.ErrorIs(t, err, node.ErrNoRoute)
	})

	t.Run("an invoice that cannot be split", func(t *testing.T) {
		t.Parallel()

		for _, change := range []func(*lnrpc.PayReq){
			func(p *lnrpc.PayReq) {
				delete(p.Features, uint32(lnwire.MPPOptional))
			},
			func(p *lnrpc.PayReq) { p.PaymentAddr = nil },
		} {
			req := splittablePayReq()
			change(req)
			r, m := remoteRouting(req)
			m.routesErr = noPath
			_, err := r.FindRoute(context.Background(), "x", fee,
				limit)
			require.ErrorIs(t, err, node.ErrNoRoute)
		}
	})

	t.Run("a refusal splitting does not help", func(t *testing.T) {
		t.Parallel()

		r, m := remoteRouting(splittablePayReq())
		m.routesErr = errors.New("destination does not support " +
			"feature 42")
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.ErrorIs(t, err, node.ErrNoRoute)
		require.Nil(t, m.lastChannelsReq)
	})

	t.Run("channels unread", func(t *testing.T) {
		t.Parallel()

		r, m := remoteRouting(splittablePayReq())
		m.routesFor = partsOnly(18_750_000, noPath)
		m.channelsErr = errors.New("permission denied")
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.ErrorIs(t, err, m.channelsErr)
		require.NotErrorIs(t, err, node.ErrNoRoute)
	})

	// Through blinded paths when the invoice has them, in the form lnd
	// takes: no destination key, final CLTV, hints or features of its own.
	t.Run("blinded paths", func(t *testing.T) {
		t.Parallel()

		req := splittablePayReq()
		req.RouteHints = []*lnrpc.RouteHint{{}}
		req.BlindedPaths = []*lnrpc.BlindedPaymentPath{{
			BlindedPath: &lnrpc.BlindedPath{},
		}}
		r, m := remoteRouting(req)
		m.routes = &lnrpc.QueryRoutesResponse{Routes: []*lnrpc.Route{{
			TotalTimeLock: 1000 + 98,
		}}}
		_, err := r.FindRoute(context.Background(), "x", fee, limit)
		require.NoError(t, err)
		require.Equal(t, req.BlindedPaths,
			m.routesReq.BlindedPaymentPaths)
		require.Empty(t, m.routesReq.PubKey)
		require.Zero(t, m.routesReq.FinalCltvDelta)
		require.Empty(t, m.routesReq.RouteHints)
		require.Empty(t, m.routesReq.DestFeatures)
	})
}

// A blinded invoice splits by its paths' features when they have any, as
// lnd's router reads them in place of the invoice's.
func TestRemoteSplitsByBlindedPathFeatures(t *testing.T) {
	t.Parallel()

	req := splittablePayReq()
	req.BlindedPaths = []*lnrpc.BlindedPaymentPath{{}}
	require.True(t, remoteSplits(req), "paths without features of their "+
		"own split by the invoice's")

	req.BlindedPaths[0].Features = []lnrpc.FeatureBit{
		lnrpc.FeatureBit(lnwire.TLVOnionPayloadRequired),
	}
	require.False(t, remoteSplits(req))

	req.BlindedPaths[0].Features = []lnrpc.FeatureBit{
		lnrpc.FeatureBit(lnwire.MPPRequired),
	}
	req.Features = nil
	require.True(t, remoteSplits(req))
}
