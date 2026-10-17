package shachain

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

// TestIndependentProducer checks that an IndependentProducer's secrets are
// deterministic, distinct, valid private keys that a shachain store refuses,
// and that it encodes, and can be made from, the same root as a shachain
// producer.
func TestIndependentProducer(t *testing.T) {
	t.Parallel()

	root := randomHash(t)
	shaProducer := NewRevocationProducer(*root)

	producer, err := NewIndependentProducerFrom(shaProducer)
	require.NoError(t, err)

	// Same root, same encoding.
	var a, b bytes.Buffer
	require.NoError(t, shaProducer.Encode(&a))
	require.NoError(t, producer.Encode(&b))
	require.Equal(t, a.Bytes(), b.Bytes())

	again := NewIndependentProducer(*root)
	seen := make(map[[32]byte]struct{})
	shaStore := NewRevocationStore()
	refused := false
	for i := uint64(0); i < 64; i++ {
		secret, err := producer.AtIndex(i)
		require.NoError(t, err)

		// Deterministic.
		same, err := again.AtIndex(i)
		require.NoError(t, err)
		require.Equal(t, *secret, *same)

		// Distinct, and different from the shachain secret.
		_, dup := seen[*secret]
		require.False(t, dup, "index %d repeats a secret", i)
		seen[*secret] = struct{}{}

		shaSecret, err := shaProducer.AtIndex(i)
		require.NoError(t, err)
		require.NotEqual(t, *shaSecret, *secret)

		// A valid private key as it stands.
		var s btcec.ModNScalar
		require.False(t, s.SetByteSlice(secret[:]))
		require.False(t, s.IsZero())

		// A shachain store can't take them.
		if !refused && shaStore.AddNextEntry(secret) != nil {
			refused = true
		}
	}
	require.True(t, refused, "a shachain store accepted every secret")
}
