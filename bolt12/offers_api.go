package bolt12

import (
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

// OfferID returns the offer's identifier: the BOLT 12 Merkle root of its
// signable records. Two offers with the same fields have the same id, which
// is why an offer meant to be distinct carries offer_metadata.
func OfferID(o *Offer) ([32]byte, error) {
	return merkleRoot(signableTLVs(o.AllRecords()))
}

// RequestOfferID returns the id of the offer an invoice_request mirrors: the
// Merkle root over the request's records in the offer range (1-79), which
// equals OfferID of the offer when the request mirrors it faithfully. A
// request that carries no offer fields at all has no offer, and an error is
// returned.
func RequestOfferID(ir *InvoiceRequest) ([32]byte, error) {
	var offerRecords []tlv.Record
	for _, r := range ir.AllRecords() {
		if r.Type() >= 1 && r.Type() < offerFieldsEnd {
			offerRecords = append(offerRecords, r)
		}
	}
	if len(offerRecords) == 0 {
		return [32]byte{}, fmt.Errorf("invoice request carries no " +
			"offer fields")
	}

	return merkleRoot(signableTLVs(offerRecords))
}

// InvoiceOfferID returns the id of the offer an invoice mirrors, by the same
// rule as RequestOfferID.
func InvoiceOfferID(inv *Invoice) ([32]byte, error) {
	var offerRecords []tlv.Record
	for _, r := range inv.AllRecords() {
		if r.Type() >= 1 && r.Type() < offerFieldsEnd {
			offerRecords = append(offerRecords, r)
		}
	}
	if len(offerRecords) == 0 {
		return [32]byte{}, fmt.Errorf("invoice carries no offer " +
			"fields")
	}

	return merkleRoot(signableTLVs(offerRecords))
}

// MerkleRoot exposes the BOLT 12 Merkle root of a record set, for callers
// that build their own signatures or ids.
func MerkleRoot(records []tlv.Record) ([32]byte, error) {
	return merkleRoot(signableTLVs(records))
}

// ErrChainNotNamed is returned by the write-side chain checks below when a
// message names no chain at all. Per BOLT 12 an absent chain means Bitcoin
// mainnet, so a node on any other chain must never emit one.
var ErrChainNotNamed = errors.New("message names no chain; an absent chain " +
	"means Bitcoin mainnet")

// BitcoinMainnetChain returns the chain hash BOLT 12 assumes when a message
// names none. Callers on another chain compare against it to make sure they
// never fall back to it by accident.
func BitcoinMainnetChain() [32]byte {
	return bitcoinMainnetGenesisHash
}

// ValidateOfferWriteOnChain runs the upstream writer checks and then insists
// the offer names activeChain in offer_chains. The upstream validator has
// no chain context, so on its own it lets a node write an offer that a payer
// reads as a Bitcoin mainnet offer.
func ValidateOfferWriteOnChain(o *Offer, activeChain [32]byte) error {
	if err := ValidateOfferWrite(o); err != nil {
		return err
	}
	if !o.OfferChains.IsSome() {
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
// insists the request names activeChain in invreq_chain.
func ValidateInvoiceRequestWriteOnChain(ir *InvoiceRequest,
	activeChain [32]byte) error {

	if err := ValidateInvoiceRequestWrite(ir); err != nil {
		return err
	}
	if !ir.InvreqChain.IsSome() {
		return ErrChainNotNamed
	}
	if getInvreqChain(ir) != activeChain {
		return ErrUnsupportedChain
	}

	return nil
}

// ValidateInvoiceWriteOnChain runs the upstream writer checks and then
// insists the invoice names activeChain in invreq_chain.
func ValidateInvoiceWriteOnChain(inv *Invoice, activeChain [32]byte) error {
	if err := ValidateInvoiceWrite(inv); err != nil {
		return err
	}
	if !inv.InvreqChain.IsSome() {
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
