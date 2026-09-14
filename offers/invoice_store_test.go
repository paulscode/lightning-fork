package offers

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/stretchr/testify/require"
)

// TestInvoiceStore covers storing, fetching and listing issued invoices.
func TestInvoiceStore(t *testing.T) {
	t.Parallel()

	backend, cleanup, err := kvdb.GetTestBackend(t.TempDir(), "offers")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	store, err := NewInvoiceStore(backend)
	require.NoError(t, err)

	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	base := time.Unix(1_800_000_000, 0).UTC()
	offerA, offerB := OfferID{0xa}, OfferID{0xb}
	mk := func(n byte, id OfferID, at time.Time) *IssuedInvoice {
		return &IssuedInvoice{
			PaymentHash: [32]byte{n},
			OfferID:     id,
			PayerID:     payer.PubKey(),
			AmountMsat:  uint64(n) * 1000,
			Quantity:    uint64(n),
			CreatedAt:   at,
			Bolt12:      "lni1" + string(rune('a'+n)),
		}
	}
	require.NoError(t, store.Put(mk(3, offerA, base.Add(3*time.Second))))
	require.NoError(t, store.Put(mk(1, offerA, base.Add(time.Second))))
	require.NoError(t, store.Put(mk(2, offerB, base.Add(2*time.Second))))

	got, err := store.Get([32]byte{1})
	require.NoError(t, err)
	require.Equal(t, mk(1, offerA, base.Add(time.Second)), got)
	_, err = store.Get([32]byte{7})
	require.ErrorIs(t, err, ErrInvoiceNotFound)

	list, err := store.ListForOffer(offerA)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, [32]byte{1}, list[0].PaymentHash, "oldest first")
	require.Equal(t, [32]byte{3}, list[1].PaymentHash)
	list, err = store.ListForOffer(OfferID{0xc})
	require.NoError(t, err)
	require.Empty(t, list)

	// A damaged record is skipped by List and reported by Get.
	bad := [32]byte{9}
	require.NoError(t, kvdb.Update(backend, func(tx kvdb.RwTx) error {
		return tx.ReadWriteBucket(invoicesBucket).Put(bad[:], []byte{1})
	}, func() {}))
	list, err = store.ListForOffer(offerB)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = store.Get(bad)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvoiceNotFound)

	// No payer id is refused.
	require.Error(t, store.Put(&IssuedInvoice{PaymentHash: [32]byte{5}}))

	// A record without required fields is refused on read.
	_, err = deserializeInvoice([32]byte{5}, []byte{6, 1, 1})
	require.Error(t, err)
}
