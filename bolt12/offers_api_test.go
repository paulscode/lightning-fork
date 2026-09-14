package bolt12

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestOfferIDStableAndMirrored checks that an offer's id is a pure function
// of its fields, that offer_metadata changes it, and that the id computed
// from an invoice_request that mirrors the offer equals the offer's own.
func TestOfferIDStableAndMirrored(t *testing.T) {
	t.Parallel()

	issuer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	chain := [32]byte{1, 2, 3}
	mk := func(meta []byte) *Offer {
		o := &Offer{
			OfferChains: tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType2](ChainsRecord{
					Chains: [][32]byte{chain},
				}),
			),
			OfferDescription: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType10](
					tlv.Blob("OCEAN Payouts for bc1q..."),
				),
			),
			OfferIssuerID: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType22](
					issuer.PubKey(),
				),
			),
		}
		if meta != nil {
			o.OfferMetadata = tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType4](
					tlv.Blob(meta),
				),
			)
		}
		return o
	}

	a := mk([]byte{9, 9, 9})
	b := mk([]byte{9, 9, 9})
	c := mk([]byte{8, 8, 8})

	idA, err := OfferID(a)
	require.NoError(t, err)
	idB, err := OfferID(b)
	require.NoError(t, err)
	idC, err := OfferID(c)
	require.NoError(t, err)
	require.Equal(t, idA, idB, "same fields, same id")
	require.NotEqual(t, idA, idC, "metadata tells offers apart")

	// The encoded form decodes back to the same id.
	encoded, err := a.Encode()
	require.NoError(t, err)
	decoded, err := DecodeOffer(encoded)
	require.NoError(t, err)
	idDecoded, err := OfferID(decoded)
	require.NoError(t, err)
	require.Equal(t, idA, idDecoded)

	// A request built from the offer mirrors it, and its offer id matches.
	ir, err := NewInvoiceRequestFromOffer(a, payer.PubKey(), []byte{7}, chain)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](TUint64(1000)),
	)
	idReq, err := RequestOfferID(ir)
	require.NoError(t, err)
	require.Equal(t, idA, idReq, "the request's offer id is the offer's")

	// The invoice derived from the request mirrors the same offer.
	inv := NewInvoiceFromRequest(ir)
	idInv, err := InvoiceOfferID(inv)
	require.NoError(t, err)
	require.Equal(t, idA, idInv)

	// A request with no offer fields has no offer id.
	_, err = RequestOfferID(&InvoiceRequest{})
	require.Error(t, err)
}

// TestBitcoinDefaultChainPinned pins the chain BOLT 12 assumes when none is
// named. The btcd fork this tree builds against keeps Bitcoin's genesis, and
// this test fails the day it stops doing so, because then a chain-less
// offer would be taken for one of ours.
func TestBitcoinDefaultChainPinned(t *testing.T) {
	t.Parallel()

	want, err := hex.DecodeString(
		"6fe28c0ab6f1b372c1a6a246ae63f74f931e8365e15a089c68d6190000000000",
	)
	require.NoError(t, err)
	btc := BitcoinMainnetChain()
	require.Equal(t, want, btc[:])
	require.Equal(t, [32]byte(*chaincfg.MainNetParams.GenesisHash),
		BitcoinMainnetChain())
}

// TestWriteOnChain checks the chain-pinned writer validators: a message that
// names no chain, or another chain, is refused; one naming the active chain
// passes.
func TestWriteOnChain(t *testing.T) {
	t.Parallel()

	issuer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	payer, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	ours := [32]byte{0xb2, 1}
	other := [32]byte{0xb2, 2}

	mkOffer := func(chains [][32]byte) *Offer {
		o := &Offer{
			OfferDescription: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType10](
					tlv.Blob("test"),
				),
			),
			OfferIssuerID: tlv.SomeRecordT(
				tlv.NewPrimitiveRecord[tlv.TlvType22](
					issuer.PubKey(),
				),
			),
		}
		if chains != nil {
			o.OfferChains = tlv.SomeRecordT(
				tlv.NewRecordT[tlv.TlvType2](ChainsRecord{
					Chains: chains,
				}),
			)
		}
		return o
	}

	// Offers.
	require.ErrorIs(t, ValidateOfferWriteOnChain(mkOffer(nil), ours),
		ErrChainNotNamed, "chain-less offer means Bitcoin")
	require.ErrorIs(t,
		ValidateOfferWriteOnChain(mkOffer([][32]byte{other}), ours),
		ErrUnsupportedChain)
	require.NoError(t,
		ValidateOfferWriteOnChain(mkOffer([][32]byte{ours}), ours))
	require.NoError(t, ValidateOfferWriteOnChain(
		mkOffer([][32]byte{other, ours}), ours,
	), "any listed chain may be ours")

	// The upstream writer checks still run first.
	bad := mkOffer([][32]byte{ours})
	bad.OfferDescription = tlv.OptionalRecordT[tlv.TlvType10, tlv.Blob]{}
	bad.OfferAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType8](TUint64(1000)),
	)
	require.ErrorIs(t, ValidateOfferWriteOnChain(bad, ours),
		ErrMissingDescription)

	// Requests: built from our offer, naming our chain.
	offer := mkOffer([][32]byte{ours})
	ir, err := NewInvoiceRequestFromOffer(
		offer, payer.PubKey(), []byte{1, 2, 3}, ours,
	)
	require.NoError(t, err)
	ir.InvreqAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType82](TUint64(1000)),
	)
	require.NoError(t, ValidateInvoiceRequestWriteOnChain(ir, ours))
	require.ErrorIs(t, ValidateInvoiceRequestWriteOnChain(ir, other),
		ErrUnsupportedChain)

	// A request that drops invreq_chain is refused by us even though the
	// upstream writer check passes for it when the offer has no chains.
	spontaneous := &InvoiceRequest{
		InvreqMetadata: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType0](tlv.Blob{1}),
		),
		OfferDescription: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType10](tlv.Blob("x")),
		),
		OfferIssuerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType22](issuer.PubKey()),
		),
		InvreqAmount: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType82](TUint64(1000)),
		),
		InvreqPayerID: tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType88](payer.PubKey()),
		),
	}
	require.NoError(t, ValidateInvoiceRequestWrite(spontaneous),
		"upstream cannot know the chain is wrong")
	require.ErrorIs(t,
		ValidateInvoiceRequestWriteOnChain(spontaneous, ours),
		ErrChainNotNamed)

	// Invoices: derived from the request, then with the chain dropped.
	inv := NewInvoiceFromRequest(ir)
	inv.InvoiceCreatedAt = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType164](TUint64(1_800_000_000)),
	)
	inv.InvoiceAmount = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType170](TUint64(1000)),
	)
	inv.InvoicePaymentHash = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType168]([32]byte{9}),
	)
	inv.InvoiceNodeID = tlv.SomeRecordT(
		tlv.NewPrimitiveRecord[tlv.TlvType176](issuer.PubKey()),
	)
	intro, err := lnwire.NewPubkeyIntro(issuer.PubKey())
	require.NoError(t, err)
	inv.InvoicePaths = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType160](lnwire.BlindedPaths{
			Paths: []lnwire.BlindedPath{{
				IntroductionNode: intro,
				BlindingPoint:    payer.PubKey(),
				Hops: []lnwire.BlindedHop{{
					BlindedNodeID: issuer.PubKey(),
					EncryptedData: []byte{1, 2, 3},
				}},
			}},
		}),
	)
	inv.InvoiceBlindedPay = tlv.SomeRecordT(
		tlv.NewRecordT[tlv.TlvType162](BlindedPayInfos{
			Infos: []BlindedPayInfo{{
				CltvExpiryDelta: 80,
				HtlcMaximumMsat: 1_000_000,
			}},
		}),
	)
	require.NoError(t, ValidateInvoiceWriteOnChain(inv, ours))
	require.ErrorIs(t, ValidateInvoiceWriteOnChain(inv, other),
		ErrUnsupportedChain)
	inv.InvreqChain = tlv.OptionalRecordT[tlv.TlvType80, [32]byte]{}
	require.ErrorIs(t, ValidateInvoiceWriteOnChain(inv, ours),
		ErrChainNotNamed)
}

// TestOfferIDMatchesCoreLightning pins the offer id to Core Lightning's: an
// offer minted by this node, decoded by an unmodified lightningd (v25.02)
// on 2026-09-14, whose decode reported this offer_id.
func TestOfferIDMatchesCoreLightning(t *testing.T) {
	t.Parallel()

	const (
		lno   = "lno1qgsp42cfmy5d53te84hklymmj60rhphsgqux95x72c5f59jr002egfgyzqvgkdva63tzw0scz6n2wzs0pe3s5rnsv96xsmr9wdejqurjda3x293pqw0dqrx3xcv2dy2fdlpp9k3cduvkgcdtmt4lfsmym5l0apks6hkax"
		clnID = "7dad0c9d067b4dcc867ab926ce43daf2454397d8057d110b2ed5be06d79940d8"
	)
	_, data, err := Decode(lno)
	require.NoError(t, err)
	o, err := DecodeOffer(data)
	require.NoError(t, err)
	id, err := OfferID(o)
	require.NoError(t, err)
	require.Equal(t, clnID, hex.EncodeToString(id[:]))

	// The raw bytes give the same, so the records re-encode faithfully.
	raw := sha256.Sum256(data)
	require.Equal(t, raw, id)
}
