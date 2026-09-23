package offers

import (
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/bolt12"
)

var (
	// ErrWrongPath is returned when a request for an offer did not arrive
	// the way the offer says it must: over one of the offer's blinded
	// paths when it has them, and not over a blinded path when it does
	// not.
	ErrWrongPath = errors.New("request did not arrive over the offer's " +
		"path")

	// ErrNotOurOffer is returned when a request mirrors an offer this
	// node did not mint.
	ErrNotOurOffer = errors.New("offer was not minted by this node")
)

// OfferFromRequest rebuilds the offer an invoice request mirrors, from the
// offer fields the request carries.
func OfferFromRequest(ir *bolt12.InvoiceRequest) *bolt12.Offer {
	return &bolt12.Offer{
		OfferChains:         ir.OfferChains,
		OfferMetadata:       ir.OfferMetadata,
		OfferCurrency:       ir.OfferCurrency,
		OfferAmount:         ir.OfferAmount,
		OfferDescription:    ir.OfferDescription,
		OfferFeatures:       ir.OfferFeatures,
		OfferAbsoluteExpiry: ir.OfferAbsoluteExpiry,
		OfferPaths:          ir.OfferPaths,
		OfferIssuer:         ir.OfferIssuer,
		OfferQuantityMax:    ir.OfferQuantityMax,
		OfferIssuerID:       ir.OfferIssuerID,
	}
}

// OfferForRequest returns the stored offer a request is for, once the
// request is known to be for one of ours that may be served: the offer it
// mirrors verifies as minted by this node, it arrived the way the offer
// requires, and the offer is stored, enabled and unexpired. An offer that
// verifies as ours but is not stored, because the node was restored from
// seed, is stored again first.
func (m *Manager) OfferForRequest(ir *bolt12.InvoiceRequest,
	pathID []byte) (*Record, *bolt12.Offer, error) {

	offer := OfferFromRequest(ir)
	if !m.IsOurs(offer) {
		return nil, nil, ErrNotOurOffer
	}

	// The spec's arrival rule: with offer_paths, only requests over one
	// of them count; without, only requests not over a blinded path.
	if offer.OfferPaths.IsSome() {
		want, err := m.PathSecretFor(offer)
		if err != nil {
			return nil, nil, err
		}
		if len(pathID) != len(want) ||
			subtle.ConstantTimeCompare(pathID, want[:]) != 1 {

			return nil, nil, ErrWrongPath
		}
	} else if len(pathID) != 0 {
		return nil, nil, ErrWrongPath
	}

	id, err := bolt12.OfferID(offer)
	if err != nil {
		return nil, nil, fmt.Errorf("offer id: %w", err)
	}
	record, err := m.ServeableOffer(OfferID(id))
	if errors.Is(err, ErrOfferNotFound) {
		record, err = m.RestoreOffer(offer)
		if err != nil {
			return nil, nil, err
		}
		record, err = m.ServeableOffer(record.ID)
	}
	if err != nil {
		return nil, nil, err
	}

	return record, offer, nil
}

// RestoreOffer stores a record for an offer this node minted whose record
// is gone, as after a restore from seed. The offer must verify as ours and
// pass the reader checks for this chain and time.
func (m *Manager) RestoreOffer(offer *bolt12.Offer) (*Record, error) {
	if !m.IsOurs(offer) {
		return nil, ErrNotOurOffer
	}
	now := m.cfg.Clock.Now().Truncate(0).UTC()
	if err := bolt12.ValidateOfferRead(
		offer, now, m.cfg.ChainHash, bolt12.Blake2bFeatures,
	); err != nil {
		return nil, fmt.Errorf("offer does not validate: %w", err)
	}
	encoded, err := offer.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode offer: %w", err)
	}
	id, err := bolt12.OfferID(offer)
	if err != nil {
		return nil, fmt.Errorf("offer id: %w", err)
	}
	str, err := bolt12.Encode(OfferHRP, encoded)
	if err != nil {
		return nil, fmt.Errorf("bech32 encode: %w", err)
	}
	pathSecret, err := m.PathSecretFor(offer)
	if err != nil {
		return nil, err
	}

	record := &Record{
		ID:              OfferID(id),
		Offer:           encoded,
		Bolt12:          str,
		Description:     string(optBlob(offer.OfferDescription)),
		AmountMsat:      uint64(offer.OfferAmount.ValOpt().UnwrapOr(0)),
		CreatedAt:       now.Truncate(1e9),
		Active:          true,
		IssuerKeyFamily: uint32(m.cfg.IssuerKey.Family),
		IssuerKeyIndex:  m.cfg.IssuerKey.Index,
		PathSecret:      pathSecret,
		Label:           "restored from a request",
	}
	offer.OfferAbsoluteExpiry.WhenSome(
		func(r tlv14) { record.AbsoluteExpiry = unixTime(uint64(r.Val)) },
	)
	err = m.cfg.Store.Put(record)
	if err != nil && !errors.Is(err, ErrOfferExists) {
		return nil, err
	}
	// A record that was lost may have been disabled before it was lost;
	// that is not knowable here, so the offer comes back enabled.
	log.Warnf("Restored offer %v from a request (%s); it is enabled, "+
		"disable it again if it had been", record.ID, describe(record))

	return m.cfg.Store.Get(record.ID)
}
