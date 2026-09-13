package lnwire

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func hashOf(b byte) chainhash.Hash {
	var h chainhash.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func encodeInit(t *testing.T, msg *Init) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, msg.Encode(&buf, 0))
	return buf.Bytes()
}

func decodeInit(t *testing.T, b []byte) *Init {
	t.Helper()
	var msg Init
	require.NoError(t, msg.Decode(bytes.NewReader(b), 0))
	return &msg
}

// TestInitNetworksRoundTrip checks that the networks list survives a wire
// round trip, that its absence decodes to nil, and that the record lands in
// the TLV stream at type 1 with the expected layout.
func TestInitNetworksRoundTrip(t *testing.T) {
	features := NewRawFeatureVector(DataLossProtectRequired)

	// Absent: no record, nil on decode, and the message is byte-identical
	// to one built by an implementation that knows nothing about the
	// field.
	plain := NewInitMessage(NewRawFeatureVector(), features)
	plainBytes := encodeInit(t, plain)
	require.Nil(t, decodeInit(t, plainBytes).Networks)

	// One chain hash.
	one := NewInitMessage(NewRawFeatureVector(), features)
	one.Networks = []chainhash.Hash{hashOf(0xaa)}
	oneBytes := encodeInit(t, one)
	require.Equal(t, len(plainBytes)+2+chainhash.HashSize, len(oneBytes))

	// Type 1, length 32, then the hash, right after the feature vectors.
	tail := oneBytes[len(plainBytes):]
	require.Equal(t, byte(1), tail[0])
	require.Equal(t, byte(chainhash.HashSize), tail[1])
	require.Equal(t, hashOf(0xaa), chainhash.Hash(tail[2:]))

	decoded := decodeInit(t, oneBytes)
	require.Equal(t, one.Networks, decoded.Networks)
	require.Empty(t, decoded.ExtraData)
	require.Empty(t, decoded.CustomRecords)

	// Two chain hashes, re-encoded, is a fixed point.
	two := NewInitMessage(NewRawFeatureVector(), features)
	two.Networks = []chainhash.Hash{hashOf(0x01), hashOf(0x02)}
	twoBytes := encodeInit(t, two)
	decodedTwo := decodeInit(t, twoBytes)
	require.Equal(t, two.Networks, decodedTwo.Networks)
	require.Equal(t, twoBytes, encodeInit(t, decodedTwo))
}

// TestInitNetworksWithCustomRecords checks that the networks record coexists
// with custom records and stays sorted in the stream.
func TestInitNetworksWithCustomRecords(t *testing.T) {
	msg := NewInitMessage(NewRawFeatureVector(), NewRawFeatureVector())
	msg.Networks = []chainhash.Hash{hashOf(0x07)}
	msg.CustomRecords = CustomRecords{
		MinCustomRecordsTlvType + 5: []byte{1, 2, 3},
	}

	b := encodeInit(t, msg)
	decoded := decodeInit(t, b)
	require.Equal(t, msg.Networks, decoded.Networks)
	require.Equal(t, msg.CustomRecords, decoded.CustomRecords)
	require.Equal(t, b, encodeInit(t, decoded))
}

// TestInitNetworksBadLength checks that a networks record whose length is not
// a multiple of 32 is refused rather than silently truncated.
func TestInitNetworksBadLength(t *testing.T) {
	msg := NewInitMessage(NewRawFeatureVector(), NewRawFeatureVector())
	base := encodeInit(t, msg)

	// Append a hand-built type-1 record of 31 bytes.
	bad := append([]byte{}, base...)
	bad = append(bad, 1, 31)
	bad = append(bad, make([]byte, 31)...)

	var decoded Init
	require.Error(t, decoded.Decode(bytes.NewReader(bad), 0))
}

// TestInitNetworksEmptyList checks the nil/empty distinction: a nil list
// omits the field, an empty list is an empty record, and both survive a round
// trip unchanged so the message is a fixed point either way.
func TestInitNetworksEmptyList(t *testing.T) {
	plain := NewInitMessage(NewRawFeatureVector(), NewRawFeatureVector())
	plainBytes := encodeInit(t, plain)
	require.Nil(t, decodeInit(t, plainBytes).Networks)

	empty := NewInitMessage(NewRawFeatureVector(), NewRawFeatureVector())
	empty.Networks = []chainhash.Hash{}
	emptyBytes := encodeInit(t, empty)
	require.Equal(t, append(append([]byte{}, plainBytes...), 1, 0), emptyBytes)

	decoded := decodeInit(t, emptyBytes)
	require.NotNil(t, decoded.Networks)
	require.Empty(t, decoded.Networks)
	require.Equal(t, emptyBytes, encodeInit(t, decoded))
}
