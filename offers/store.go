package offers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// offersBucket is the top-level bucket that holds one record per
	// offer, keyed by the offer id.
	offersBucket = []byte("offers-v1")

	// ErrOfferNotFound is returned when no offer has the given id.
	ErrOfferNotFound = errors.New("offer not found")

	// ErrOfferExists is returned when an offer with the same id is
	// already stored. Ids are Merkle roots of the offer's fields, so this
	// means the same offer, not a collision.
	ErrOfferExists = errors.New("offer already exists")
)

// OfferID identifies an offer: the BOLT 12 Merkle root of its fields.
type OfferID [32]byte

// String returns the hex form of the id.
func (id OfferID) String() string {
	return fmt.Sprintf("%x", id[:])
}

// Record is what the node keeps about an offer it issued. The encoded offer
// is the source of truth; the other fields are what the node needs to serve
// it and to show it without decoding.
type Record struct {
	// ID is the offer id, the Merkle root of the offer's records.
	ID OfferID

	// Offer is the offer's TLV encoding, exactly as published.
	Offer []byte

	// Bolt12 is the lno1... string, exactly as published.
	Bolt12 string

	// Description is offer_description, or empty when the offer has none.
	Description string

	// AmountMsat is offer_amount in millisatoshi, or zero for an offer
	// that lets the payer choose ("any").
	AmountMsat uint64

	// AbsoluteExpiry is offer_absolute_expiry, or the zero time for an
	// offer that does not expire.
	AbsoluteExpiry time.Time

	// CreatedAt is when the node minted the offer.
	CreatedAt time.Time

	// Active is false once the operator disabled the offer: requests for
	// it are refused, but it stays listed.
	Active bool

	// IssuerKeyFamily and IssuerKeyIndex locate the key that signs the
	// offer's invoices (offer_issuer_id) in the node's key ring.
	IssuerKeyFamily uint32
	IssuerKeyIndex  uint32

	// PathSecret is a per-offer secret from which the path_id of the
	// offer's blinded paths is derived, so an onion message that reaches
	// us through one of them identifies the offer.
	PathSecret [32]byte

	// InvoicesIssued counts the invoices the node has issued for the
	// offer.
	InvoicesIssued uint64

	// Label is an operator-chosen note, not part of the offer.
	Label string
}

// Store persists offer records.
type Store interface {
	// Put stores a new record. It fails with ErrOfferExists if the id is
	// taken.
	Put(record *Record) error

	// Get returns the record with the given id, or ErrOfferNotFound.
	Get(id OfferID) (*Record, error)

	// List returns every record, in no particular order.
	List() ([]*Record, error)

	// Update applies fn to the stored record under the store's lock and
	// writes the result back. fn sees a copy; returning an error leaves
	// the record untouched.
	Update(id OfferID, fn func(record *Record) error) error

	// Delete removes the record with the given id.
	Delete(id OfferID) error
}

// Record TLV types. Primitive records only, so the encoding needs nothing
// beyond the tlv package.
const (
	typeOffer          tlv.Type = 1
	typeBolt12         tlv.Type = 2
	typeDescription    tlv.Type = 3
	typeAmountMsat     tlv.Type = 4
	typeAbsoluteExpiry tlv.Type = 5
	typeCreatedAt      tlv.Type = 6
	typeActive         tlv.Type = 7
	typeIssuerFamily   tlv.Type = 8
	typeIssuerIndex    tlv.Type = 9
	typePathSecret     tlv.Type = 10
	typeInvoicesIssued tlv.Type = 11
	typeLabel          tlv.Type = 12
)

// serialize encodes a record for storage.
func serialize(r *Record) ([]byte, error) {
	var (
		offer      = r.Offer
		bolt12     = []byte(r.Bolt12)
		desc       = []byte(r.Description)
		amount     = r.AmountMsat
		expiry     = uint64(0)
		created    = uint64(r.CreatedAt.Unix())
		active     = uint8(0)
		family     = r.IssuerKeyFamily
		index      = r.IssuerKeyIndex
		pathSecret = r.PathSecret
		issued     = r.InvoicesIssued
		label      = []byte(r.Label)
	)
	if !r.AbsoluteExpiry.IsZero() {
		expiry = uint64(r.AbsoluteExpiry.Unix())
	}
	if r.Active {
		active = 1
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(typeOffer, &offer),
		tlv.MakePrimitiveRecord(typeBolt12, &bolt12),
		tlv.MakePrimitiveRecord(typeDescription, &desc),
		tlv.MakePrimitiveRecord(typeAmountMsat, &amount),
		tlv.MakePrimitiveRecord(typeAbsoluteExpiry, &expiry),
		tlv.MakePrimitiveRecord(typeCreatedAt, &created),
		tlv.MakePrimitiveRecord(typeActive, &active),
		tlv.MakePrimitiveRecord(typeIssuerFamily, &family),
		tlv.MakePrimitiveRecord(typeIssuerIndex, &index),
		tlv.MakePrimitiveRecord(typePathSecret, &pathSecret),
		tlv.MakePrimitiveRecord(typeInvoicesIssued, &issued),
		tlv.MakePrimitiveRecord(typeLabel, &label),
	}
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// deserialize decodes a stored record. Unknown types are tolerated so a
// newer node's records still read on an older one.
func deserialize(id OfferID, data []byte) (*Record, error) {
	var (
		offer      []byte
		bolt12     []byte
		desc       []byte
		amount     uint64
		expiry     uint64
		created    uint64
		active     uint8
		family     uint32
		index      uint32
		pathSecret [32]byte
		issued     uint64
		label      []byte
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(typeOffer, &offer),
		tlv.MakePrimitiveRecord(typeBolt12, &bolt12),
		tlv.MakePrimitiveRecord(typeDescription, &desc),
		tlv.MakePrimitiveRecord(typeAmountMsat, &amount),
		tlv.MakePrimitiveRecord(typeAbsoluteExpiry, &expiry),
		tlv.MakePrimitiveRecord(typeCreatedAt, &created),
		tlv.MakePrimitiveRecord(typeActive, &active),
		tlv.MakePrimitiveRecord(typeIssuerFamily, &family),
		tlv.MakePrimitiveRecord(typeIssuerIndex, &index),
		tlv.MakePrimitiveRecord(typePathSecret, &pathSecret),
		tlv.MakePrimitiveRecord(typeInvoicesIssued, &issued),
		tlv.MakePrimitiveRecord(typeLabel, &label),
	)
	if err != nil {
		return nil, err
	}
	if _, err := stream.DecodeWithParsedTypes(
		bytes.NewReader(data),
	); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(offer) == 0 {
		return nil, fmt.Errorf("offer %v: record holds no offer", id)
	}

	r := &Record{
		ID:              id,
		Offer:           offer,
		Bolt12:          string(bolt12),
		Description:     string(desc),
		AmountMsat:      amount,
		CreatedAt:       time.Unix(int64(created), 0).UTC(),
		Active:          active == 1,
		IssuerKeyFamily: family,
		IssuerKeyIndex:  index,
		PathSecret:      pathSecret,
		InvoicesIssued:  issued,
		Label:           string(label),
	}
	if expiry != 0 {
		r.AbsoluteExpiry = time.Unix(int64(expiry), 0).UTC()
	}

	return r, nil
}

// KVStore keeps offer records in a kvdb bucket.
type KVStore struct {
	db kvdb.Backend
}

// NewKVStore returns a store over the given backend, creating its bucket.
func NewKVStore(db kvdb.Backend) (*KVStore, error) {
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(offersBucket)
		return err
	}, func() {})
	if err != nil {
		return nil, fmt.Errorf("create offers bucket: %w", err)
	}

	return &KVStore{db: db}, nil
}

// A compile-time check that KVStore satisfies Store.
var _ Store = (*KVStore)(nil)

// Put stores a new record.
func (s *KVStore) Put(record *Record) error {
	data, err := serialize(record)
	if err != nil {
		return err
	}

	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(offersBucket)
		if bucket == nil {
			return errors.New("offers bucket missing")
		}
		if bucket.Get(record.ID[:]) != nil {
			return ErrOfferExists
		}

		return bucket.Put(record.ID[:], data)
	}, func() {})
}

// Get returns one record.
func (s *KVStore) Get(id OfferID) (*Record, error) {
	var record *Record
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(offersBucket)
		if bucket == nil {
			return ErrOfferNotFound
		}
		data := bucket.Get(id[:])
		if data == nil {
			return ErrOfferNotFound
		}
		var err error
		record, err = deserialize(id, data)

		return err
	}, func() { record = nil })
	if err != nil {
		return nil, err
	}

	return record, nil
}

// List returns every record.
func (s *KVStore) List() ([]*Record, error) {
	var records []*Record
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(offersBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			if len(k) != 32 || v == nil {
				return nil
			}
			var id OfferID
			copy(id[:], k)
			record, err := deserialize(id, v)
			if err != nil {
				// One damaged record must not hide the rest;
				// Get still reports it for its own id.
				log.Errorf("Skipping offer %v that does not "+
					"deserialize: %v", id, err)

				return nil
			}
			records = append(records, record)

			return nil
		})
	}, func() { records = nil })
	if err != nil {
		return nil, err
	}

	return records, nil
}

// Update rewrites one record through fn.
func (s *KVStore) Update(id OfferID, fn func(record *Record) error) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(offersBucket)
		if bucket == nil {
			return ErrOfferNotFound
		}
		data := bucket.Get(id[:])
		if data == nil {
			return ErrOfferNotFound
		}
		record, err := deserialize(id, data)
		if err != nil {
			return err
		}
		if err := fn(record); err != nil {
			return err
		}
		record.ID = id
		updated, err := serialize(record)
		if err != nil {
			return err
		}

		return bucket.Put(id[:], updated)
	}, func() {})
}

// Delete removes one record.
func (s *KVStore) Delete(id OfferID) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(offersBucket)
		if bucket == nil {
			return ErrOfferNotFound
		}
		if bucket.Get(id[:]) == nil {
			return ErrOfferNotFound
		}

		return bucket.Delete(id[:])
	}, func() {})
}
