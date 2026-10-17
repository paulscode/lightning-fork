package channeldb

import (
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/shachain"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestRevocationLogCommitSecret checks that a revocation log entry stores the
// per-commitment secret it's given, and that entries written without one
// read back without one.
func TestRevocationLogCommitSecret(t *testing.T) {
	t.Parallel()

	var secret [32]byte
	_, err := rand.Read(secret[:])
	require.NoError(t, err)

	fullDB, err := MakeTestDB(t)
	require.NoError(t, err)
	backend := fullDB.ChannelStateDB().backend

	withSecret := testChannelCommit
	withSecret.CommitHeight = 7
	withoutSecret := testChannelCommit
	withoutSecret.CommitHeight = 8

	// The secret is kept whether or not the node stores HTLC amounts in
	// its revocation log.
	for _, noAmtData := range []bool{false, true} {
		var got, gotNone RevocationLog
		err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
			_, bucket, err := createTestRevocationLogBuckets(tx)
			require.NoError(t, err)

			err = putRevocationLogWithSecret(
				bucket, &withSecret, 0, 1, noAmtData,
				fn.Some(secret),
			)
			require.NoError(t, err)

			err = putRevocationLog(
				bucket, &withoutSecret, 0, 1, noAmtData,
			)
			require.NoError(t, err)

			got, err = fetchRevocationLog(
				bucket, withSecret.CommitHeight,
			)
			require.NoError(t, err)

			gotNone, err = fetchRevocationLog(
				bucket, withoutSecret.CommitHeight,
			)

			return err
		}, func() {})
		require.NoError(t, err)

		var stored [32]byte
		got.CommitSecret.WhenSome(
			func(r tlv.RecordT[tlv.TlvType65537, [32]byte]) {
				stored = r.Val
			},
		)
		require.True(t, got.CommitSecret.IsSome(), "noAmtData=%v",
			noAmtData)
		require.Equal(t, secret, stored)

		require.True(t, gotNone.CommitSecret.IsNone())

		err = kvdb.Update(backend, func(tx kvdb.RwTx) error {
			return tx.DeleteTopLevelBucket(openChannelBucket)
		}, func() {})
		require.NoError(t, err)
	}
}

// TestNewRemoteSecretStore checks the store chosen for each channel type.
func TestNewRemoteSecretStore(t *testing.T) {
	t.Parallel()

	chanType := SingleFunderTweaklessBit | AnchorOutputsBit
	_, ok := NewRemoteSecretStore(chanType).(*shachain.RevocationStore)
	require.True(t, ok)

	chanType |= IndependentSecretsBit
	require.True(t, chanType.HasIndependentSecrets())
	_, ok = NewRemoteSecretStore(chanType).(*shachain.IndependentStore)
	require.True(t, ok)
}

// TestUsesIndependentSecrets checks that either the channel type bit or the
// store type marks a channel as keeping its secrets in the revocation log, so
// a channel whose two disagree still finds them there.
func TestUsesIndependentSecrets(t *testing.T) {
	t.Parallel()

	base := SingleFunderTweaklessBit | AnchorOutputsBit

	cases := []struct {
		name  string
		bit   bool
		store shachain.Store
		want  bool
	}{
		{"neither", false, shachain.NewRevocationStore(), false},
		{"both", true, shachain.NewIndependentStore(), true},
		{"bit only", true, shachain.NewRevocationStore(), true},
		{"store only", false, shachain.NewIndependentStore(), true},
	}
	for _, c := range cases {
		chanType := base
		if c.bit {
			chanType |= IndependentSecretsBit
		}
		channel := &OpenChannel{
			ChanType:        chanType,
			RevocationStore: c.store,
		}
		require.Equal(t, c.want, channel.UsesIndependentSecrets(),
			c.name)
	}
}

// TestRevokedCommitSecretRequiresNewest checks that the secret written to the
// revocation log is the newest one in the store, and that a store whose
// count doesn't match the commitment being revoked is refused rather than
// answered with the secret before it.
func TestRevokedCommitSecretRequiresNewest(t *testing.T) {
	t.Parallel()

	store := shachain.NewIndependentStore()
	var newest [32]byte
	for i := 0; i < 3; i++ {
		_, err := rand.Read(newest[:])
		require.NoError(t, err)
		h := chainhash.Hash(newest)
		require.NoError(t, store.AddNextEntry(&h))
	}

	channel := &OpenChannel{
		ChanType: SingleFunderTweaklessBit | AnchorOutputsBit |
			IndependentSecretsBit,
		RevocationStore: store,
	}

	// The store holds secrets 0..2, so it can revoke commitment 2.
	channel.RemoteCommitment.CommitHeight = 2
	secret, err := channel.revokedCommitSecret()
	require.NoError(t, err)
	require.Equal(t, fn.Some(newest), secret)

	// Commitment 1's secret is in the store too, but it's not the newest:
	// the store and the commitment chain are out of step.
	channel.RemoteCommitment.CommitHeight = 1
	_, err = channel.revokedCommitSecret()
	require.ErrorContains(t, err, "want the newest")

	// Channels without the add-on don't record a secret at all.
	channel.ChanType &^= IndependentSecretsBit
	channel.RevocationStore = shachain.NewRevocationStore()
	secret, err = channel.revokedCommitSecret()
	require.NoError(t, err)
	require.True(t, secret.IsNone())
}
