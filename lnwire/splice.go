package lnwire

import (
	"bytes"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// The splicing messages of BOLT 2.

// SpliceInit starts a splice (type 80). Only the quiescence initiator sends
// it.
type SpliceInit struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// FundingContribution is the change to the sender's channel balance:
	// positive adds funds, negative takes them out (s64).
	FundingContribution btcutil.Amount

	// FundingFeeRatePerKw is the splice transaction's feerate in satoshis
	// per kiloweight.
	FundingFeeRatePerKw uint32

	// LockTime is the splice transaction's locktime.
	LockTime uint32

	// FundingPubKey is the sender's key for the new funding output.
	FundingPubKey *btcec.PublicKey

	// RequireConfirmedInputs asks the receiver to add only confirmed
	// inputs (TLV type 2, no value).
	RequireConfirmedInputs bool

	// MaxHtlcValueInFlight is the max_htlc_value_in_flight_msat the
	// sender requires of the receiver once the splice takes effect, to
	// raise it (TLV type 4073, u64).
	MaxHtlcValueInFlight fn.Option[MilliSatoshi]

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *SpliceInit) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := writeS64(w, m.FundingContribution); err != nil {
		return err
	}
	if err := WriteUint32(w, m.FundingFeeRatePerKw); err != nil {
		return err
	}
	if err := WriteUint32(w, m.LockTime); err != nil {
		return err
	}
	if err := WritePublicKey(w, m.FundingPubKey); err != nil {
		return err
	}

	var records []tlv.RecordProducer
	if m.RequireConfirmedInputs {
		records = append(records, requireConfirmedInputsType)
	}
	records = append(records, inFlightRecords(m.MaxHtlcValueInFlight)...)

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *SpliceInit) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}

	contribution, err := readS64(r)
	if err != nil {
		return err
	}
	m.FundingContribution = contribution

	err = ReadElements(
		r, &m.FundingFeeRatePerKw, &m.LockTime, &m.FundingPubKey,
	)
	if err != nil {
		return err
	}

	return decodeFlagExtra(
		r, &m.RequireConfirmedInputs, &m.MaxHtlcValueInFlight,
		&m.ExtraData,
	)
}

// MsgType returns MsgSpliceInit.
func (m *SpliceInit) MsgType() MessageType { return MsgSpliceInit }

// SerializedSize returns the serialized size of the message in bytes.
func (m *SpliceInit) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *SpliceInit) TargetChanID() ChannelID { return m.ChannelID }

// SpliceAck accepts a splice (type 81).
type SpliceAck struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// FundingContribution is the change to the sender's channel balance
	// (s64).
	FundingContribution btcutil.Amount

	// FundingPubKey is the sender's key for the new funding output.
	FundingPubKey *btcec.PublicKey

	// RequireConfirmedInputs asks the receiver to add only confirmed
	// inputs (TLV type 2, no value).
	RequireConfirmedInputs bool

	// MaxHtlcValueInFlight is the max_htlc_value_in_flight_msat the
	// sender requires of the receiver once the splice takes effect, to
	// raise it (TLV type 4073, u64).
	MaxHtlcValueInFlight fn.Option[MilliSatoshi]

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *SpliceAck) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := writeS64(w, m.FundingContribution); err != nil {
		return err
	}
	if err := WritePublicKey(w, m.FundingPubKey); err != nil {
		return err
	}

	var records []tlv.RecordProducer
	if m.RequireConfirmedInputs {
		records = append(records, requireConfirmedInputsType)
	}
	records = append(records, inFlightRecords(m.MaxHtlcValueInFlight)...)

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *SpliceAck) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}

	contribution, err := readS64(r)
	if err != nil {
		return err
	}
	m.FundingContribution = contribution

	if err := ReadElements(r, &m.FundingPubKey); err != nil {
		return err
	}

	return decodeFlagExtra(
		r, &m.RequireConfirmedInputs, &m.MaxHtlcValueInFlight,
		&m.ExtraData,
	)
}

// MsgType returns MsgSpliceAck.
func (m *SpliceAck) MsgType() MessageType { return MsgSpliceAck }

// SerializedSize returns the serialized size of the message in bytes.
func (m *SpliceAck) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *SpliceAck) TargetChanID() ChannelID { return m.ChannelID }

// SpliceLocked says the sender considers a splice transaction deep enough
// (type 77).
type SpliceLocked struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// SpliceTxid is the id of the splice transaction.
	SpliceTxid chainhash.Hash

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// Encode serializes the message.
func (m *SpliceLocked) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := WriteBytes(w, m.SpliceTxid[:]); err != nil {
		return err
	}

	return encodeTLVs(w, m.ExtraData)
}

// Decode deserializes the message.
func (m *SpliceLocked) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID); err != nil {
		return err
	}
	if _, err := io.ReadFull(r, m.SpliceTxid[:]); err != nil {
		return err
	}

	return readExtraData(r, &m.ExtraData)
}

// MsgType returns MsgSpliceLocked.
func (m *SpliceLocked) MsgType() MessageType { return MsgSpliceLocked }

// SerializedSize returns the serialized size of the message in bytes.
func (m *SpliceLocked) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *SpliceLocked) TargetChanID() ChannelID { return m.ChannelID }

// StartBatch announces that the next BatchSize messages for the channel form
// one batch (type 127): while a splice is pending, one commitment_signed per
// funding transaction.
type StartBatch struct {
	// ChannelID identifies the channel.
	ChannelID ChannelID

	// BatchSize is the number of messages in the batch.
	BatchSize uint16

	// BatchMessageType is the type of the batched messages (TLV type 1).
	BatchMessageType fn.Option[MessageType]

	// ExtraData holds the TLV stream.
	ExtraData ExtraOpaqueData
}

// MaxBatchSize is the largest batch the spec allows.
const MaxBatchSize = 20

// Encode serializes the message.
func (m *StartBatch) Encode(w *bytes.Buffer, _ uint32) error {
	if err := WriteChannelID(w, m.ChannelID); err != nil {
		return err
	}
	if err := WriteUint16(w, m.BatchSize); err != nil {
		return err
	}

	var records []tlv.RecordProducer
	m.BatchMessageType.WhenSome(func(t MessageType) {
		v := uint16(t)
		records = append(records, u16Record{1, &v})
	})

	return encodeTLVs(w, m.ExtraData, records...)
}

// Decode deserializes the message.
func (m *StartBatch) Decode(r io.Reader, _ uint32) error {
	if err := ReadElements(r, &m.ChannelID, &m.BatchSize); err != nil {
		return err
	}

	var v uint16
	parsed, extra, err := decodeTLVs(r, u16Record{1, &v})
	if err != nil {
		return err
	}
	m.BatchMessageType = fn.None[MessageType]()
	if parsed.Contains(1) {
		m.BatchMessageType = fn.Some(MessageType(v))
	}
	m.ExtraData = extra

	return nil
}

// MsgType returns MsgStartBatch.
func (m *StartBatch) MsgType() MessageType { return MsgStartBatch }

// SerializedSize returns the serialized size of the message in bytes.
func (m *StartBatch) SerializedSize() (uint32, error) {
	return MessageSerializedSize(m)
}

// TargetChanID returns the channel the message is for.
func (m *StartBatch) TargetChanID() ChannelID { return m.ChannelID }

// decodeFlagExtra reads the TLV stream of splice_init and splice_ack, whose
// known records are require_confirmed_inputs and max_htlc_value_in_flight.
func decodeFlagExtra(r io.Reader, requireConfirmed *bool,
	inFlight *fn.Option[MilliSatoshi], extra *ExtraOpaqueData) error {

	var v uint64
	parsed, data, err := decodeTLVs(
		r, requireConfirmedInputsType,
		u64Record{maxHtlcValueInFlightType, &v},
	)
	if err != nil {
		return err
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
	_ Message     = (*SpliceInit)(nil)
	_ Message     = (*SpliceAck)(nil)
	_ Message     = (*SpliceLocked)(nil)
	_ Message     = (*StartBatch)(nil)
	_ LinkUpdater = (*SpliceInit)(nil)
	_ LinkUpdater = (*SpliceAck)(nil)
	_ LinkUpdater = (*SpliceLocked)(nil)
	_ LinkUpdater = (*StartBatch)(nil)
)
