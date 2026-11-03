//go:build bridgerpc
// +build bridgerpc

package bridgerpc

import (
	"context"
	"strings"
	"testing"

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

	r := &Remote{main: &fakeMain{info: &lnrpc.GetInfoResponse{
		IdentityPubkey: key,
	}}}
	got, err = r.IdentityKey(ctx)
	require.NoError(t, err)
	require.Equal(t, key, got)

	r = &Remote{main: &fakeMain{info: &lnrpc.GetInfoResponse{}}}
	_, err = r.IdentityKey(ctx)
	require.Error(t, err)
}
