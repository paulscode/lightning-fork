//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// What gRPC and lnd actually say when a call never reached a handler, and
// what they say when it may have. Only the first may be called unsent: the
// second, read as unsent, could have a swap paid twice.
func TestNeverDelivered(t *testing.T) {
	t.Parallel()

	unsent := []error{
		// gRPC with no transport, as the lab saw with the node down.
		status.Error(codes.Unavailable, `connection error: desc = `+
			`"transport: Error while dialing: dial tcp `+
			`127.0.0.1:10019: connect: connection refused"`),
		status.Error(codes.Unavailable, "name resolver error: produced "+
			"zero addresses"),
		// lnd before the router is registered.
		status.Error(codes.Unimplemented, "unknown service "+
			"routerrpc.Router"),
		// lnd's interceptor, before any handler.
		status.Error(codes.Unknown, rpcperms.ErrWaitingToStart.Error()),
		status.Error(codes.Unknown, rpcperms.ErrWalletLocked.Error()),
		status.Error(codes.Unknown, rpcperms.ErrNoWallet.Error()),
		status.Error(codes.Unknown, rpcperms.ErrRPCStarting.Error()),
		status.Error(codes.Unknown, "verification failed: signature "+
			"mismatch after caveat verification"),
		status.Error(codes.Unknown, "permission denied"),
	}
	for _, err := range unsent {
		require.True(t, neverDelivered(err), "%v", err)
	}

	ambiguous := []error{
		status.Error(codes.Unavailable, "transport is closing"),
		status.Error(codes.Unavailable, "error reading from server: EOF"),
		status.Error(codes.Unavailable, "connection error: desc = "+
			`"error reading server preface: read: connection reset"`),
		status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
		status.Error(codes.Canceled, "context canceled"),
		status.Error(codes.Unknown, "server is still in the process of "+
			"starting"),
		status.Error(codes.Unimplemented, "unknown method SendPaymentV2"),
		errors.New("connection refused"), // not a gRPC status
		context.DeadlineExceeded,
		nil,
	}
	for _, err := range ambiguous {
		require.False(t, neverDelivered(err), "%v", err)
	}
}

func TestRemotePaySaysWhenNothingWasSent(t *testing.T) {
	t.Parallel()

	req := node.PayRequest{
		Invoice: "lnbc1payme", CLTVLimit: 80, Timeout: time.Minute,
	}
	dial := status.Error(codes.Unavailable, `connection error: desc = `+
		`"transport: Error while dialing: dial tcp 127.0.0.1:10019: `+
		`connect: connection refused"`)

	// The stream could not be opened at all.
	_, err := remote(nil, nil, &fakeRouter{sendErr: dial}).Pay(
		context.Background(), req)
	require.ErrorIs(t, err, node.ErrNotSent)

	// It opened and lnd refused before saying anything about the payment.
	_, err = remote(nil, nil, &fakeRouter{send: &fakeStream{
		err: status.Error(codes.Unknown,
			rpcperms.ErrWalletLocked.Error()),
	}}).Pay(context.Background(), req)
	require.ErrorIs(t, err, node.ErrNotSent)

	// A refusal after an update is not a refusal of the payment: the
	// node has already said it is working on it.
	got, err := remote(nil, nil, &fakeRouter{send: &fakeStream{
		updates: []*lnrpc.Payment{{Status: lnrpc.Payment_IN_FLIGHT}},
		err: status.Error(codes.Unknown,
			rpcperms.ErrWalletLocked.Error()),
	}}).Pay(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, node.PaymentInFlight, got.State)

	// An ambiguous failure of the stream is in flight, as before.
	got, err = remote(nil, nil, &fakeRouter{send: &fakeStream{
		err: status.Error(codes.Unavailable, "transport is closing"),
	}}).Pay(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, node.PaymentInFlight, got.State)

	// And an ambiguous failure to open it is an error that is not
	// ErrNotSent, which leaves the swap in Paying.
	_, err = remote(nil, nil, &fakeRouter{
		sendErr: status.Error(codes.Unavailable, "transport is closing"),
	}).Pay(context.Background(), req)
	require.Error(t, err)
	require.NotErrorIs(t, err, node.ErrNotSent)
}
