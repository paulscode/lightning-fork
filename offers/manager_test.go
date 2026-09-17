package offers

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	testChain  = [32]byte{0xb2, 0xb2, 0xb2, 1}
	testSecret = [32]byte{0x5e, 0xc7, 0xe7}
)

// create mints an offer that must be new.
func (e *testEnv) create(t *testing.T, params CreateParams) *Record {
	t.Helper()

	rec, created, err := e.manager.CreateOffer(context.Background(), params)
	require.NoError(t, err)
	require.True(t, created, "expected a new offer")

	return rec
}

type testEnv struct {
	manager *Manager
	store   *KVStore
	clock   *clock.TestClock
	issuer  *btcec.PrivateKey
	paths   *fakePathBuilder
	reach   bool
}

type fakePathBuilder struct {
	calls   int
	secrets [][32]byte
	paths   []lnwire.BlindedPath
	err     error
}

func (f *fakePathBuilder) BuildOfferPaths(_ context.Context,
	secret [32]byte) ([]lnwire.BlindedPath, error) {

	f.calls++
	f.secrets = append(f.secrets, secret)

	return f.paths, f.err
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	return newTestEnvWithSecret(t, testSecret)
}

func newTestEnvWithSecret(t *testing.T, secret [32]byte) *testEnv {
	t.Helper()

	issuer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	env := &testEnv{
		store:  testStore(t),
		clock:  clock.NewTestClock(time.Unix(1_800_000_000, 0)),
		issuer: issuer,
		paths:  &fakePathBuilder{},
		reach:  true,
	}
	env.manager, err = NewManager(Config{
		ChainHash: testChain,
		Secret:    secret,
		IssuerKey: keychain.KeyDescriptor{
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyNodeKey,
				Index:  0,
			},
			PubKey: issuer.PubKey(),
		},
		Store:         env.store,
		Clock:         env.clock,
		PathBuilder:   env.paths,
		NodeReachable: func() bool { return env.reach },
	})
	require.NoError(t, err)

	return env
}

func fakeBlindedPath(t *testing.T) lnwire.BlindedPath {
	t.Helper()

	intro, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	blinding, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hop, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	node, err := lnwire.NewPubkeyIntro(intro.PubKey())
	require.NoError(t, err)

	return lnwire.BlindedPath{
		IntroductionNode: node,
		BlindingPoint:    blinding.PubKey(),
		Hops: []lnwire.BlindedHop{{
			BlindedNodeID: hop.PubKey(),
			EncryptedData: []byte{1, 2, 3, 4},
		}},
	}
}

// TestCreateOfferForThisChain mints the OCEAN-shaped offer and checks what a
// payer would see.
func TestCreateOfferForThisChain(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	rec := env.create(t, CreateParams{
		Description: "OCEAN Payouts for bc1qexampleaddress",
		Label:       "pool",
	})
	require.True(t, rec.Active)
	require.Equal(t, uint64(0), rec.AmountMsat, "any amount")
	require.True(t, rec.AbsoluteExpiry.IsZero())
	require.Equal(t, "pool", rec.Label)
	require.Equal(t, uint32(keychain.KeyFamilyNodeKey), rec.IssuerKeyFamily)
	require.NotEqual(t, [32]byte{}, rec.PathSecret)
	require.Equal(t, env.clock.Now().UTC(), rec.CreatedAt)

	// The string decodes to an offer for our chain with our key.
	hrp, data, err := bolt12.Decode(rec.Bolt12)
	require.NoError(t, err)
	require.Equal(t, OfferHRP, hrp)
	require.Equal(t, rec.Offer, data)
	offer, err := bolt12.DecodeOffer(data)
	require.NoError(t, err)
	require.Equal(t, [][32]byte{testChain}, offerChains(offer))
	require.True(t, IssuerID(offer).IsEqual(env.issuer.PubKey()))
	require.True(t, offer.OfferMetadata.IsSome(), "metadata names it ours")
	require.True(t, env.manager.IsOurs(offer))
	pathSecret, err := env.manager.PathSecretFor(offer)
	require.NoError(t, err)
	require.Equal(t, pathSecret, rec.PathSecret)
	require.False(t, offer.OfferPaths.IsSome(), "reachable node: no paths")
	require.False(t, offer.OfferAmount.IsSome())
	var desc string
	offer.OfferDescription.WhenSome(func(r tlv.RecordT[tlv.TlvType10, tlv.Blob]) {
		desc = string(r.Val)
	})
	require.Equal(t, "OCEAN Payouts for bc1qexampleaddress", desc)

	// It passes read validation for our chain and fails for another.
	require.NoError(t, bolt12.ValidateOfferRead(
		offer, env.clock.Now(), testChain, nil,
	))
	require.ErrorIs(t, bolt12.ValidateOfferRead(
		offer, env.clock.Now(), [32]byte{0xff}, nil,
	), bolt12.ErrUnsupportedChain)

	// Stored under its id, which is the Merkle root of its fields.
	id, err := bolt12.OfferID(offer)
	require.NoError(t, err)
	require.Equal(t, OfferID(id), rec.ID)
	got, err := env.manager.LookupOffer(rec.ID)
	require.NoError(t, err)
	require.Equal(t, rec, got)

	// The same fields mint the same offer: after a restore from seed the
	// pool's copy of the string is minted again, not a new one. The
	// label is not part of the offer, so it does not change the id.
	env.clock.SetTime(env.clock.Now().Add(time.Hour))
	rec2, created, err := env.manager.CreateOffer(
		context.Background(), CreateParams{
			Description: "OCEAN Payouts for bc1qexampleaddress",
			Label:       "other label",
		},
	)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, rec, rec2, "the stored record comes back")

	// A different node secret mints a different offer for the same
	// fields, and does not recognise ours.
	other := newTestEnvWithSecret(t, [32]byte{1})
	rec3 := other.create(t, CreateParams{
		Description: "OCEAN Payouts for bc1qexampleaddress",
	})
	require.NotEqual(t, rec.ID, rec3.ID)
	require.False(t, other.manager.IsOurs(offer))

	// Tampered metadata is not ours; a foreign offer with our issuer id
	// and no metadata is not ours either.
	tampered := *offer
	tampered.OfferMetadata = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType4](tlv.Blob(make([]byte, 16))),
	)
	require.False(t, env.manager.IsOurs(&tampered))
	foreign := *offer
	foreign.OfferMetadata = tlv.OptionalRecordT[tlv.TlvType4, tlv.Blob]{}
	require.False(t, env.manager.IsOurs(&foreign))
}

// TestCreateOfferFields covers amount, expiry, issuer and quantity.
func TestCreateOfferFields(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	expiry := env.clock.Now().Add(time.Hour).Add(300 * time.Millisecond)
	rec := env.create(t, CreateParams{
		Description:    "coffee",
		AmountMsat:     150_000,
		AbsoluteExpiry: expiry.In(time.FixedZone("x", -5*3600)),
		Issuer:         "Paul's stand",
		QuantityMax:    3,
	})
	require.Equal(t, uint64(150_000), rec.AmountMsat)
	expiry = expiry.Truncate(time.Second).UTC()
	require.Equal(t, expiry, rec.AbsoluteExpiry,
		"whole seconds, UTC, as stored")
	stored, err := env.manager.LookupOffer(rec.ID)
	require.NoError(t, err)
	require.Equal(t, rec, stored, "the returned record is the stored one")

	offer, err := bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	var (
		amount uint64
		exp    uint64
		issuer string
		qty    uint64
	)
	offer.OfferAmount.WhenSome(func(r tlv.RecordT[tlv.TlvType8, bolt12.TUint64]) {
		amount = uint64(r.Val)
	})
	offer.OfferAbsoluteExpiry.WhenSome(func(r tlv.RecordT[tlv.TlvType14, bolt12.TUint64]) {
		exp = uint64(r.Val)
	})
	offer.OfferIssuer.WhenSome(func(r tlv.RecordT[tlv.TlvType18, tlv.Blob]) {
		issuer = string(r.Val)
	})
	offer.OfferQuantityMax.WhenSome(func(r tlv.RecordT[tlv.TlvType20, bolt12.TUint64]) {
		qty = uint64(r.Val)
	})
	require.Equal(t, uint64(150_000), amount)
	require.Equal(t, uint64(expiry.Unix()), exp)
	require.Equal(t, "Paul's stand", issuer)
	require.Equal(t, uint64(3), qty)

	// A request for a quantity over the maximum is refused by the spec
	// validator, and one within it accepted.
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{1}, testChain,
	)
	require.NoError(t, err)
	ir.InvreqQuantity = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType86](bolt12.TUint64(4)),
	)
	require.Error(t, bolt12.ValidateInvoiceRequestWrite(ir))
	ir.InvreqQuantity = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType86](bolt12.TUint64(3)),
	)
	require.NoError(t, bolt12.ValidateInvoiceRequestWrite(ir))

	// "Any quantity" is offer_quantity_max present and zero.
	anyRec := env.create(t, CreateParams{
		Description: "any", QuantityAny: true,
	})
	anyOffer, err := bolt12.DecodeOffer(anyRec.Offer)
	require.NoError(t, err)
	require.True(t, anyOffer.OfferQuantityMax.IsSome())
	require.Equal(t, uint64(0),
		uint64(anyOffer.OfferQuantityMax.ValOpt().UnwrapOr(9)))
	ir, err = bolt12.NewInvoiceRequestFromOffer(
		anyOffer, payer.PubKey(), []byte{1}, testChain,
	)
	require.NoError(t, err)
	ir.InvreqQuantity = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType86](bolt12.TUint64(1_000_000)),
	)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(1000)),
	)
	require.NoError(t, bolt12.ValidateInvoiceRequestWrite(ir))

	// No quantity at all: a request must not ask for one.
	plain := env.create(t, CreateParams{Description: "plain"})
	plainOffer, err := bolt12.DecodeOffer(plain.Offer)
	require.NoError(t, err)
	require.False(t, plainOffer.OfferQuantityMax.IsSome())
}

// TestCreateOfferRejects covers the inputs that must not become offers.
func TestCreateOfferRejects(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	ctx := context.Background()

	_, _, err := env.manager.CreateOffer(ctx, CreateParams{AmountMsat: 10})
	require.ErrorIs(t, err, ErrDescriptionRequired)

	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description:    "late",
		AbsoluteExpiry: env.clock.Now().Add(-time.Second),
	})
	require.ErrorIs(t, err, ErrExpiryInPast)
	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description:    "now",
		AbsoluteExpiry: env.clock.Now(),
	})
	require.ErrorIs(t, err, ErrExpiryInPast)

	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: "bad\x00text",
	})
	require.ErrorIs(t, err, ErrControlCharacters)

	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: string([]byte{0xff, 0xfe}),
	})
	require.ErrorIs(t, err, ErrInvalidUTF8)

	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: strings.Repeat("a", MaxDescriptionBytes+1),
	})
	require.ErrorIs(t, err, ErrTextTooLong)
	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: "x", Issuer: strings.Repeat("i", MaxIssuerBytes+1),
	})
	require.ErrorIs(t, err, ErrTextTooLong)
	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: "x", Label: strings.Repeat("l", MaxLabelBytes+1),
	})
	require.ErrorIs(t, err, ErrTextTooLong)

	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: "x", WithPaths: true, NoPaths: true,
	})
	require.ErrorIs(t, err, ErrPathsConflict)

	_, _, err = env.manager.CreateOffer(ctx, CreateParams{
		Description: "x", QuantityMax: 2, QuantityAny: true,
	})
	require.ErrorIs(t, err, ErrQuantityConflict)

	// Nothing was stored.
	list, err := env.manager.ListOffers(false)
	require.NoError(t, err)
	require.Empty(t, list)

	// The cap on offers.
	env.manager.cfg.MaxOffers = 2
	env.create(t, CreateParams{Description: "1"})
	env.create(t, CreateParams{Description: "2"})
	_, _, err = env.manager.CreateOffer(ctx, CreateParams{Description: "3"})
	require.ErrorIs(t, err, ErrTooManyOffers)
	_, created, err := env.manager.CreateOffer(ctx, CreateParams{
		Description: "1",
	})
	require.NoError(t, err, "an existing offer is still returned")
	require.False(t, created)
}

// TestOfferPaths covers when blinded paths are asked for and used.
func TestOfferPaths(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	ctx := context.Background()
	env.paths.paths = []lnwire.BlindedPath{fakeBlindedPath(t)}

	// Reachable node: no paths unless asked.
	rec := env.create(t, CreateParams{Description: "a"})
	require.Equal(t, 0, env.paths.calls)
	offer, err := bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	require.False(t, offer.OfferPaths.IsSome())

	rec = env.create(t, CreateParams{Description: "b", WithPaths: true})
	require.Equal(t, 1, env.paths.calls)
	require.Equal(t, rec.PathSecret, env.paths.secrets[0],
		"the builder gets the offer's own secret")
	offer, err = bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	require.True(t, offer.OfferPaths.IsSome())
	require.True(t, IssuerID(offer).IsEqual(env.issuer.PubKey()),
		"the issuer id stays beside the paths")

	// Unreachable node: paths by default, none when forbidden.
	env.reach = false
	rec = env.create(t, CreateParams{Description: "c"})
	require.Equal(t, 2, env.paths.calls)
	offer, err = bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	require.True(t, offer.OfferPaths.IsSome())

	rec = env.create(t, CreateParams{Description: "d", NoPaths: true})
	require.Equal(t, 2, env.paths.calls)
	offer, err = bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	require.False(t, offer.OfferPaths.IsSome())

	// No intro peer: the offer falls back to the node id, and is still
	// a valid offer.
	env.paths.paths = nil
	rec = env.create(t, CreateParams{Description: "e"})
	offer, err = bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	require.False(t, offer.OfferPaths.IsSome())
	require.NotNil(t, IssuerID(offer))

	// A builder failure is a failure.
	env.paths.err = errors.New("no peers")
	_, _, err = env.manager.CreateOffer(ctx, CreateParams{Description: "f"})
	require.Error(t, err)
}

// TestListDisableServe covers listing order, disabling, expiry and the
// serveability check the request handler relies on.
func TestListDisableServe(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	first := env.create(t, CreateParams{Description: "1"})
	env.clock.SetTime(env.clock.Now().Add(time.Minute))
	second := env.create(t, CreateParams{
		Description:    "2",
		AbsoluteExpiry: env.clock.Now().Add(time.Hour),
	})

	list, err := env.manager.ListOffers(false)
	require.NoError(t, err)
	require.Equal(t, []OfferID{second.ID, first.ID}, []OfferID{
		list[0].ID, list[1].ID,
	}, "newest first")

	got, err := env.manager.ServeableOffer(first.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, got.ID)

	require.NoError(t, env.manager.DisableOffer(first.ID))
	_, err = env.manager.ServeableOffer(first.ID)
	require.ErrorIs(t, err, ErrOfferDisabled)
	active, err := env.manager.ListOffers(true)
	require.NoError(t, err)
	require.Len(t, active, 1)
	all, err := env.manager.ListOffers(false)
	require.NoError(t, err)
	require.Len(t, all, 2, "disabled offers stay listed")
	require.NoError(t, env.manager.EnableOffer(first.ID))
	_, err = env.manager.ServeableOffer(first.ID)
	require.NoError(t, err)

	// Expiry follows the spec reader: valid at the expiry second, expired
	// one second past it.
	env.clock.SetTime(second.AbsoluteExpiry)
	_, err = env.manager.ServeableOffer(second.ID)
	require.NoError(t, err)
	env.clock.SetTime(second.AbsoluteExpiry.Add(time.Second))
	_, err = env.manager.ServeableOffer(second.ID)
	require.ErrorIs(t, err, ErrOfferExpired)

	_, err = env.manager.ServeableOffer(OfferID{42})
	require.ErrorIs(t, err, ErrOfferNotFound)
	require.ErrorIs(t, env.manager.DisableOffer(OfferID{42}), ErrOfferNotFound)

	require.NoError(t, env.manager.CountInvoice(first.ID))
	require.NoError(t, env.manager.CountInvoice(first.ID))
	got, err = env.manager.LookupOffer(first.ID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), got.InvoicesIssued)
}

// TestDecodeBolt12 decodes our own offer, an offer for another chain, a
// request and an invoice, and rejects junk.
func TestDecodeBolt12(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	rec := env.create(t, CreateParams{Description: "ours"})

	d, err := env.manager.DecodeBolt12("  " + rec.Bolt12 + "\n")
	require.NoError(t, err)
	require.Equal(t, OfferHRP, d.HRP)
	require.NotNil(t, d.Offer)
	require.True(t, d.ForThisChain)
	require.True(t, d.Ours)
	require.NoError(t, d.ValidationError)
	require.Equal(t, rec.ID, *d.OfferID)
	require.Equal(t, [][32]byte{testChain}, d.Chains)

	// Our own offer, expired: still ours and for this chain, not valid.
	expiring := env.create(t, CreateParams{
		Description:    "soon",
		AbsoluteExpiry: env.clock.Now().Add(time.Minute),
	})
	env.clock.SetTime(env.clock.Now().Add(2 * time.Minute))
	d, err = env.manager.DecodeBolt12(expiring.Bolt12)
	require.NoError(t, err)
	require.True(t, d.ForThisChain)
	require.True(t, d.Ours)
	require.ErrorIs(t, d.ValidationError, bolt12.ErrOfferExpired)

	// An offer for another chain decodes but is flagged.
	other := &bolt12.Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{{0xaa}},
			}),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](env.issuer.PubKey()),
		),
	}
	otherBytes, err := other.Encode()
	require.NoError(t, err)
	otherStr, err := bolt12.Encode(OfferHRP, otherBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(otherStr)
	require.NoError(t, err)
	require.False(t, d.ForThisChain)
	require.False(t, d.Ours)
	require.ErrorIs(t, d.ValidationError, bolt12.ErrUnsupportedChain)
	require.Equal(t, [][32]byte{{0xaa}}, d.Chains)

	// An offer listing our chain among others is for this chain.
	multi := &bolt12.Offer{
		OfferChains: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](bolt12.ChainsRecord{
				Chains: [][32]byte{{0xaa}, testChain},
			}),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](env.issuer.PubKey()),
		),
	}
	multiBytes, err := multi.Encode()
	require.NoError(t, err)
	multiStr, err := bolt12.Encode(OfferHRP, multiBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(multiStr)
	require.NoError(t, err)
	require.True(t, d.ForThisChain)
	require.False(t, d.Ours, "no metadata of ours")
	require.NoError(t, d.ValidationError)

	// An offer that names no chain is a Bitcoin offer, not ours.
	bitcoin := &bolt12.Offer{
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](env.issuer.PubKey()),
		),
	}
	bitcoinBytes, err := bitcoin.Encode()
	require.NoError(t, err)
	bitcoinStr, err := bolt12.Encode(OfferHRP, bitcoinBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(bitcoinStr)
	require.NoError(t, err)
	require.False(t, d.ForThisChain)

	// An absent offer_chains means Bitcoin mainnet, so it is reported as
	// naming that rather than as naming nothing. This environment runs on
	// a test chain, so mainnet is still not it.
	require.Equal(t, [][32]byte{bitcoinMainnetGenesis()}, d.Chains)
	require.ErrorIs(t, d.ValidationError, bolt12.ErrUnsupportedChain)

	// A chain-less offer that also fails a check that runs before the
	// chain check is still not for this chain: the flag comes from the
	// data, not from which error the validator happened to return.
	unknownEven := append([]byte{}, bitcoinBytes...)
	unknownEven = append(unknownEven, 24, 1, 0) // type 24, length 1
	unknownStr, err := bolt12.Encode(OfferHRP, unknownEven)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(unknownStr)
	require.NoError(t, err)
	require.False(t, d.ForThisChain)
	require.False(t, d.Ours)
	require.Error(t, d.ValidationError)
	require.NotErrorIs(t, d.ValidationError, bolt12.ErrUnsupportedChain)

	// The same, with a required feature bit nobody knows.
	features := &bolt12.Offer{
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](env.issuer.PubKey()),
		),
		OfferFeatures: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](
				*lnwire.NewRawFeatureVector(lnwire.FeatureBit(1000)),
			),
		),
	}
	featBytes, err := features.Encode()
	require.NoError(t, err)
	featStr, err := bolt12.Encode(OfferHRP, featBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(featStr)
	require.NoError(t, err)
	require.False(t, d.ForThisChain)
	require.Error(t, d.ValidationError)

	// A request for our offer mirrors it and names our chain.
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	offer, err := bolt12.DecodeOffer(rec.Offer)
	require.NoError(t, err)
	ir, err := bolt12.NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{1, 2, 3, 4}, testChain,
	)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(5000)),
	)
	sig, err := bolt12.SignInvoiceRequest(ir, payer)
	require.NoError(t, err)
	ir.Signature = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType240](sig),
	)
	irBytes, err := ir.Encode()
	require.NoError(t, err)
	irStr, err := bolt12.Encode(InvoiceRequestHRP, irBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(irStr)
	require.NoError(t, err)
	require.Equal(t, InvoiceRequestHRP, d.HRP)
	require.NotNil(t, d.InvoiceRequest)
	require.True(t, d.ForThisChain)
	require.Equal(t, rec.ID, *d.OfferID)

	// A request for the other chain's offer, and one for the Bitcoin
	// offer, which names no chain.
	otherIR, err := bolt12.NewInvoiceRequestFromOffer(
		other, payer.PubKey(), []byte{1}, [32]byte{0xaa},
	)
	require.NoError(t, err)
	otherIR.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(5000)),
	)
	irBytes, err = otherIR.Encode()
	require.NoError(t, err)
	irStr, err = bolt12.Encode(InvoiceRequestHRP, irBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(irStr)
	require.NoError(t, err)
	require.False(t, d.ForThisChain)
	require.Equal(t, [][32]byte{{0xaa}}, d.Chains)

	btcIR, err := bolt12.NewInvoiceRequestFromOffer(
		bitcoin, payer.PubKey(), []byte{1}, bolt12.BitcoinMainnetChain(),
	)
	require.NoError(t, err)
	btcIR.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](bolt12.TUint64(5000)),
	)
	irBytes, err = btcIR.Encode()
	require.NoError(t, err)
	irStr, err = bolt12.Encode(InvoiceRequestHRP, irBytes)
	require.NoError(t, err)
	d, err = env.manager.DecodeBolt12(irStr)
	require.NoError(t, err)
	require.False(t, d.ForThisChain)
	require.Empty(t, d.Chains, "the spec default is not written out")

	// Junk and unknown prefixes are refused.
	_, err = env.manager.DecodeBolt12("lnbc1notbolt12")
	require.Error(t, err)
	_, err = env.manager.DecodeBolt12("")
	require.Error(t, err)
}

// TestNewManagerRequirements covers the configuration checks.
func TestNewManagerRequirements(t *testing.T) {
	t.Parallel()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	desc := keychain.KeyDescriptor{PubKey: key.PubKey()}
	store := testStore(t)

	_, err = NewManager(Config{
		ChainHash: testChain, IssuerKey: desc, Secret: testSecret,
	})
	require.Error(t, err, "store")
	_, err = NewManager(Config{
		ChainHash: testChain, Store: store, Secret: testSecret,
	})
	require.Error(t, err, "issuer key")
	_, err = NewManager(Config{
		IssuerKey: desc, Store: store, Secret: testSecret,
	})
	require.Error(t, err, "chain")
	_, err = NewManager(Config{
		ChainHash: testChain, IssuerKey: desc, Store: store,
	})
	require.Error(t, err, "secret")
	m, err := NewManager(Config{
		ChainHash: testChain, IssuerKey: desc, Store: store,
		Secret: testSecret,
	})
	require.NoError(t, err)
	require.Equal(t, testChain, m.ChainHash())
	require.NotNil(t, m.cfg.Clock, "a default clock is provided")
	require.Equal(t, DefaultMaxOffers, m.cfg.MaxOffers)
}

// TestDeriveSecret checks the secret is a pure function of the base
// encryption key, tagged apart from the channel backup key.
func TestDeriveSecret(t *testing.T) {
	t.Parallel()

	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	ring := &fakeKeyRing{key: key}

	a, err := DeriveSecret(ring)
	require.NoError(t, err)
	b, err := DeriveSecret(ring)
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.NotEqual(t, [32]byte{}, a)
	require.Equal(t, keychain.KeyLocator{
		Family: keychain.KeyFamilyBaseEncryption, Index: 0,
	}, ring.lastLoc)

	// Not the plain hash of the key, which is the backup key.
	plain := sha256.Sum256(key.PubKey().SerializeCompressed())
	require.NotEqual(t, plain, a)

	_, err = DeriveSecret(&fakeKeyRing{err: errors.New("no wallet")})
	require.Error(t, err)
}

type fakeKeyRing struct {
	key     *btcec.PrivateKey
	err     error
	lastLoc keychain.KeyLocator
}

func (f *fakeKeyRing) DeriveNextKey(
	keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	return keychain.KeyDescriptor{}, errors.New("not used")
}

func (f *fakeKeyRing) DeriveKey(
	loc keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	f.lastLoc = loc
	if f.err != nil {
		return keychain.KeyDescriptor{}, f.err
	}

	return keychain.KeyDescriptor{
		KeyLocator: loc, PubKey: f.key.PubKey(),
	}, nil
}

// bitcoinMainnetGenesis is the hash an absent offer_chains defaults to.
func bitcoinMainnetGenesis() [32]byte {
	return [32]byte(*chaincfg.MainNetParams.GenesisHash)
}

// An offer that names no chain is for Bitcoin mainnet by the spec's default,
// and since 2026-09-17 that is this chain's chain_hash. So on a node in
// production such an offer is for this chain, and most offers omit the field.
//
// This is the case the old reading got wrong. It used to be right by accident:
// while this chain advertised a chain_hash of its own, "names no chain" and
// "not for this chain" happened to coincide.
func TestOfferWithNoChainsIsForThisChainOnMainnet(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	env.manager.cfg.ChainHash = bitcoinMainnetGenesis()

	offer := &bolt12.Offer{
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](
				env.issuer.PubKey(),
			),
		),
	}
	raw, err := offer.Encode()
	require.NoError(t, err)
	str, err := bolt12.Encode(OfferHRP, raw)
	require.NoError(t, err)

	d, err := env.manager.DecodeBolt12(str)
	require.NoError(t, err)

	require.True(t, d.ForThisChain, "an offer that omits offer_chains is "+
		"for Bitcoin mainnet by the spec's default, which is this "+
		"chain's chain_hash; refusing it would refuse the common case")
	require.NoError(t, d.ValidationError)

	// And the two must agree. They disagreed before: the validator applied
	// the default and this did not.
	require.Equal(t, d.ValidationError == nil, d.ForThisChain)
}
