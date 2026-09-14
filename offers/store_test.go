package offers

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *KVStore {
	t.Helper()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "offers")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	store, err := NewKVStore(backend)
	require.NoError(t, err)

	return store
}

func sampleRecord(id byte) *Record {
	var oid OfferID
	oid[0] = id
	var secret [32]byte
	secret[31] = id

	return &Record{
		ID:              oid,
		Offer:           []byte{1, 2, 3, id},
		Bolt12:          "lno1test",
		Description:     "OCEAN Payouts for bc1qexample",
		AmountMsat:      0,
		AbsoluteExpiry:  time.Unix(1_900_000_000, 0).UTC(),
		CreatedAt:       time.Unix(1_800_000_000+int64(id), 0).UTC(),
		Active:          true,
		IssuerKeyFamily: 6,
		IssuerKeyIndex:  0,
		PathSecret:      secret,
		InvoicesIssued:  3,
		Label:           "pool",
	}
}

// TestStoreRoundTrip stores a record and reads back every field.
func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	want := sampleRecord(1)
	require.NoError(t, store.Put(want))

	got, err := store.Get(want.ID)
	require.NoError(t, err)
	require.Equal(t, want, got)

	// The same id again is the same offer, not a second one.
	require.ErrorIs(t, store.Put(want), ErrOfferExists)

	// Unknown ids are unknown.
	_, err = store.Get(OfferID{9})
	require.ErrorIs(t, err, ErrOfferNotFound)
}

// TestStoreNoExpiryAndInactive covers the zero expiry and the disabled flag.
func TestStoreNoExpiryAndInactive(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	want := sampleRecord(2)
	want.AbsoluteExpiry = time.Time{}
	want.Active = false
	want.Label = ""
	require.NoError(t, store.Put(want))

	got, err := store.Get(want.ID)
	require.NoError(t, err)
	require.True(t, got.AbsoluteExpiry.IsZero())
	require.False(t, got.Active)
	require.Equal(t, want, got)
}

// TestStoreListUpdateDelete covers the remaining operations.
func TestStoreListUpdateDelete(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	a, b := sampleRecord(3), sampleRecord(4)
	require.NoError(t, store.Put(a))
	require.NoError(t, store.Put(b))

	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 2)

	require.NoError(t, store.Update(a.ID, func(r *Record) error {
		r.Active = false
		r.InvoicesIssued++

		return nil
	}))
	got, err := store.Get(a.ID)
	require.NoError(t, err)
	require.False(t, got.Active)
	require.Equal(t, uint64(4), got.InvoicesIssued)

	// A failing update leaves the record alone.
	boom := errors.New("boom")
	require.ErrorIs(t, store.Update(a.ID, func(r *Record) error {
		r.InvoicesIssued = 99

		return boom
	}), boom)
	got, err = store.Get(a.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(4), got.InvoicesIssued)

	require.ErrorIs(t, store.Update(OfferID{7}, func(*Record) error {
		return nil
	}), ErrOfferNotFound)

	require.NoError(t, store.Delete(b.ID))
	_, err = store.Get(b.ID)
	require.ErrorIs(t, err, ErrOfferNotFound)
	require.ErrorIs(t, store.Delete(b.ID), ErrOfferNotFound)
	list, err = store.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
}

// TestDeserializeToleratesUnknownTypes makes sure a record written by a
// newer node with an extra field still reads.
func TestDeserializeToleratesUnknownTypes(t *testing.T) {
	t.Parallel()

	want := sampleRecord(5)
	data, err := serialize(want)
	require.NoError(t, err)

	// Append an unknown record.
	extra := []byte("future")
	stream, err := tlv.NewStream(tlv.MakePrimitiveRecord(200, &extra))
	require.NoError(t, err)
	var buf bytes.Buffer
	buf.Write(data)
	require.NoError(t, stream.Encode(&buf))

	got, err := deserialize(want.ID, buf.Bytes())
	require.NoError(t, err)
	require.Equal(t, want, got)

	// A record without an offer is corrupt.
	_, err = deserialize(want.ID, nil)
	require.Error(t, err)
}

// TestListSkipsDamagedRecord checks one undecodable value does not hide the
// rest of the offers from List, while Get still reports it.
func TestListSkipsDamagedRecord(t *testing.T) {
	t.Parallel()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "offers")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	store, err := NewKVStore(backend)
	require.NoError(t, err)

	good := &Record{
		ID:        OfferID{1},
		Offer:     []byte{1, 2, 3},
		Bolt12:    "lno1good",
		CreatedAt: time.Unix(1_800_000_000, 0).UTC(),
		Active:    true,
	}
	require.NoError(t, store.Put(good))

	bad := OfferID{2}
	require.NoError(t, kvdb.Update(backend, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(offersBucket)

		return bucket.Put(bad[:], []byte{0xff, 0xff, 0xff})
	}, func() {}))

	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, good.ID, records[0].ID)

	_, err = store.Get(bad)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrOfferNotFound)
}
