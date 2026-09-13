package lnwire

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/tlv"
)

// InitNetworksRecordType is the TLV type of the BOLT 1 `networks` field of
// the init message: the chain hashes of the networks the sender will gossip
// or open channels for.
const InitNetworksRecordType tlv.Type = 1

// InitNetworks is the BOLT 1 `networks` record of the init message: a list of
// chain hashes, 32 bytes each, with no count prefix.
type InitNetworks []chainhash.Hash

// Record returns the TLV record for the networks list.
func (n *InitNetworks) Record() tlv.Record {
	sizeFunc := func() uint64 {
		return uint64(len(*n) * chainhash.HashSize)
	}
	return tlv.MakeDynamicRecord(
		InitNetworksRecordType, n, sizeFunc, encodeInitNetworks,
		decodeInitNetworks,
	)
}

func encodeInitNetworks(w io.Writer, val interface{}, _ *[8]byte) error {
	nets, ok := val.(*InitNetworks)
	if !ok {
		return tlv.NewTypeForEncodingErr(val, "*lnwire.InitNetworks")
	}
	for _, h := range *nets {
		if _, err := w.Write(h[:]); err != nil {
			return err
		}
	}
	return nil
}

func decodeInitNetworks(r io.Reader, val interface{}, _ *[8]byte,
	l uint64) error {

	nets, ok := val.(*InitNetworks)
	if !ok {
		return tlv.NewTypeForDecodingErr(val, "*lnwire.InitNetworks", l, 0)
	}
	if l%chainhash.HashSize != 0 {
		return fmt.Errorf("init networks record length %d is not a "+
			"multiple of %d", l, chainhash.HashSize)
	}

	count := int(l / chainhash.HashSize)
	out := make(InitNetworks, count)
	for i := 0; i < count; i++ {
		if _, err := io.ReadFull(r, out[i][:]); err != nil {
			return err
		}
	}
	*nets = out
	return nil
}

// Init is the first message reveals the features supported or required by this
// node. Nodes wait for receipt of the other's features to simplify error
// diagnosis where features are incompatible. Each node MUST wait to receive
// init before sending any other messages.
type Init struct {
	// GlobalFeatures is a legacy feature vector used for backwards
	// compatibility with older nodes. Any features defined here should be
	// merged with those presented in Features.
	GlobalFeatures *RawFeatureVector

	// Features is a feature vector containing the features supported by
	// the remote node.
	//
	// NOTE: Older nodes may place some features in GlobalFeatures, but all
	// new features are to be added in Features. When handling an Init
	// message, any GlobalFeatures should be merged into the unified
	// Features field.
	Features *RawFeatureVector

	// Networks is the BOLT 1 `networks` list: the chain hashes the sender
	// will gossip or open channels for. Nil when the sender omitted it.
	// Two chains that share a genesis block (Bitcoin and Bitcoin BLAKE2b)
	// can only be told apart at the handshake through this field, so a
	// node on the BLAKE2b chain always sends it and may refuse peers that
	// do not list its chain.
	Networks []chainhash.Hash

	// CustomRecords maps TLV types to byte slices, storing arbitrary data
	// intended for inclusion in the ExtraData field of the init message.
	CustomRecords CustomRecords

	// ExtraData is the set of data that was appended to this message to
	// fill out the full maximum transport message size. These fields can
	// be used to specify optional data such as custom TLV fields.
	ExtraData ExtraOpaqueData
}

// NewInitMessage creates new instance of init message object.
func NewInitMessage(gf *RawFeatureVector, f *RawFeatureVector) *Init {
	return &Init{
		GlobalFeatures: gf,
		Features:       f,
	}
}

// A compile time check to ensure Init implements the lnwire.Message
// interface.
var _ Message = (*Init)(nil)

// A compile time check to ensure Init implements the lnwire.SizeableMessage
// interface.
var _ SizeableMessage = (*Init)(nil)

// Decode deserializes a serialized Init message stored in the passed
// io.Reader observing the specified protocol version.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Decode(r io.Reader, pver uint32) error {
	var msgExtraData ExtraOpaqueData

	err := ReadElements(r,
		&msg.GlobalFeatures,
		&msg.Features,
		&msgExtraData,
	)
	if err != nil {
		return err
	}

	var networks InitNetworks
	customRecords, parsed, extraDData, err := ParseAndExtractCustomRecords(
		msgExtraData, &networks,
	)
	if err != nil {
		return err
	}

	msg.Networks = nil
	if parsed.Contains(InitNetworksRecordType) {
		msg.Networks = networks
	}
	msg.CustomRecords = customRecords
	msg.ExtraData = extraDData

	return nil
}

// Encode serializes the target Init into the passed io.Writer observing
// the protocol version specified.
//
// This is part of the lnwire.Message interface.
func (msg *Init) Encode(w *bytes.Buffer, pver uint32) error {
	if err := WriteRawFeatureVector(w, msg.GlobalFeatures); err != nil {
		return err
	}

	if err := WriteRawFeatureVector(w, msg.Features); err != nil {
		return err
	}

	// A nil list means the field is omitted; an empty non-nil list is an
	// empty record, so that a message decoded with one re-encodes to the
	// same bytes.
	var knownRecords []tlv.RecordProducer
	if msg.Networks != nil {
		networks := InitNetworks(msg.Networks)
		knownRecords = append(knownRecords, &networks)
	}

	extraData, err := MergeAndEncode(
		knownRecords, msg.ExtraData, msg.CustomRecords,
	)
	if err != nil {
		return err
	}

	return WriteBytes(w, extraData)
}

// MsgType returns the integer uniquely identifying this message type on the
// wire.
//
// This is part of the lnwire.Message interface.
func (msg *Init) MsgType() MessageType {
	return MsgInit
}

// SerializedSize returns the serialized size of the message in bytes.
//
// This is part of the lnwire.SizeableMessage interface.
func (msg *Init) SerializedSize() (uint32, error) {
	return MessageSerializedSize(msg)
}
