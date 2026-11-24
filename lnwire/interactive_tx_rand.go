package lnwire

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// Random instances of the interactive transaction and splicing messages, for
// the wire round trip property test.

// randExtra returns random TLV records of unknown odd types: even ones are
// refused on decode, and the message's known types are avoided.
func randExtra(t *rapid.T, known ...uint64) ExtraOpaqueData {
	records, _ := RandTLVRecords(t, fn.NewSet(known...), 0)
	for typ := range records {
		if typ%2 == 0 {
			delete(records, typ)
		}
	}
	if len(records) == 0 {
		return nil
	}

	encoded, err := records.Serialize()
	require.NoError(t, err)

	return ExtraOpaqueData(encoded)
}

// randInFlight returns a random max_htlc_value_in_flight, or none.
func randInFlight(t *rapid.T) fn.Option[MilliSatoshi] {
	if !rapid.Bool().Draw(t, "inFlight") {
		return fn.None[MilliSatoshi]()
	}

	return fn.Some(MilliSatoshi(rapid.Uint64().Draw(t, "inFlightMsat")))
}

// randBytes returns a random byte slice of length within [lo, hi], nil when
// empty.
func randBytes(t *rapid.T, lo, hi int, label string) []byte {
	n := rapid.IntRange(lo, hi).Draw(t, label+"Len")
	if n == 0 {
		return nil
	}

	return rapid.SliceOfN(rapid.Byte(), n, n).Draw(t, label)
}

// randHash returns a random hash.
func randHash(t *rapid.T, label string) chainhash.Hash {
	var h chainhash.Hash
	copy(h[:], rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(t, label))

	return h
}

// randContribution returns a random s64 amount over its whole range.
func randContribution(t *rapid.T, label string) btcutil.Amount {
	return btcutil.Amount(rapid.Int64().Draw(t, label))
}

// RandTestMessage returns a random TxAddInput.
func (m *TxAddInput) RandTestMessage(t *rapid.T) Message {
	msg := &TxAddInput{
		ChannelID:       RandChannelID(t),
		SerialID:        rapid.Uint64().Draw(t, "serialID"),
		PrevTx:          randBytes(t, 0, 400, "prevTx"),
		PrevTxVout:      rapid.Uint32().Draw(t, "vout"),
		Sequence:        rapid.Uint32().Draw(t, "sequence"),
		SharedInputTxid: fn.None[chainhash.Hash](),
		ExtraData:       randExtra(t, 0),
	}
	if rapid.Bool().Draw(t, "shared") {
		msg.SharedInputTxid = fn.Some(randHash(t, "sharedTxid"))
	}

	return msg
}

// RandTestMessage returns a random TxAddOutput.
func (m *TxAddOutput) RandTestMessage(t *rapid.T) Message {
	return &TxAddOutput{
		ChannelID: RandChannelID(t),
		SerialID:  rapid.Uint64().Draw(t, "serialID"),
		Amount: btcutil.Amount(rapid.Int64Range(0, 21e14).Draw(
			t, "amount",
		)),
		Script:    randBytes(t, 0, 80, "script"),
		ExtraData: randExtra(t),
	}
}

// RandTestMessage returns a random TxRemoveInput.
func (m *TxRemoveInput) RandTestMessage(t *rapid.T) Message {
	return &TxRemoveInput{
		ChannelID: RandChannelID(t),
		SerialID:  rapid.Uint64().Draw(t, "serialID"),
		ExtraData: randExtra(t),
	}
}

// RandTestMessage returns a random TxRemoveOutput.
func (m *TxRemoveOutput) RandTestMessage(t *rapid.T) Message {
	return &TxRemoveOutput{
		ChannelID: RandChannelID(t),
		SerialID:  rapid.Uint64().Draw(t, "serialID"),
		ExtraData: randExtra(t),
	}
}

// RandTestMessage returns a random TxComplete.
func (m *TxComplete) RandTestMessage(t *rapid.T) Message {
	return &TxComplete{
		ChannelID: RandChannelID(t),
		ExtraData: randExtra(t),
	}
}

// RandTestMessage returns a random TxSignatures.
func (m *TxSignatures) RandTestMessage(t *rapid.T) Message {
	msg := &TxSignatures{
		ChannelID:            RandChannelID(t),
		TxID:                 randHash(t, "txid"),
		SharedInputSignature: fn.None[Sig](),
		ExtraData:            randExtra(t, 0),
	}

	numWitnesses := rapid.IntRange(0, 4).Draw(t, "numWitnesses")
	for i := 0; i < numWitnesses; i++ {
		numItems := rapid.IntRange(0, 3).Draw(
			t, fmt.Sprintf("numItems-%d", i),
		)
		witness := make(wire.TxWitness, numItems)
		for j := range witness {
			witness[j] = rapid.SliceOfN(rapid.Byte(), 0, 80).Draw(
				t, fmt.Sprintf("item-%d-%d", i, j),
			)
		}
		msg.Witnesses = append(msg.Witnesses, witness)
	}

	if rapid.Bool().Draw(t, "sharedSig") {
		msg.SharedInputSignature = fn.Some(RandSignature(t))
	}

	return msg
}

// RandTestMessage returns a random TxInitRbf.
func (m *TxInitRbf) RandTestMessage(t *rapid.T) Message {
	msg := &TxInitRbf{
		ChannelID:                 RandChannelID(t),
		LockTime:                  rapid.Uint32().Draw(t, "lockTime"),
		FeeRatePerKw:              rapid.Uint32().Draw(t, "feeRate"),
		FundingOutputContribution: fn.None[btcutil.Amount](),
		RequireConfirmedInputs:    rapid.Bool().Draw(t, "confirmed"),
		MaxHtlcValueInFlight:      randInFlight(t),
		ExtraData:                 randExtra(t, 0, 2, 4073),
	}
	if rapid.Bool().Draw(t, "contribution") {
		msg.FundingOutputContribution = fn.Some(
			randContribution(t, "amount"),
		)
	}

	return msg
}

// RandTestMessage returns a random TxAckRbf.
func (m *TxAckRbf) RandTestMessage(t *rapid.T) Message {
	msg := &TxAckRbf{
		ChannelID:                 RandChannelID(t),
		FundingOutputContribution: fn.None[btcutil.Amount](),
		RequireConfirmedInputs:    rapid.Bool().Draw(t, "confirmed"),
		MaxHtlcValueInFlight:      randInFlight(t),
		ExtraData:                 randExtra(t, 0, 2, 4073),
	}
	if rapid.Bool().Draw(t, "contribution") {
		msg.FundingOutputContribution = fn.Some(
			randContribution(t, "amount"),
		)
	}

	return msg
}

// RandTestMessage returns a random TxAbort.
func (m *TxAbort) RandTestMessage(t *rapid.T) Message {
	return &TxAbort{
		ChannelID: RandChannelID(t),
		Data:      randBytes(t, 0, 200, "data"),
		ExtraData: randExtra(t),
	}
}

// RandTestMessage returns a random SpliceInit.
func (m *SpliceInit) RandTestMessage(t *rapid.T) Message {
	return &SpliceInit{
		ChannelID:              RandChannelID(t),
		FundingContribution:    randContribution(t, "contribution"),
		FundingFeeRatePerKw:    rapid.Uint32().Draw(t, "feeRate"),
		LockTime:               rapid.Uint32().Draw(t, "lockTime"),
		FundingPubKey:          RandPubKey(t),
		RequireConfirmedInputs: rapid.Bool().Draw(t, "confirmed"),
		MaxHtlcValueInFlight:   randInFlight(t),
		ExtraData:              randExtra(t, 2, 4073),
	}
}

// RandTestMessage returns a random SpliceAck.
func (m *SpliceAck) RandTestMessage(t *rapid.T) Message {
	return &SpliceAck{
		ChannelID:              RandChannelID(t),
		FundingContribution:    randContribution(t, "contribution"),
		FundingPubKey:          RandPubKey(t),
		RequireConfirmedInputs: rapid.Bool().Draw(t, "confirmed"),
		MaxHtlcValueInFlight:   randInFlight(t),
		ExtraData:              randExtra(t, 2, 4073),
	}
}

// RandTestMessage returns a random SpliceLocked.
func (m *SpliceLocked) RandTestMessage(t *rapid.T) Message {
	return &SpliceLocked{
		ChannelID:  RandChannelID(t),
		SpliceTxid: randHash(t, "txid"),
		ExtraData:  randExtra(t),
	}
}

// RandTestMessage returns a random StartBatch.
func (m *StartBatch) RandTestMessage(t *rapid.T) Message {
	msg := &StartBatch{
		ChannelID:        RandChannelID(t),
		BatchSize:        rapid.Uint16().Draw(t, "batchSize"),
		BatchMessageType: fn.None[MessageType](),
		ExtraData:        randExtra(t, 1),
	}
	if rapid.Bool().Draw(t, "messageType") {
		msg.BatchMessageType = fn.Some(MessageType(
			rapid.Uint16().Draw(t, "type"),
		))
	}

	return msg
}

// Compile time checks.
var (
	_ TestMessage = (*TxAddInput)(nil)
	_ TestMessage = (*TxAddOutput)(nil)
	_ TestMessage = (*TxRemoveInput)(nil)
	_ TestMessage = (*TxRemoveOutput)(nil)
	_ TestMessage = (*TxComplete)(nil)
	_ TestMessage = (*TxSignatures)(nil)
	_ TestMessage = (*TxInitRbf)(nil)
	_ TestMessage = (*TxAckRbf)(nil)
	_ TestMessage = (*TxAbort)(nil)
	_ TestMessage = (*SpliceInit)(nil)
	_ TestMessage = (*SpliceAck)(nil)
	_ TestMessage = (*SpliceLocked)(nil)
	_ TestMessage = (*StartBatch)(nil)
)
