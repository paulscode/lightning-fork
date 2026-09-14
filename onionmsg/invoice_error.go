package onionmsg

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// Onion message payload TLV types for BOLT 12 messages, from BOLT 4's
	// onionmsg_tlv.
	TypeInvoiceRequest tlv.Type = 64
	TypeInvoice        tlv.Type = 66
	TypeInvoiceError   tlv.Type = 68

	// invoice_error's own TLV types, from BOLT 12.
	typeErroneousField tlv.Type = 1
	typeSuggestedValue tlv.Type = 3
	typeErrorText      tlv.Type = 5

	// MaxInvoiceErrorBytes bounds the text of an invoice_error we accept
	// or send.
	MaxInvoiceErrorBytes = 1024
)

// InvoiceError is BOLT 12's invoice_error: the reply to a request or an
// invoice that could not be honoured.
type InvoiceError struct {
	// ErroneousField is the TLV type of the field the error is about,
	// when the sender named one.
	ErroneousField *uint64

	// SuggestedValue is what the sender would accept for that field, when
	// it said.
	SuggestedValue []byte

	// Message is the human-readable error, UTF-8.
	Message string
}

// Encode serialises the error as the value of an invoice_error TLV.
func (e *InvoiceError) Encode() ([]byte, error) {
	if e.Message == "" {
		return nil, fmt.Errorf("invoice_error needs a message")
	}
	if !utf8.ValidString(e.Message) {
		return nil, fmt.Errorf("invoice_error text is not UTF-8")
	}
	if len(e.Message) > MaxInvoiceErrorBytes {
		return nil, fmt.Errorf("invoice_error text over %d bytes",
			MaxInvoiceErrorBytes)
	}
	if e.SuggestedValue != nil && e.ErroneousField == nil {
		return nil, fmt.Errorf("invoice_error suggests a value for " +
			"no field")
	}
	var records []tlv.Record
	if e.ErroneousField != nil {
		field := *e.ErroneousField
		records = append(records, tlv.MakeDynamicRecord(
			typeErroneousField, &field, func() uint64 {
				return tlv.SizeTUint64(field)
			}, tlv.ETUint64, tlv.DTUint64,
		))
	}
	if e.SuggestedValue != nil {
		value := e.SuggestedValue
		records = append(records, tlv.MakePrimitiveRecord(
			typeSuggestedValue, &value,
		))
	}
	msg := []byte(e.Message)
	records = append(records, tlv.MakePrimitiveRecord(typeErrorText, &msg))

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := stream.Encode(&b); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// DecodeInvoiceError parses the value of an invoice_error TLV.
func DecodeInvoiceError(data []byte) (*InvoiceError, error) {
	var (
		field    uint64
		value    []byte
		text     []byte
		hasField bool
	)
	stream, err := tlv.NewStream(
		tlv.MakeDynamicRecord(
			typeErroneousField, &field, func() uint64 {
				return tlv.SizeTUint64(field)
			}, tlv.ETUint64, tlv.DTUint64,
		),
		tlv.MakePrimitiveRecord(typeSuggestedValue, &value),
		tlv.MakePrimitiveRecord(typeErrorText, &text),
	)
	if err != nil {
		return nil, err
	}
	parsed, err := stream.DecodeWithParsedTypes(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("invoice_error: %w", err)
	}
	if _, ok := parsed[typeErrorText]; !ok {
		return nil, fmt.Errorf("invoice_error without error text")
	}
	if !utf8.Valid(text) {
		return nil, fmt.Errorf("invoice_error text is not UTF-8")
	}
	if len(text) > MaxInvoiceErrorBytes {
		// Cut on a rune boundary, so the text stays UTF-8.
		cut := MaxInvoiceErrorBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
	_, hasField = parsed[typeErroneousField]
	_, hasValue := parsed[typeSuggestedValue]
	if hasValue && !hasField {
		return nil, fmt.Errorf("invoice_error suggests a value for " +
			"no field")
	}

	out := &InvoiceError{Message: string(text)}
	if hasField {
		out.ErroneousField = &field
	}
	if hasValue {
		out.SuggestedValue = value
	}

	return out, nil
}
