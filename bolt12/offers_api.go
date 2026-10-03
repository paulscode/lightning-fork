package bolt12

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/tlv"
)

// This file is not from LND master: it exports the two things an offers layer
// needs from the codecs, so the ported files can stay byte-identical to
// upstream. Everything here is a thin wrapper over unexported functions.

// offerFieldsEnd is the first TLV type that is not part of an offer. BOLT 12
// reserves types 1-79 for the offer, which an invoice_request and an invoice
// mirror; an offer's identity is the Merkle root of exactly those records.
const offerFieldsEnd tlv.Type = 80

// DecodeOffer decodes the raw TLV bytes of an offer, as produced by Decode on
// an lno1... string.
func DecodeOffer(data []byte) (*Offer, error) {
	return decodeOffer(data)
}

// OfferID returns the offer's identifier: the SHA256 of the offer's TLV
// bytes. BOLT 12 does not define an offer id; this is Core Lightning's
// definition, so an id shown here matches what its tools show for the same
// offer. Two offers with the same fields have the same id, which is why an
// offer meant to be distinct carries offer_metadata.
func OfferID(o *Offer) ([32]byte, error) {
	return tlvDigest(o.AllRecords())
}

// RequestOfferID returns the id of the offer an invoice_request mirrors: the
// digest of the request's records in the offer range (1-79), which equals
// OfferID of the offer when the request mirrors it faithfully. A request
// that carries no offer fields at all has no offer, and an error is
// returned.
func RequestOfferID(ir *InvoiceRequest) ([32]byte, error) {
	offerRecords := offerRange(ir.AllRecords())
	if len(offerRecords) == 0 {
		return [32]byte{}, fmt.Errorf("invoice request carries no " +
			"offer fields")
	}

	return tlvDigest(offerRecords)
}

// InvoiceOfferID returns the id of the offer an invoice mirrors, by the same
// rule as RequestOfferID.
func InvoiceOfferID(inv *Invoice) ([32]byte, error) {
	offerRecords := offerRange(inv.AllRecords())
	if len(offerRecords) == 0 {
		return [32]byte{}, fmt.Errorf("invoice carries no offer " +
			"fields")
	}

	return tlvDigest(offerRecords)
}

// offerRange keeps the records in the offer range, types 1 to 79.
func offerRange(records []tlv.Record) []tlv.Record {
	var out []tlv.Record
	for _, r := range records {
		if r.Type() >= 1 && r.Type() < offerFieldsEnd {
			out = append(out, r)
		}
	}

	return out
}

// tlvDigest is the SHA256 of the records' TLV encoding, without the writer
// checks, so a foreign or invalid offer can still be named.
func tlvDigest(records []tlv.Record) ([32]byte, error) {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return [32]byte{}, err
	}
	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return [32]byte{}, err
	}

	return sha256.Sum256(b.Bytes()), nil
}

// MerkleRoot exposes the BOLT 12 Merkle root of a record set, for callers
// that build their own signatures or ids.
func MerkleRoot(records []tlv.Record) ([32]byte, error) {
	return merkleRoot(signableTLVs(records))
}

// ErrChainNotNamed is returned by the write-side chain checks below when a
// message names no chain at all. Per BOLT 12 an absent chain means the chain
// that starts at Bitcoin's genesis block, which the SHA256 and BLAKE2b
// mainnets share, so a node on any other network (testnet, signet, regtest)
// must never emit one.
//
// The text names the chains by their proof of work rather than calling one
// of them Bitcoin: which chain that is depends on who is reading.
var ErrChainNotNamed = errors.New("message names no chain, which BOLT 12 " +
	"reads as mainnet (the SHA256 and BLAKE2b chains share its genesis " +
	"block), not this network")

// absentChainIsOurs reports whether a message that names no chain names the
// active one anyway: when the active chain's hash is Bitcoin mainnet's genesis
// hash, which is what an absent chain means. Lightning Fork's mainnet shares
// that genesis block, so there an absent chain is exactly right, and BOLT 12
// asks writers to omit it.
func absentChainIsOurs(activeChain [32]byte) bool {
	return activeChain == bitcoinMainnetGenesisHash
}

// BitcoinMainnetChain returns the chain hash BOLT 12 assumes when a message
// names none. Callers on another chain compare against it to make sure they
// never fall back to it by accident.
func BitcoinMainnetChain() [32]byte {
	return bitcoinMainnetGenesisHash
}

// ValidateOfferWriteOnChain runs the upstream writer checks and then insists
// the offer names activeChain in offer_chains, or names no chain where that
// means activeChain (see absentChainIsOurs). The upstream validator has
// no chain context, so on its own it lets a node write an offer that a payer
// reads as a Bitcoin mainnet offer.
func ValidateOfferWriteOnChain(o *Offer, activeChain [32]byte) error {
	if err := ValidateOfferWrite(o); err != nil {
		return err
	}
	if !o.OfferChains.IsSome() {
		if absentChainIsOurs(activeChain) {
			return nil
		}

		return ErrChainNotNamed
	}
	for _, chain := range getOfferChains(o) {
		if chain == activeChain {
			return nil
		}
	}

	return ErrUnsupportedChain
}

// ValidateInvoiceRequestWriteOnChain runs the upstream writer checks and then
// insists the request names activeChain in invreq_chain, or names no chain
// where that means activeChain.
func ValidateInvoiceRequestWriteOnChain(ir *InvoiceRequest,
	activeChain [32]byte) error {

	if err := ValidateInvoiceRequestWrite(ir); err != nil {
		return err
	}
	if !ir.InvreqChain.IsSome() {
		if absentChainIsOurs(activeChain) {
			return nil
		}

		return ErrChainNotNamed
	}
	if getInvreqChain(ir) != activeChain {
		return ErrUnsupportedChain
	}

	return nil
}

// ValidateInvoiceWriteOnChain runs the upstream writer checks and then
// insists the invoice names activeChain in invreq_chain, or names no chain
// where that means activeChain.
func ValidateInvoiceWriteOnChain(inv *Invoice, activeChain [32]byte) error {
	if err := ValidateInvoiceWrite(inv); err != nil {
		return err
	}
	if !inv.InvreqChain.IsSome() {
		if absentChainIsOurs(activeChain) {
			return nil
		}

		return ErrChainNotNamed
	}
	var chain [32]byte
	inv.InvreqChain.WhenSome(
		func(r tlv.RecordT[tlv.TlvType80, [32]byte]) {
			chain = r.Val
		},
	)
	if chain != activeChain {
		return ErrUnsupportedChain
	}

	return nil
}
