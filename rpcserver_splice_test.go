package lnd

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// TestSplicePermissions pins the permissions of the splice RPCs. Starting a
// splice moves funds between the wallet and a channel, so it needs on-chain
// and off-chain write, as opening and closing do: a macaroon with
// offchain:write alone (the dashboard's "Lightning wallet" preset promises it
// can't send on-chain) can't splice. Estimating is read-only.
func TestSplicePermissions(t *testing.T) {
	t.Parallel()

	perms := MainRPCServerPermissions()

	require.ElementsMatch(t, []bakery.Op{
		{Entity: "onchain", Action: "write"},
		{Entity: "offchain", Action: "write"},
	}, perms["/lnrpc.Lightning/SpliceChannel"])

	require.ElementsMatch(t, []bakery.Op{
		{Entity: "onchain", Action: "read"},
		{Entity: "offchain", Action: "read"},
	}, perms["/lnrpc.Lightning/EstimateSplice"])

	// The same as opening and closing a channel.
	require.ElementsMatch(t, perms["/lnrpc.Lightning/OpenChannel"],
		perms["/lnrpc.Lightning/SpliceChannel"])
	require.ElementsMatch(t, perms["/lnrpc.Lightning/CloseChannel"],
		perms["/lnrpc.Lightning/SpliceChannel"])
}

// TestSpliceRequestRanges checks the request's amount and fee rate are
// refused out of range before anything else is looked at: a mistyped rate
// would pay the channel's balance in fees.
func TestSpliceRequestRanges(t *testing.T) {
	t.Parallel()

	r := &rpcServer{}
	for _, tc := range []struct {
		amount int64
		rate   uint64
		want   string
	}{
		{amount: -1, want: "negative amount"},
		{amount: btcutil.MaxSatoshi + 1, want: "most that can exist"},
		{amount: 100_000, rate: maxSpliceSatPerVByte + 1,
			want: "fee rate above"},
	} {
		_, _, err := r.spliceRequest(
			nil, lnrpc.SpliceDirection_SPLICE_IN, tc.amount, false,
			tc.rate, 0, "", false, nil,
		)
		require.ErrorContains(t, err, tc.want)
		require.Equal(t, codes.InvalidArgument, status.Code(err))

		// The estimate the same.
		_, err = r.EstimateSplice(t.Context(),
			&lnrpc.EstimateSpliceRequest{
				AmountSat: tc.amount, SatPerVbyte: tc.rate,
			})
		require.ErrorContains(t, err, tc.want)
	}
}
