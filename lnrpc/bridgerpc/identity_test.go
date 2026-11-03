//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestIdentityKeyIsWhatASwapRecords: both nodes give their key in the form a
// swap records its issuer in, and nothing that could not be one.
func TestIdentityKeyIsWhatASwapRecords(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	key := "02" + strings.Repeat("ab", 32)

	got, err := NewLocal(&Deps{NodeKey: strings.ToUpper(key)}).IdentityKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key, got)

	_, err = NewLocal(&Deps{}).IdentityKey(ctx)
	require.Error(t, err)

	now := time.Unix(1_800_000_000, 0)
	main := &fakeMain{info: &lnrpc.GetInfoResponse{IdentityPubkey: key}}
	r := &Remote{main: main, now: func() time.Time { return now }}
	got, err = r.IdentityKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key, got)

	// A recent answer serves the next quote without asking again; one
	// older than the bridge's polls is asked for afresh.
	other := "03" + strings.Repeat("cd", 32)
	main.info = &lnrpc.GetInfoResponse{IdentityPubkey: other}
	got, err = r.IdentityKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key, got)
	now = now.Add(routeTipAge + time.Second)
	got, err = r.IdentityKey(ctx)
	require.NoError(t, err)
	require.Equal(t, other, got)

	// The bridge's tip polls keep it fresh.
	main.info = &lnrpc.GetInfoResponse{IdentityPubkey: key, BlockHeight: 7}
	now = now.Add(routeTipAge + time.Second)
	_, err = r.BestBlock(ctx)
	require.NoError(t, err)
	main.info = &lnrpc.GetInfoResponse{}
	got, err = r.IdentityKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key, got)

	r = &Remote{main: &fakeMain{info: &lnrpc.GetInfoResponse{}},
		now: time.Now}
	_, err = r.IdentityKey(ctx)
	require.Error(t, err)
}
