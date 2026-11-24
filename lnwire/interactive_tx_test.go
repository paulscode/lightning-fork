package lnwire

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// The expected encodings below are built by hand from the field layout in
// BOLT 2, field by field, independently of the code under test, so that a
// mistake made the same way in Encode and Decode (a wrong field order or
// width) still shows. A round trip alone can't catch that.

// h joins hex fragments, ignoring spaces.
func h(t *testing.T, parts ...string) []byte {
	t.Helper()

	b, err := hex.DecodeString(strings.ReplaceAll(
		strings.Join(parts, ""), " ", "",
	))
	require.NoError(t, err)

	return b
}

const (
	chanIDHex = "1111111111111111111111111111111111111111111111111111111111111111"
	hashHex   = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	pubKeyHex = "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
)

func testChanID() ChannelID {
	var id ChannelID
	copy(id[:], bytes.Repeat([]byte{0x11}, 32))

	return id
}

// testHash is not a palindrome, so a byte reversal shows: on the wire a
// txid is in internal order, the same bytes as chainhash.Hash.
func testHash() chainhash.Hash {
	var hash chainhash.Hash
	for i := range hash {
		hash[i] = byte(i + 1)
	}

	return hash
}

func testPubKey(t *testing.T) *btcec.PublicKey {
	t.Helper()

	b, err := hex.DecodeString(pubKeyHex)
	require.NoError(t, err)
	key, err := btcec.ParsePubKey(b)
	require.NoError(t, err)

	return key
}

// checkGolden encodes msg, compares with want (type included), and decodes
// want back into a message equal to msg.
func checkGolden(t *testing.T, msg Message, want []byte) {
	t.Helper()

	var b bytes.Buffer
	_, err := WriteMessage(&b, msg, 0)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(want), hex.EncodeToString(b.Bytes()))

	decoded, err := ReadMessage(bytes.NewReader(want), 0)
	require.NoError(t, err)
	require.Equal(t, msg, decoded)
}

func TestInteractiveTxGolden(t *testing.T) {
	t.Parallel()

	t.Run("tx_add_input shared", func(t *testing.T) {
		checkGolden(t, &TxAddInput{
			ChannelID:       testChanID(),
			SerialID:        2,
			PrevTxVout:      1,
			Sequence:        0xfffffffd,
			SharedInputTxid: fn.Some(testHash()),
		}, h(t,
			"0042", chanIDHex, "0000000000000002",
			"0000", // prevtx_len, no prevtx
			"00000001", "fffffffd",
			"00 20", hashHex, // TLV 0, 32 bytes
		))
	})

	t.Run("tx_add_input wallet", func(t *testing.T) {
		checkGolden(t, &TxAddInput{
			ChannelID:       testChanID(),
			SerialID:        3,
			PrevTx:          []byte{0xab, 0xcd},
			PrevTxVout:      0,
			Sequence:        0xfffffffd,
			SharedInputTxid: fn.None[chainhash.Hash](),
		}, h(t,
			"0042", chanIDHex, "0000000000000003",
			"0002 abcd", "00000000", "fffffffd",
		))
	})

	t.Run("tx_add_output", func(t *testing.T) {
		checkGolden(t, &TxAddOutput{
			ChannelID: testChanID(),
			SerialID:  4,
			Amount:    btcutil.Amount(100_000),
			Script:    []byte{0x00, 0x14},
		}, h(t,
			"0043", chanIDHex, "0000000000000004",
			"00000000000186a0", "0002 0014",
		))
	})

	t.Run("tx_remove_input", func(t *testing.T) {
		checkGolden(t, &TxRemoveInput{
			ChannelID: testChanID(), SerialID: 5,
		}, h(t, "0044", chanIDHex, "0000000000000005"))
	})

	t.Run("tx_remove_output", func(t *testing.T) {
		checkGolden(t, &TxRemoveOutput{
			ChannelID: testChanID(), SerialID: 6,
		}, h(t, "0045", chanIDHex, "0000000000000006"))
	})

	t.Run("tx_complete", func(t *testing.T) {
		checkGolden(t, &TxComplete{ChannelID: testChanID()},
			h(t, "0046", chanIDHex))
	})

	t.Run("tx_signatures", func(t *testing.T) {
		var sig Sig
		copy(sig.bytes[:], bytes.Repeat([]byte{0x33}, 64))

		checkGolden(t, &TxSignatures{
			ChannelID: testChanID(),
			TxID:      testHash(),
			Witnesses: []wire.TxWitness{
				{{0x01, 0x02}, {0x03}},
			},
			SharedInputSignature: fn.Some(sig),
		}, h(t,
			"0047", chanIDHex, hashHex,
			"0001",                  // num_witnesses
			"0006 02 02 0102 01 03", // len, then the bitcoin witness
			"00 40", strings.Repeat("33", 64),
		))
	})

	t.Run("tx_init_rbf", func(t *testing.T) {
		checkGolden(t, &TxInitRbf{
			ChannelID:                 testChanID(),
			LockTime:                  800_000,
			FeeRatePerKw:              253,
			FundingOutputContribution: fn.Some(btcutil.Amount(-1)),
			RequireConfirmedInputs:    true,
		}, h(t,
			"0048", chanIDHex, "000c3500", "000000fd",
			"00 08 ffffffffffffffff", // TLV 0, s64 -1
			"02 00",                  // TLV 2, no value
		))
	})

	t.Run("tx_ack_rbf", func(t *testing.T) {
		checkGolden(t, &TxAckRbf{
			ChannelID:                 testChanID(),
			FundingOutputContribution: fn.None[btcutil.Amount](),
		}, h(t, "0049", chanIDHex))
	})

	// max_htlc_value_in_flight (bolts#20): type 4073 is the BigSize
	// fd0fe9, after the lower types.
	t.Run("tx_init_rbf in flight", func(t *testing.T) {
		checkGolden(t, &TxInitRbf{
			ChannelID:                 testChanID(),
			LockTime:                  800_000,
			FeeRatePerKw:              253,
			FundingOutputContribution: fn.Some(btcutil.Amount(0)),
			RequireConfirmedInputs:    true,
			MaxHtlcValueInFlight: fn.Some(
				MilliSatoshi(1_980_000_000),
			),
		}, h(t,
			"0048", chanIDHex, "000c3500", "000000fd",
			"00 08 0000000000000000",     // TLV 0, s64 0
			"02 00",                      // TLV 2, no value
			"fd0fe9 08 0000000076046700", // TLV 4073, u64
		))
	})

	t.Run("tx_ack_rbf in flight", func(t *testing.T) {
		checkGolden(t, &TxAckRbf{
			ChannelID:                 testChanID(),
			FundingOutputContribution: fn.None[btcutil.Amount](),
			MaxHtlcValueInFlight:      fn.Some(MilliSatoshi(1)),
		}, h(t, "0049", chanIDHex, "fd0fe9 08 0000000000000001"))
	})

	t.Run("tx_abort", func(t *testing.T) {
		checkGolden(t, &TxAbort{
			ChannelID: testChanID(),
			Data:      []byte("no"),
		}, h(t, "004a", chanIDHex, "0002 6e6f"))
	})
}

func TestSpliceGolden(t *testing.T) {
	t.Parallel()

	key := testPubKey(t)

	t.Run("splice_init out", func(t *testing.T) {
		checkGolden(t, &SpliceInit{
			ChannelID:              testChanID(),
			FundingContribution:    btcutil.Amount(-50_000),
			FundingFeeRatePerKw:    1000,
			LockTime:               800_000,
			FundingPubKey:          key,
			RequireConfirmedInputs: true,
		}, h(t,
			"0050", chanIDHex,
			"ffffffffffff3cb0", // s64 -50000
			"000003e8", "000c3500", pubKeyHex,
			"02 00",
		))
	})

	t.Run("splice_ack", func(t *testing.T) {
		checkGolden(t, &SpliceAck{
			ChannelID:           testChanID(),
			FundingContribution: 0,
			FundingPubKey:       key,
		}, h(t, "0051", chanIDHex, "0000000000000000", pubKeyHex))
	})

	t.Run("splice_init in flight", func(t *testing.T) {
		checkGolden(t, &SpliceInit{
			ChannelID:              testChanID(),
			FundingContribution:    btcutil.Amount(1_000_000),
			FundingFeeRatePerKw:    1000,
			LockTime:               800_000,
			FundingPubKey:          key,
			RequireConfirmedInputs: true,
			MaxHtlcValueInFlight: fn.Some(
				MilliSatoshi(1_980_000_000),
			),
		}, h(t,
			"0050", chanIDHex,
			"00000000000f4240", // s64 1000000
			"000003e8", "000c3500", pubKeyHex,
			"02 00",
			"fd0fe9 08 0000000076046700",
		))
	})

	t.Run("splice_ack in flight", func(t *testing.T) {
		checkGolden(t, &SpliceAck{
			ChannelID:           testChanID(),
			FundingContribution: 0,
			FundingPubKey:       key,
			MaxHtlcValueInFlight: fn.Some(
				MilliSatoshi(990_000_000),
			),
		}, h(t, "0051", chanIDHex, "0000000000000000", pubKeyHex,
			"fd0fe9 08 000000003b023380"))
	})

	t.Run("splice_locked", func(t *testing.T) {
		checkGolden(t, &SpliceLocked{
			ChannelID:  testChanID(),
			SpliceTxid: testHash(),
		}, h(t, "004d", chanIDHex, hashHex))
	})

	t.Run("start_batch", func(t *testing.T) {
		checkGolden(t, &StartBatch{
			ChannelID:        testChanID(),
			BatchSize:        2,
			BatchMessageType: fn.Some(MessageType(MsgCommitSig)),
		}, h(t,
			"007f", chanIDHex, "0002",
			"01 02 0084", // TLV 1, message_type 132
		))
	})
}

// TestSpliceTLVsGolden covers the splice records added to existing
// messages.
func TestSpliceTLVsGolden(t *testing.T) {
	t.Parallel()

	var sig Sig
	copy(sig.bytes[:], bytes.Repeat([]byte{0x33}, 64))

	t.Run("commitment_signed funding_txid", func(t *testing.T) {
		checkGolden(t, &CommitSig{
			ChanID:      testChanID(),
			CommitSig:   sig,
			FundingTxid: fn.Some(testHash()),
		}, h(t,
			"0084", chanIDHex, strings.Repeat("33", 64),
			"0000",           // no HTLC signatures
			"01 20", hashHex, // TLV 1
		))
	})

	t.Run("channel_reestablish splice records", func(t *testing.T) {
		key := testPubKey(t)
		var secret [32]byte
		copy(secret[:], bytes.Repeat([]byte{0x44}, 32))

		checkGolden(t, &ChannelReestablish{
			ChanID:                    testChanID(),
			NextLocalCommitHeight:     7,
			RemoteCommitTailHeight:    6,
			LastRemoteCommitSecret:    secret,
			LocalUnrevokedCommitPoint: key,
			NextFunding: fn.Some(FundingTxidFlags{
				Txid:            testHash(),
				RetransmitFlags: RetransmitCommitSig,
			}),
			MyCurrentFundingLocked: fn.Some(FundingTxidFlags{
				Txid:            testHash(),
				RetransmitFlags: RetransmitAnnounceSigs,
			}),
		}, h(t,
			"0088", chanIDHex,
			"0000000000000007", "0000000000000006",
			strings.Repeat("44", 32), pubKeyHex,
			"01 21", hashHex, "01", // next_funding
			"05 21", hashHex, "01", // my_current_funding_locked
		))
	})
}

// TestInteractiveTxDecodeRefusals checks inputs a peer controls that must
// not decode.
func TestInteractiveTxDecodeRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  []byte
	}{{
		// A witness whose item count runs past its data.
		name: "witness count beyond its bytes",
		raw: h(t, "0047", chanIDHex, hashHex, "0001",
			"0001 05"),
	}, {
		// Bytes left over after the witness items.
		name: "trailing bytes in a witness",
		raw: h(t, "0047", chanIDHex, hashHex, "0001",
			"0003 01 00 ff"),
	}, {
		// A flag record that carries a value.
		name: "require_confirmed_inputs with a value",
		raw:  h(t, "0049", chanIDHex, "02 01 01"),
	}, {
		// A contribution that isn't eight bytes.
		name: "short s64 record",
		raw:  h(t, "0049", chanIDHex, "00 02 0001"),
	}, {
		// A prevtx length beyond the message.
		name: "prevtx longer than the message",
		raw: h(t, "0042", chanIDHex, "0000000000000002",
			"ffff 00"),
	}}

	for _, tc := range cases {
		_, err := ReadMessage(bytes.NewReader(tc.raw), 0)
		require.Error(t, err, tc.name)
	}
}

// TestUnknownTLVsSurviveReencoding checks that a decoded message re-encodes
// with the unknown records it arrived with, alongside its known ones.
func TestUnknownTLVsSurviveReencoding(t *testing.T) {
	t.Parallel()

	raw := h(t,
		"0042", chanIDHex, "0000000000000002", "0000",
		"00000001", "fffffffd",
		"00 20", hashHex, // shared_input_txid
		"03 01 aa",   // unknown odd
		"07 02 bbcc", // unknown odd
	)

	msg, err := ReadMessage(bytes.NewReader(raw), 0)
	require.NoError(t, err)
	in := msg.(*TxAddInput)
	require.True(t, in.SharedInputTxid.IsSome())

	var b bytes.Buffer
	_, err = WriteMessage(&b, in, 0)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(raw), hex.EncodeToString(b.Bytes()))

	// Encoding didn't fold the known record into ExtraData.
	require.Equal(t, h(t, "03 01 aa", "07 02 bbcc"), []byte(in.ExtraData))
}

// TestUnknownEvenTLVRefused checks BOLT 1: an unknown even type is one the
// receiver was told it must understand.
func TestUnknownEvenTLVRefused(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{
		h(t, "004d", chanIDHex, hashHex, "02 00"),
		h(t, "0046", chanIDHex, "04 01 00"),
		h(t, "0050", chanIDHex, "0000000000000000", "00000000",
			"00000000", pubKeyHex, "04 00"),
	} {
		_, err := ReadMessage(bytes.NewReader(raw), 0)
		require.ErrorContains(t, err, "unknown even TLV type")
	}
}

// TestS64Extremes checks the edges of the signed contribution.
func TestS64Extremes(t *testing.T) {
	t.Parallel()

	key := testPubKey(t)
	for _, tc := range []struct {
		amt btcutil.Amount
		hex string
	}{
		{btcutil.Amount(math.MinInt64), "8000000000000000"},
		{btcutil.Amount(math.MaxInt64), "7fffffffffffffff"},
		{0, "0000000000000000"},
		{-1, "ffffffffffffffff"},
	} {
		checkGolden(t, &SpliceAck{
			ChannelID:           testChanID(),
			FundingContribution: tc.amt,
			FundingPubKey:       key,
		}, h(t, "0051", chanIDHex, tc.hex, pubKeyHex))
	}
}

// TestDraftLengthReestablishRefused checks that a 32-byte record in type 1
// of channel_reestablish, the layout of an older splicing draft, is refused
// rather than misread.
func TestDraftLengthReestablishRefused(t *testing.T) {
	t.Parallel()

	raw := h(t,
		"0088", chanIDHex, "0000000000000007", "0000000000000006",
		strings.Repeat("44", 32), pubKeyHex,
		"01 20", hashHex,
	)
	_, err := ReadMessage(bytes.NewReader(raw), 0)
	require.Error(t, err)
}

// TestStartBatchWithoutMessageType checks the record is optional on the
// wire.
func TestStartBatchWithoutMessageType(t *testing.T) {
	t.Parallel()

	checkGolden(t, &StartBatch{
		ChannelID:        testChanID(),
		BatchSize:        3,
		BatchMessageType: fn.None[MessageType](),
	}, h(t, "007f", chanIDHex, "0003"))
}
