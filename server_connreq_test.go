package lnd

import (
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestHasPersistentConnReq checks that a permanent request is recognised as a
// duplicate only for the same key and the same network address.
func TestHasPersistentConnReq(t *testing.T) {
	t.Parallel()

	key1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	addr := func(k *btcec.PrivateKey, host string) *lnwire.NetAddress {
		tcp, err := net.ResolveTCPAddr("tcp", host)
		require.NoError(t, err)

		return &lnwire.NetAddress{
			IdentityKey: k.PubKey(),
			Address:     tcp,
		}
	}

	existing := []*connmgr.ConnReq{
		{Addr: addr(key1, "10.0.0.1:9735"), Permanent: true},
	}

	require.True(t, hasPersistentConnReq(
		existing, addr(key1, "10.0.0.1:9735"),
	))
	require.False(t, hasPersistentConnReq(
		existing, addr(key1, "10.0.0.2:9735"),
	))
	require.False(t, hasPersistentConnReq(
		existing, addr(key2, "10.0.0.1:9735"),
	))
	require.False(t, hasPersistentConnReq(
		nil, addr(key1, "10.0.0.1:9735"),
	))
}
