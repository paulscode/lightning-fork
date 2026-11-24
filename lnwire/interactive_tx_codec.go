package lnwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// Helpers shared by the interactive transaction construction and splicing
// messages (BOLT 2).

// writeU16Bytes writes a byte slice prefixed with its length as a u16.
func writeU16Bytes(w *bytes.Buffer, b []byte) error {
	if len(b) > math.MaxUint16 {
		return fmt.Errorf("field of %d bytes exceeds the u16 length "+
			"prefix", len(b))
	}
	if err := WriteUint16(w, uint16(len(b))); err != nil {
		return err
	}

	return WriteBytes(w, b)
}

// readU16Bytes reads a byte slice prefixed with its length as a u16. An empty
// field reads as nil.
func readU16Bytes(r io.Reader) ([]byte, error) {
	var l uint16
	if err := ReadElement(r, &l); err != nil {
		return nil, err
	}
	if l == 0 {
		return nil, nil
	}

	b := make([]byte, l)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}

	return b, nil
}

// writeS64 writes a signed amount as the spec's s64: eight bytes, big
// endian, two's complement.
func writeS64(w *bytes.Buffer, amt btcutil.Amount) error {
	return WriteUint64(w, uint64(amt))
}

// readS64 reads a signed amount written by writeS64.
func readS64(r io.Reader) (btcutil.Amount, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}

	return btcutil.Amount(int64(binary.BigEndian.Uint64(b[:]))), nil
}

// flagRecord is a TLV record with no value: its presence is what it says, as
// require_confirmed_inputs in splice_init, splice_ack, tx_init_rbf and
// tx_ack_rbf.
type flagRecord tlv.Type

// Record returns the zero-length record for the flag's type.
func (f flagRecord) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		tlv.Type(f), nil, 0,
		func(io.Writer, interface{}, *[8]byte) error {
			return nil
		},
		func(_ io.Reader, _ interface{}, _ *[8]byte, l uint64) error {
			if l != 0 {
				return fmt.Errorf("flag record %d carries %d "+
					"bytes, expected none", f, l)
			}

			return nil
		},
	)
}

// requireConfirmedInputsType is the TLV type of require_confirmed_inputs.
const requireConfirmedInputsType = flagRecord(2)

// maxHtlcValueInFlightType is the TLV type of max_htlc_value_in_flight in
// splice_init, splice_ack, tx_init_rbf and tx_ack_rbf (bolts#20): the
// max_htlc_value_in_flight_msat its sender requires of its peer once the
// splice transaction takes effect. Odd: a peer that doesn't know it ignores
// it.
const maxHtlcValueInFlightType tlv.Type = 4073

// u64Record is a TLV record holding a u64.
type u64Record struct {
	typ tlv.Type
	val *uint64
}

// Record returns the eight-byte record for the value.
func (u u64Record) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(u.typ, u.val)
}

// inFlightRecords returns max_htlc_value_in_flight's record if the value is
// set, for a message's known records.
func inFlightRecords(inFlight fn.Option[MilliSatoshi]) []tlv.RecordProducer {
	var records []tlv.RecordProducer
	inFlight.WhenSome(func(msat MilliSatoshi) {
		v := uint64(msat)
		records = append(records, u64Record{
			maxHtlcValueInFlightType, &v,
		})
	})

	return records
}

// inFlightOption returns max_htlc_value_in_flight as decoded into v, if the
// stream had it.
func inFlightOption(parsed fn.Set[tlv.Type],
	v uint64) fn.Option[MilliSatoshi] {

	if !parsed.Contains(maxHtlcValueInFlightType) {
		return fn.None[MilliSatoshi]()
	}

	return fn.Some(MilliSatoshi(v))
}

// s64Record is a TLV record holding a signed amount as an s64.
type s64Record struct {
	typ tlv.Type
	amt *btcutil.Amount
}

// Record returns the eight-byte record for the amount.
func (s s64Record) Record() tlv.Record {
	return tlv.MakeStaticRecord(
		s.typ, s.amt, 8,
		func(w io.Writer, val interface{}, buf *[8]byte) error {
			amt, ok := val.(*btcutil.Amount)
			if !ok {
				return tlv.NewTypeForEncodingErr(
					val, "*btcutil.Amount",
				)
			}
			v := uint64(*amt)

			return tlv.EUint64T(w, v, buf)
		},
		func(r io.Reader, val interface{}, buf *[8]byte,
			l uint64) error {

			amt, ok := val.(*btcutil.Amount)
			if !ok || l != 8 {
				return tlv.NewTypeForDecodingErr(
					val, "*btcutil.Amount", l, 8,
				)
			}
			var v uint64
			if err := tlv.DUint64(r, &v, buf, l); err != nil {
				return err
			}
			*amt = btcutil.Amount(int64(v))

			return nil
		},
	)
}

// hashRecord is a TLV record holding a 32-byte hash, as a txid.
type hashRecord struct {
	typ  tlv.Type
	hash *[32]byte
}

// Record returns the 32-byte record for the hash.
func (h hashRecord) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(h.typ, h.hash)
}

// u16Record is a TLV record holding a u16.
type u16Record struct {
	typ tlv.Type
	val *uint16
}

// Record returns the two-byte record for the value.
func (u u16Record) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(u.typ, u.val)
}

// present reports whether a record of the given type was decoded by one of
// the records handed to decodeExtraData.
func present(m tlv.TypeMap, typ tlv.Type) bool {
	val, ok := m[typ]

	return ok && val == nil
}

// decodeTLVs reads a message's trailing TLV stream: records of a known type
// are parsed into the given producers, everything else is returned as the
// message's ExtraData (nil when empty). An unknown even type is refused, as
// BOLT 1 requires: the peer said the receiver must understand it.
func decodeTLVs(r io.Reader, known ...tlv.RecordProducer) (fn.Set[tlv.Type],
	ExtraOpaqueData, error) {

	var data ExtraOpaqueData
	if err := ReadElements(r, &data); err != nil {
		return nil, nil, err
	}

	parsed, extra, err := ParseAndExtractExtraData(data, known...)
	if err != nil {
		return nil, nil, err
	}

	if len(extra) == 0 {
		return parsed, nil, nil
	}

	unknown, err := extra.ExtractRecords()
	if err != nil {
		return nil, nil, err
	}
	for typ := range unknown {
		if typ%2 == 0 {
			return nil, nil, fmt.Errorf("unknown even TLV type %d",
				typ)
		}
	}

	return parsed, extra, nil
}

// encodeTLVs writes the known records merged with the unknown ones kept in
// extra, without changing extra, so a decoded message re-encodes with every
// record it arrived with.
func encodeTLVs(w *bytes.Buffer, extra ExtraOpaqueData,
	known ...tlv.RecordProducer) error {

	encoded, err := MergeAndEncode(known, extra, nil)
	if err != nil {
		return err
	}

	return WriteBytes(w, encoded)
}
