package offers

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// requestFor builds a request mirroring an offer, unsigned.
func requestFor(t *testing.T, offer *bolt12.Offer) *bolt12.InvoiceRequest {
	t.Helper()

	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{1, 2, 3}, testChain,
	)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(1000)),
	)

	return ir
}

// TestOfferForRequest covers matching a request to a stored offer, the
// arrival rule for paths, and restoring a lost offer.
func TestOfferForRequest(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	ctx := context.Background()

	// An offer without paths: requests must not come over a path.
	plain := env.create(t, CreateParams{Description: "plain"})
	plainOffer, err := bolt12.DecodeOffer(plain.Offer)
	require.NoError(t, err)
	ir := requestFor(t, plainOffer)
	mirrored := OfferFromRequest(ir)
	mirroredID, err := bolt12.OfferID(mirrored)
	require.NoError(t, err)
	require.Equal(t, plain.ID, OfferID(mirroredID))

	rec, offer, err := env.manager.OfferForRequest(ir, nil)
	require.NoError(t, err)
	require.Equal(t, plain.ID, rec.ID)
	require.NotNil(t, offer)
	_, _, err = env.manager.OfferForRequest(ir, []byte{1})
	require.ErrorIs(t, err, ErrWrongPath)

	// An offer with paths: only over the path, with its secret.
	env.paths.paths = []lnwire.BlindedPath{fakeBlindedPath(t)}
	pathed := env.create(t, CreateParams{
		Description: "pathed", WithPaths: true,
	})
	pathedOffer, err := bolt12.DecodeOffer(pathed.Offer)
	require.NoError(t, err)
	ir = requestFor(t, pathedOffer)
	_, _, err = env.manager.OfferForRequest(ir, nil)
	require.ErrorIs(t, err, ErrWrongPath)
	_, _, err = env.manager.OfferForRequest(ir, []byte("short"))
	require.ErrorIs(t, err, ErrWrongPath)
	wrong := pathed.PathSecret
	wrong[31] ^= 1
	_, _, err = env.manager.OfferForRequest(ir, wrong[:])
	require.ErrorIs(t, err, ErrWrongPath)
	rec, _, err = env.manager.OfferForRequest(ir, pathed.PathSecret[:])
	require.NoError(t, err)
	require.Equal(t, pathed.ID, rec.ID)

	// Disabled and expired offers are reported as such.
	require.NoError(t, env.manager.DisableOffer(plain.ID))
	ir = requestFor(t, plainOffer)
	_, _, err = env.manager.OfferForRequest(ir, nil)
	require.ErrorIs(t, err, ErrOfferDisabled)
	require.NoError(t, env.manager.EnableOffer(plain.ID))
	expiring := env.create(t, CreateParams{
		Description:    "soon",
		AbsoluteExpiry: env.clock.Now().Add(time.Minute),
	})
	expiringOffer, err := bolt12.DecodeOffer(expiring.Offer)
	require.NoError(t, err)
	env.clock.SetTime(env.clock.Now().Add(2 * time.Minute))
	_, _, err = env.manager.OfferForRequest(
		requestFor(t, expiringOffer), nil,
	)
	require.ErrorIs(t, err, ErrOfferExpired)

	// Not ours: a foreign offer, and ours with the metadata stripped.
	other := newTestEnvWithSecret(t, [32]byte{9})
	foreign := other.create(t, CreateParams{Description: "theirs"})
	foreignOffer, err := bolt12.DecodeOffer(foreign.Offer)
	require.NoError(t, err)
	_, _, err = env.manager.OfferForRequest(requestFor(t, foreignOffer), nil)
	require.ErrorIs(t, err, ErrNotOurOffer)
	stripped := *plainOffer
	stripped.OfferMetadata = tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]{}
	_, _, err = env.manager.OfferForRequest(requestFor(t, &stripped), nil)
	require.ErrorIs(t, err, ErrNotOurOffer)

	// A lost offer comes back from a request for it.
	require.NoError(t, env.store.Delete(plain.ID))
	_, err = env.manager.LookupOffer(plain.ID)
	require.ErrorIs(t, err, ErrOfferNotFound)
	rec, _, err = env.manager.OfferForRequest(requestFor(t, plainOffer), nil)
	require.NoError(t, err)
	require.Equal(t, plain.ID, rec.ID)
	require.Equal(t, plain.Offer, rec.Offer)
	require.Equal(t, plain.Bolt12, rec.Bolt12)
	require.Equal(t, plain.PathSecret, rec.PathSecret)
	require.Equal(t, "plain", rec.Description)
	require.True(t, rec.Active)
	require.Equal(t, env.clock.Now().Truncate(time.Second).UTC(),
		rec.CreatedAt)
	restored, err := env.manager.LookupOffer(plain.ID)
	require.NoError(t, err)
	require.Equal(t, rec, restored)

	// Restoring an expired one is refused; so is one that is not ours.
	require.NoError(t, env.store.Delete(expiring.ID))
	_, err = env.manager.RestoreOffer(expiringOffer)
	require.Error(t, err)
	_, err = env.manager.RestoreOffer(foreignOffer)
	require.ErrorIs(t, err, ErrNotOurOffer)

	// Restoring one with an expiry keeps the expiry.
	later := env.create(t, CreateParams{
		Description:    "later",
		AmountMsat:     5000,
		AbsoluteExpiry: env.clock.Now().Add(time.Hour),
	})
	laterOffer, err := bolt12.DecodeOffer(later.Offer)
	require.NoError(t, err)
	require.NoError(t, env.store.Delete(later.ID))
	rec, err = env.manager.RestoreOffer(laterOffer)
	require.NoError(t, err)
	require.Equal(t, later.AbsoluteExpiry, rec.AbsoluteExpiry)
	require.Equal(t, uint64(5000), rec.AmountMsat)
	require.Equal(t, "later", rec.Description)
	_ = ctx
}
