package offers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// invoicesBucket keys the invoices issued for offers by payment hash.
	invoicesBucket = []byte("offer-invoices-v1")

	// ErrInvoiceNotFound is returned when no issued invoice has the
	// payment hash.
	ErrInvoiceNotFound = errors.New("offer invoice not found")
)

// IssuedInvoice records a BOLT 12 invoice this node issued for an offer.
// The invoice itself lives in the invoice registry under the same payment
// hash; this keeps what the registry does not: which offer and payer it was
// for, and the signed BOLT 12 string.
type IssuedInvoice struct {
	// PaymentHash is the invoice's payment hash.
	PaymentHash [32]byte

	// OfferID is the offer the invoice was issued for.
	OfferID OfferID

	// PayerID is the payer's key from the request.
	PayerID *btcec.PublicKey

	// AmountMsat is the invoiced amount.
	AmountMsat uint64

	// Quantity is the quantity requested, or zero.
	Quantity uint64

	// CreatedAt is when the invoice was issued.
	CreatedAt time.Time

	// Bolt12 is the signed invoice as an lni1... string.
	Bolt12 string
}

// InvoiceStore persists issued invoices.
type InvoiceStore struct {
	db kvdb.Backend
}

// NewInvoiceStore returns a store over the given backend.
func NewInvoiceStore(db kvdb.Backend) (*InvoiceStore, error) {
	err := kvdb.Update(db, func(tx kvdb.RwTx) error {
		_, err := tx.CreateTopLevelBucket(invoicesBucket)

		return err
	}, func() {})
	if err != nil {
		return nil, fmt.Errorf("offer invoices bucket: %w", err)
	}

	return &InvoiceStore{db: db}, nil
}

// Put stores an issued invoice.
func (s *InvoiceStore) Put(inv *IssuedInvoice) error {
	data, err := serializeInvoice(inv)
	if err != nil {
		return err
	}

	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(invoicesBucket)
		if bucket == nil {
			return errors.New("offer invoices bucket missing")
		}

		return bucket.Put(inv.PaymentHash[:], data)
	}, func() {})
}

// Get returns the issued invoice with the payment hash.
func (s *InvoiceStore) Get(hash [32]byte) (*IssuedInvoice, error) {
	var inv *IssuedInvoice
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(invoicesBucket)
		if bucket == nil {
			return ErrInvoiceNotFound
		}
		data := bucket.Get(hash[:])
		if data == nil {
			return ErrInvoiceNotFound
		}
		var err error
		inv, err = deserializeInvoice(hash, data)

		return err
	}, func() { inv = nil })
	if err != nil {
		return nil, err
	}

	return inv, nil
}

// Delete removes an issued invoice's record.
func (s *InvoiceStore) Delete(hash [32]byte) error {
	return kvdb.Update(s.db, func(tx kvdb.RwTx) error {
		bucket := tx.ReadWriteBucket(invoicesBucket)
		if bucket == nil {
			return nil
		}

		return bucket.Delete(hash[:])
	}, func() {})
}

// ListForOffer returns the invoices issued for an offer, oldest first.
func (s *InvoiceStore) ListForOffer(id OfferID) ([]*IssuedInvoice, error) {
	return s.list(&id)
}

// List returns every issued invoice, oldest first.
func (s *InvoiceStore) List() ([]*IssuedInvoice, error) {
	return s.list(nil)
}

// list returns the issued invoices, for one offer when id is set, oldest
// first.
func (s *InvoiceStore) list(id *OfferID) ([]*IssuedInvoice, error) {
	var out []*IssuedInvoice
	err := kvdb.View(s.db, func(tx kvdb.RTx) error {
		bucket := tx.ReadBucket(invoicesBucket)
		if bucket == nil {
			return nil
		}

		return bucket.ForEach(func(k, v []byte) error {
			if len(k) != 32 || v == nil {
				return nil
			}
			var hash [32]byte
			copy(hash[:], k)
			inv, err := deserializeInvoice(hash, v)
			if err != nil {
				log.Errorf("Skipping offer invoice %x that does "+
					"not deserialize: %v", hash, err)

				return nil
			}
			if id == nil || inv.OfferID == *id {
				out = append(out, inv)
			}

			return nil
		})
	}, func() { out = nil })
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.Before(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}

	return out, nil
}

// The invoice record's TLV types.
const (
	invTypeOfferID   tlv.Type = 1
	invTypePayerID   tlv.Type = 2
	invTypeAmount    tlv.Type = 3
	invTypeCreatedAt tlv.Type = 4
	invTypeBolt12    tlv.Type = 5
	invTypeQuantity  tlv.Type = 6
)

func serializeInvoice(inv *IssuedInvoice) ([]byte, error) {
	if inv.PayerID == nil {
		return nil, errors.New("issued invoice without payer id")
	}
	offerID := inv.OfferID[:]
	payer := inv.PayerID
	created := uint64(inv.CreatedAt.Unix())
	str := []byte(inv.Bolt12)
	amount, quantity := inv.AmountMsat, inv.Quantity

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(invTypeOfferID, &offerID),
		tlv.MakePrimitiveRecord(invTypePayerID, &payer),
		tlv.MakePrimitiveRecord(invTypeAmount, &amount),
		tlv.MakePrimitiveRecord(invTypeCreatedAt, &created),
		tlv.MakePrimitiveRecord(invTypeBolt12, &str),
		tlv.MakePrimitiveRecord(invTypeQuantity, &quantity),
	)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func deserializeInvoice(hash [32]byte, data []byte) (*IssuedInvoice, error) {
	var (
		offerID          []byte
		payer            *btcec.PublicKey
		amount, quantity uint64
		created          uint64
		str              []byte
	)
	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(invTypeOfferID, &offerID),
		tlv.MakePrimitiveRecord(invTypePayerID, &payer),
		tlv.MakePrimitiveRecord(invTypeAmount, &amount),
		tlv.MakePrimitiveRecord(invTypeCreatedAt, &created),
		tlv.MakePrimitiveRecord(invTypeBolt12, &str),
		tlv.MakePrimitiveRecord(invTypeQuantity, &quantity),
	)
	if err != nil {
		return nil, err
	}
	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(data))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	for _, t := range []tlv.Type{
		invTypeOfferID, invTypePayerID, invTypeAmount,
		invTypeCreatedAt, invTypeBolt12,
	} {
		if _, ok := parsed[t]; !ok {
			return nil, fmt.Errorf("offer invoice record missing "+
				"type %d", t)
		}
	}
	if len(offerID) != 32 {
		return nil, errors.New("offer invoice record with a bad offer id")
	}
	inv := &IssuedInvoice{
		PaymentHash: hash,
		PayerID:     payer,
		AmountMsat:  amount,
		Quantity:    quantity,
		CreatedAt:   time.Unix(int64(created), 0).UTC(),
		Bolt12:      string(str),
	}
	copy(inv.OfferID[:], offerID)

	return inv, nil
}

// InvoiceID returns the payment hash of a signed BOLT 12 invoice.
func InvoiceID(inv *bolt12.Invoice) ([32]byte, bool) {
	var (
		hash [32]byte
		ok   bool
	)
	inv.InvoicePaymentHash.WhenSome(
		func(r tlv.RecordT[tlv.TlvType168, [32]byte]) {
			hash, ok = r.Val, true
		},
	)

	return hash, ok
}
