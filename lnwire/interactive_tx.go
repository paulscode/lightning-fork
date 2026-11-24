package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// The interactive transaction construction messages of BOLT 2, used to build
// a splice transaction together with the peer.

// TxAddInput adds an input to the transaction under construction (type 66).
type TxAddInput struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// SerialID orders the input in the transaction; its parity says which
	// side added it (even for the initiator).
	SerialID uint64

	// PrevTx is the transaction holding the spent output. It is empty for
	// the shared input of a splice, which SharedInputTxid names instead.
	PrevTx []byte

	// PrevTxVout is the index of the spent output.
	PrevTxVout uint32

	// Sequence is the input's sequence number.
	Sequence uint32

	// SharedInputTxid, set by a splice initiator on the input spending the
	// current funding output (TLV type 0).
	SharedInputTxid fn.Option[chainhash.Hash]

	// ExtraData holds the TLV records of unknown (odd) types, kept so the
	// message re-encodes with everything it arrived with.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxAddInput) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := WriteUint64(w, m.SerialID); err != nil {
		return err
	}
	if err := writeU16Bytes(w, m.PrevTx); err != nil {
		return err
	}
	if err := WriteUint32(w, m.PrevTxVout); err != nil {
		return err
	}
	if err := WriteUint32(w, m.Sequence); err != nil {
		return err
	}

	var records []tlv.RecordProducer
	m.SharedInputTxid.WhenSome(func(h chainhash.Hash) {
		hash := [32]byte(h)
		records = append(records, hashRecord{0, &hash})
	})

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *TxAddInput) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID, &m.SerialID); err != nil {
		return err
	}

	prevTx, err := readU16Bytes(r)
	if err != nil {
		return err
	}
	m.PrevTx = prevTx

	if err := ReadElements(r, &m.PrevTxVout, &m.Sequence); err != nil {
		return err
	}

	var hash [32]byte
	parsed, extra, err := decodeTLVs(r, hashRecord{0, &hash})
	if err != nil {
		return err
	}
	m.SharedInputTxid = fn.None[chainhash.Hash]()
	if parsed.Contains(0) {
		m.SharedInputTxid = fn.Some(chainhash.Hash(hash))
	}
	m.ExtraData = extra

	return nil
}

// MsgType returns MsgTxAddInput.
func (m *TxAddInput) MsgType() MessageType { return MsgTxAddInput }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxAddInput) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxAddInput) TargetChanID() ChannelID { return m.ChannelID }

// TxAddOutput adds an output to the transaction under construction (type
// 67).
type TxAddOutput struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// SerialID orders the output; its parity says which side added it.
	SerialID uint64

	// Amount is the output's value.
	Amount btcutil.Amount

	// Script is the output's script.
	Script []byte

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxAddOutput) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := WriteUint64(w, m.SerialID); err != nil {
		return err
	}
	if err := WriteSatoshi(w, m.Amount); err != nil {
		return err
	}
	if err := writeU16Bytes(w, m.Script); err != nil {
		return err
	}

	return encodeTLVs(w, m.ExtraData)
}

// Decode deserializes the message.
func (m *TxAddOutput) Decode(r io.Reader, _ uint32) error {
	err := ReadElements(r, &m.ChannelID, &m.SerialID, &m.Amount)
	if err != nil {
		return err
	}

	script, err := readU16Bytes(r)
	if err != nil {
		return err
	}
	m.Script = script

	return readExtraData(r, &m.ExtraData)
}

// MsgType returns MsgTxAddOutput.
func (m *TxAddOutput) MsgType() MessageType { return MsgTxAddOutput }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxAddOutput) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxAddOutput) TargetChanID() ChannelID { return m.ChannelID }

// TxRemoveInput removes an input the sender added earlier (type 68).
type TxRemoveInput struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// SerialID is the serial id of the input to remove.
	SerialID uint64

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxRemoveInput) Encode(w *bytes.Buffer, _ uint32) error {
	return encodeChanSerial(w, m.ChannelID, m.SerialID, m.ExtraData)
}

// Decode deserializes the message.
func (m *TxRemoveInput) Decode(r io.Reader, _ uint32) error {
	return decodeChanSerial(r, &m.ChannelID, &m.SerialID, &m.ExtraData)
}

// MsgType returns MsgTxRemoveInput.
func (m *TxRemoveInput) MsgType() MessageType { return MsgTxRemoveInput }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxRemoveInput) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxRemoveInput) TargetChanID() ChannelID { return m.ChannelID }

// TxRemoveOutput removes an output the sender added earlier (type 69).
type TxRemoveOutput struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// SerialID is the serial id of the output to remove.
	SerialID uint64

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxRemoveOutput) Encode(w *bytes.Buffer, _ uint32) error {
	return encodeChanSerial(w, m.ChannelID, m.SerialID, m.ExtraData)
}

// Decode deserializes the message.
func (m *TxRemoveOutput) Decode(r io.Reader, _ uint32) error {
	return decodeChanSerial(r, &m.ChannelID, &m.SerialID, &m.ExtraData)
}

// MsgType returns MsgTxRemoveOutput.
func (m *TxRemoveOutput) MsgType() MessageType { return MsgTxRemoveOutput }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxRemoveOutput) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxRemoveOutput) TargetChanID() ChannelID { return m.ChannelID }

// TxComplete says the sender has nothing more to add (type 70).
type TxComplete struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxComplete) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}

	return encodeTLVs(w, m.ExtraData)
}

// Decode deserializes the message.
func (m *TxComplete) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}

	return readExtraData(r, &m.ExtraData)
}

// MsgType returns MsgTxComplete.
func (m *TxComplete) MsgType() MessageType { return MsgTxComplete }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxComplete) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxComplete) TargetChanID() ChannelID { return m.ChannelID }

// TxSignatures carries the sender's witnesses for the inputs it added, and in
// a splice its signature for the shared input (type 71).
type TxSignatures struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// TxID is the id of the transaction being signed.
	TxID chainhash.Hash

	// Witnesses holds one witness per input the sender added, ordered by
	// serial id.
	Witnesses []wire.TxWitness

	// SharedInputSignature is the sender's signature for the shared input
	// of a splice, without a hash type byte (TLV type 0).
	SharedInputSignature fn.Option[Sig]

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// encodeWitness serializes a witness the way the Bitcoin wire protocol does:
// a CompactSize count of items, each a CompactSize length and its bytes.
func encodeWitness(witness wire.TxWitness) ([]byte, error) {
	var b bytes.Buffer
	if err := wire.WriteVarInt(&b, 0, uint64(len(witness))); err != nil {
		return nil, err
	}
	for _, item := range witness {
		if err := wire.WriteVarBytes(&b, 0, item); err != nil {
			return nil, err
		}
	}

	return b.Bytes(), nil
}

// decodeWitness parses a witness serialized by encodeWitness. Every byte must
// be accounted for.
func decodeWitness(data []byte) (wire.TxWitness, error) {
	r := bytes.NewReader(data)

	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}

	// Each item takes at least one byte, so a count beyond the data is a
	// lie; checking it first keeps the allocation bounded by the input.
	if count > uint64(len(data)) {
		return nil, fmt.Errorf("witness claims %d items in %d bytes",
			count, len(data))
	}

	witness := make(wire.TxWitness, count)
	for i := range witness {
		item, err := wire.ReadVarBytes(
			r, 0, uint32(len(data)), "witness item",
		)
		if err != nil {
			return nil, err
		}
		witness[i] = item
	}

	if r.Len() != 0 {
		return nil, fmt.Errorf("%d bytes left after witness", r.Len())
	}

	return witness, nil
}

// Encode serializes the message.
func (m *TxSignatures) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := WriteBytes(w, m.TxID[:]); err != nil {
		return err
	}
	if len(m.Witnesses) > 0xffff {
		return fmt.Errorf("%d witnesses exceed the u16 count",
			len(m.Witnesses))
	}
	if err := WriteUint16(w, uint16(len(m.Witnesses))); err != nil {
		return err
	}
	for _, witness := range m.Witnesses {
		data, err := encodeWitness(witness)
		if err != nil {
			return err
		}
		if err := writeU16Bytes(w, data); err != nil {
			return err
		}
	}

	var records []tlv.RecordProducer
	m.SharedInputSignature.WhenSome(func(sig Sig) {
		rec := tlv.NewRecordT[tlv.TlvType0](sig)
		records = append(records, &rec)
	})

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *TxSignatures) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, m.TxID[:]); err != nil {
		return err
	}

	var count uint16
	if err := ReadElement(r, &count); err != nil {
		return err
	}

	m.Witnesses = nil
	for i := 0; i < int(count); i++ {
		data, err := readU16Bytes(r)
		if err != nil {
			return err
		}
		witness, err := decodeWitness(data)
		if err != nil {
			return fmt.Errorf("witness %d: %w", i, err)
		}
		m.Witnesses = append(m.Witnesses, witness)
	}

	sig := tlv.ZeroRecordT[tlv.TlvType0, Sig]()
	parsed, extra, err := decodeTLVs(r, &sig)
	if err != nil {
		return err
	}
	m.SharedInputSignature = fn.None[Sig]()
	if parsed.Contains(0) {
		m.SharedInputSignature = fn.Some(sig.Val)
	}
	m.ExtraData = extra

	return nil
}

// MsgType returns MsgTxSignatures.
func (m *TxSignatures) MsgType() MessageType { return MsgTxSignatures }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxSignatures) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxSignatures) TargetChanID() ChannelID { return m.ChannelID }

// TxInitRbf starts a replacement of a transaction negotiated earlier (type
// 72).
type TxInitRbf struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// LockTime is the replacement's locktime.
	LockTime uint32

	// FeeRatePerKw is the replacement's feerate in satoshis per kiloweight.
	FeeRatePerKw uint32

	// FundingOutputContribution is the sender's contribution to the
	// funding output, when it changes (TLV type 0, s64).
	FundingOutputContribution fn.Option[btcutil.Amount]

	// RequireConfirmedInputs asks the receiver to add only confirmed
	// inputs (TLV type 2, no value).
	RequireConfirmedInputs bool

	// MaxHtlcValueInFlight is, for a splice's replacement, the
	// max_htlc_value_in_flight_msat the sender requires of the receiver
	// once it takes effect (TLV type 4073, u64). Absent, the value the
	// sender set for the splice's last attempt applies.
	MaxHtlcValueInFlight fn.Option[MilliSatoshi]

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxInitRbf) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := WriteUint32(w, m.LockTime); err != nil {
		return err
	}
	if err := WriteUint32(w, m.FeeRatePerKw); err != nil {
		return err
	}

	records := rbfRecords(
		m.FundingOutputContribution, m.RequireConfirmedInputs,
		m.MaxHtlcValueInFlight,
	)

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *TxInitRbf) Decode(r io.Reader, _ uint32) error {
	err := ReadElements(r, &m.ChannelID, &m.LockTime, &m.FeeRatePerKw)
	if err != nil {
		return err
	}

	return decodeRbfExtra(
		r, &m.FundingOutputContribution, &m.RequireConfirmedInputs,
		&m.MaxHtlcValueInFlight, &m.ExtraData,
	)
}

// MsgType returns MsgTxInitRbf.
func (m *TxInitRbf) MsgType() MessageType { return MsgTxInitRbf }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxInitRbf) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxInitRbf) TargetChanID() ChannelID { return m.ChannelID }

// TxAckRbf accepts a replacement (type 73).
type TxAckRbf struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// FundingOutputContribution is the sender's contribution to the
	// funding output, when it changes (TLV type 0, s64).
	FundingOutputContribution fn.Option[btcutil.Amount]

	// RequireConfirmedInputs asks the receiver to add only confirmed
	// inputs (TLV type 2, no value).
	RequireConfirmedInputs bool

	// MaxHtlcValueInFlight is, for a splice's replacement, the
	// max_htlc_value_in_flight_msat the sender requires of the receiver
	// once it takes effect (TLV type 4073, u64). Absent, the value the
	// sender set for the splice's last attempt applies.
	MaxHtlcValueInFlight fn.Option[MilliSatoshi]

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxAckRbf) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}

	records := rbfRecords(
		m.FundingOutputContribution, m.RequireConfirmedInputs,
		m.MaxHtlcValueInFlight,
	)

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *TxAckRbf) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}

	return decodeRbfExtra(
		r, &m.FundingOutputContribution, &m.RequireConfirmedInputs,
		&m.MaxHtlcValueInFlight, &m.ExtraData,
	)
}

// MsgType returns MsgTxAckRbf.
func (m *TxAckRbf) MsgType() MessageType { return MsgTxAckRbf }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxAckRbf) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxAckRbf) TargetChanID() ChannelID { return m.ChannelID }

// TxAbort ends the negotiation (type 74).
type TxAbort struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// Data is the peer's reason. It is the peer's text: show it only after
	// filtering it to printable characters and capping its length.
	Data []byte

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *TxAbort) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := writeU16Bytes(w, m.Data); err != nil {
		return err
	}

	return encodeTLVs(w, m.ExtraData)
}

// Decode deserializes the message.
func (m *TxAbort) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}

	data, err := readU16Bytes(r)
	if err != nil {
		return err
	}
	m.Data = data

	return readExtraData(r, &m.ExtraData)
}

// MsgType returns MsgTxAbort.
func (m *TxAbort) MsgType() MessageType { return MsgTxAbort }

// SerializedSize returns the serialized size of the message in bytes.
func (m *TxAbort) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *TxAbort) TargetChanID() ChannelID { return m.ChannelID }

// encodeChanSerial writes a message made of a channel id, a serial id and
// its TLV stream.
func encodeChanSerial(w *bytes.Buffer, chanID ChannelID, serialID uint64,
	extra ExtraOpaqueData) error {

	if err := WriteChannelID(w, chanID); err != nil {
		return err
	}
	if err := WriteUint64(w, serialID); err != nil {
		return err
	}

	return encodeTLVs(w, extra)
}

// decodeChanSerial reads a message written by encodeChanSerial.
func decodeChanSerial(r io.Reader, chanID *ChannelID, serialID *uint64,
	extra *ExtraOpaqueData) error {

	if err := ReadElements(r, chanID, serialID); err != nil {
		return err
	}

	return readExtraData(r, extra)
}

// readExtraData reads a message's trailing TLV stream; an empty one reads as
// nil, so that a decoded message compares equal to the one encoded.
func readExtraData(r io.Reader, extra *ExtraOpaqueData) error {
	_, data, err := decodeTLVs(r)
	if err != nil {
		return err
	}
	*extra = data

	return nil
}

// rbfRecords returns the records of tx_init_rbf and tx_ack_rbf.
func rbfRecords(contribution fn.Option[btcutil.Amount],
	requireConfirmed bool,
	inFlight fn.Option[MilliSatoshi]) []tlv.RecordProducer {

	var records []tlv.RecordProducer
	contribution.WhenSome(func(amt btcutil.Amount) {
		records = append(records, s64Record{0, &amt})
	})
	if requireConfirmed {
		records = append(records, requireConfirmedInputsType)
	}

	return append(records, inFlightRecords(inFlight)...)
}

// decodeRbfExtra reads the TLV stream of tx_init_rbf and tx_ack_rbf.
func decodeRbfExtra(r io.Reader, contribution *fn.Option[btcutil.Amount],
	requireConfirmed *bool, inFlight *fn.Option[MilliSatoshi],
	extra *ExtraOpaqueData) error {

	var (
		amt btcutil.Amount
		v   uint64
	)
	parsed, data, err := decodeTLVs(
		r, s64Record{0, &amt}, requireConfirmedInputsType,
		u64Record{maxHtlcValueInFlightType, &v},
	)
	if err != nil {
		return err
	}

	*contribution = fn.None[btcutil.Amount]()
	if parsed.Contains(0) {
		*contribution = fn.Some(amt)
	}
	*requireConfirmed = parsed.Contains(
		tlv.Type(requireConfirmedInputsType),
	)
	*inFlight = inFlightOption(parsed, v)
	*extra = data

	return nil
}

// Compile time checks.
var (
	_ Message     = (*TxAddInput)(nil)
	_ Message     = (*TxAddOutput)(nil)
	_ Message     = (*TxRemoveInput)(nil)
	_ Message     = (*TxRemoveOutput)(nil)
	_ Message     = (*TxComplete)(nil)
	_ Message     = (*TxSignatures)(nil)
	_ Message     = (*TxInitRbf)(nil)
	_ Message     = (*TxAckRbf)(nil)
	_ Message     = (*TxAbort)(nil)
	_ LinkUpdater = (*TxAddInput)(nil)
	_ LinkUpdater = (*TxAddOutput)(nil)
	_ LinkUpdater = (*TxRemoveInput)(nil)
	_ LinkUpdater = (*TxRemoveOutput)(nil)
	_ LinkUpdater = (*TxComplete)(nil)
	_ LinkUpdater = (*TxSignatures)(nil)
	_ LinkUpdater = (*TxInitRbf)(nil)
	_ LinkUpdater = (*TxAckRbf)(nil)
	_ LinkUpdater = (*TxAbort)(nil)
)
