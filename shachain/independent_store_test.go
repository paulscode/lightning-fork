package shachain

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func randomHash(t *testing.T) *chainhash.Hash {
	t.Helper()

	var h chainhash.Hash
	_, err := rand.Read(h[:])
	require.NoError(t, err)

	return &h
}

// TestIndependentStoreAcceptsUnrelatedSecrets checks that the store accepts
// secrets with no shachain relationship, which RevocationStore refuses, and
// that it serves the two most recent ones.
func TestIndependentStoreAcceptsUnrelatedSecrets(t *testing.T) {
	t.Parallel()

	// A RevocationStore refuses two random secrets in a row, because the
	// second (index 1) must derive the first (index 0).
	shaStore := NewRevocationStore()
	require.NoError(t, shaStore.AddNextEntry(randomHash(t)))
	require.Error(t, shaStore.AddNextEntry(randomHash(t)))

	store := NewIndependentStore()
	_, err := store.LookUp(0)
	require.Error(t, err)

	var last, prev *chainhash.Hash
	for i := uint64(0); i < 100; i++ {
		prev = last
		last = randomHash(t)
		require.NoError(t, store.AddNextEntry(last))

		got, err := store.LookUp(i)
		require.NoError(t, err)
		require.Equal(t, *last, *got)

		if i > 0 {
			got, err := store.LookUp(i - 1)
			require.NoError(t, err)
			require.Equal(t, *prev, *got)
		}

		if i > 1 {
			_, err := store.LookUp(i - 2)
			require.ErrorIs(t, err, ErrSecretNotRetained)
		}
	}

	require.EqualValues(t, 100, store.NumEntries())

	_, err = store.LookUp(100)
	require.Error(t, err)
}

// TestStoreEncodingRoundTrip checks that NewStoreFromBytes decodes both store
// kinds, reads exactly the bytes of the store, and preserves their contents.
func TestStoreEncodingRoundTrip(t *testing.T) {
	t.Parallel()

	trailer := []byte{0xde, 0xad, 0xbe, 0xef}

	// Empty and populated independent stores.
	for _, n := range []int{0, 1, 2, 7} {
		store := NewIndependentStore()
		var last, prev *chainhash.Hash
		for i := 0; i < n; i++ {
			prev = last
			last = randomHash(t)
			require.NoError(t, store.AddNextEntry(last))
		}

		var b bytes.Buffer
		require.NoError(t, store.Encode(&b))
		b.Write(trailer)

		r := bytes.NewReader(b.Bytes())
		decoded, err := NewStoreFromBytes(r)
		require.NoError(t, err)

		ind, ok := decoded.(*IndependentStore)
		require.True(t, ok)
		require.EqualValues(t, n, ind.NumEntries())
		if n > 0 {
			got, err := ind.LookUp(uint64(n - 1))
			require.NoError(t, err)
			require.Equal(t, *last, *got)
		}
		if n > 1 {
			got, err := ind.LookUp(uint64(n - 2))
			require.NoError(t, err)
			require.Equal(t, *prev, *got)
		}

		rest := make([]byte, r.Len())
		_, _ = r.Read(rest)
		require.Equal(t, trailer, rest)
	}

	// A shachain store still decodes as a RevocationStore.
	seed := randomHash(t)
	producer := NewRevocationProducer(*seed)
	shaStore := NewRevocationStore()
	for i := uint64(0); i < 10; i++ {
		secret, err := producer.AtIndex(i)
		require.NoError(t, err)
		require.NoError(t, shaStore.AddNextEntry(secret))
	}

	var b bytes.Buffer
	require.NoError(t, shaStore.Encode(&b))
	b.Write(trailer)

	r := bytes.NewReader(b.Bytes())
	decoded, err := NewStoreFromBytes(r)
	require.NoError(t, err)

	_, ok := decoded.(*RevocationStore)
	require.True(t, ok)
	for i := uint64(0); i < 10; i++ {
		want, err := producer.AtIndex(i)
		require.NoError(t, err)
		got, err := decoded.LookUp(i)
		require.NoError(t, err)
		require.Equal(t, *want, *got)
	}

	rest := make([]byte, r.Len())
	_, _ = r.Read(rest)
	require.Equal(t, trailer, rest)
}
