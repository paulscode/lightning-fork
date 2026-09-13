package input

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
)

// withUnifiedSigHash runs f with the opt-in set as given and restores it.
func withUnifiedSigHash(t *testing.T, enabled, allowLegacy bool, f func()) {
	t.Helper()
	prev, prevAllow := UnifiedSigHash(), AllowLegacySigHash()
	SetUnifiedSigHash(enabled)
	SetAllowLegacySigHash(allowLegacy)
	t.Cleanup(func() {
		SetUnifiedSigHash(prev)
		SetAllowLegacySigHash(prevAllow)
	})
	f()
}

// TestSoleSignerSigHash: the opt-in is on by default, gives ALL|UNIFIED for
// every spend type, and off gives what the node signed with before.
func TestSoleSignerSigHash(t *testing.T) {
	require.Equal(t, DefaultUnifiedSigHash(), UnifiedSigHash(),
		"the opt-in must start at the build's default")

	withUnifiedSigHash(t, true, false, func() {
		want := txscript.SigHashAll | txscript.SigHashUnified
		require.Equal(t, want, SoleSignerSigHash(false))
		require.Equal(t, want, SoleSignerSigHash(true))
		require.True(t, OptInSigHash(SoleSignerSigHash(true)))
	})
	withUnifiedSigHash(t, false, false, func() {
		require.Equal(t, txscript.SigHashAll, SoleSignerSigHash(false))
		require.Equal(t, txscript.SigHashDefault, SoleSignerSigHash(true))
	})

	// What a watchtower reconstructs never changes.
	require.Equal(t, txscript.SigHashAll, LegacySoleSignerSigHash(false))
	require.Equal(t, txscript.SigHashDefault, LegacySoleSignerSigHash(true))
}

// TestCheckPsbtSigHashOptIn covers every place a PSBT can carry a hash type
// that did not opt in.
func TestCheckPsbtSigHashOptIn(t *testing.T) {
	newPacket := func() *psbt.Packet {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{})
		packet, err := psbt.NewFromUnsignedTx(tx)
		require.NoError(t, err)
		return packet
	}
	sig := func(hashType byte) []byte {
		return append(make([]byte, 70), hashType)
	}

	withUnifiedSigHash(t, true, false, func() {
		// Nothing declared, nothing signed: fine.
		require.NoError(t, CheckPsbtSigHashOptIn(newPacket()))

		p := newPacket()
		p.Inputs[0].SighashType = txscript.SigHashAll
		require.ErrorContains(t, CheckPsbtSigHashOptIn(p), "0x1")
		p.Inputs[0].SighashType = txscript.SigHashAll | txscript.SigHashUnified
		require.NoError(t, CheckPsbtSigHashOptIn(p))

		p = newPacket()
		p.Inputs[0].PartialSigs = []*psbt.PartialSig{{Signature: sig(0x01)}}
		require.Error(t, CheckPsbtSigHashOptIn(p))
		p.Inputs[0].PartialSigs = []*psbt.PartialSig{{Signature: sig(0x21)}}
		require.NoError(t, CheckPsbtSigHashOptIn(p))

		p = newPacket()
		p.Inputs[0].TaprootKeySpendSig = make([]byte, 64)
		require.ErrorContains(t, CheckPsbtSigHashOptIn(p), "SIGHASH_DEFAULT")
		p.Inputs[0].TaprootKeySpendSig = append(make([]byte, 64), 0x21)
		require.NoError(t, CheckPsbtSigHashOptIn(p))

		p = newPacket()
		p.Inputs[0].TaprootScriptSpendSig = []*psbt.TaprootScriptSpendSig{
			{SigHash: txscript.SigHashAll},
		}
		require.Error(t, CheckPsbtSigHashOptIn(p))
		p.Inputs[0].TaprootScriptSpendSig[0].SigHash = 0x21
		require.NoError(t, CheckPsbtSigHashOptIn(p))
	})

	// The escape hatch, and the opt-in being off, accept everything.
	p := newPacket()
	p.Inputs[0].SighashType = txscript.SigHashAll
	withUnifiedSigHash(t, true, true, func() {
		require.NoError(t, CheckPsbtSigHashOptIn(p))
	})
	withUnifiedSigHash(t, false, false, func() {
		require.NoError(t, CheckPsbtSigHashOptIn(p))
	})
}

// TestOptInPsbtInputs: inputs declaring nothing or the library defaults are
// raised to the sole-signer type; anything else, and inputs without a
// spent output, are left alone.
func TestOptInPsbtInputs(t *testing.T) {
	tx := wire.NewMsgTx(2)
	for i := 0; i < 5; i++ {
		tx.AddTxIn(&wire.TxIn{})
	}
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	p2wkh := []byte{0x00, 0x14}
	p2tr := append([]byte{0x51, 0x20}, make([]byte, 32)...)
	packet.Inputs[0].WitnessUtxo = &wire.TxOut{PkScript: p2wkh}
	packet.Inputs[1].WitnessUtxo = &wire.TxOut{PkScript: p2wkh}
	packet.Inputs[1].SighashType = txscript.SigHashAll
	packet.Inputs[2].WitnessUtxo = &wire.TxOut{PkScript: p2tr}
	packet.Inputs[3].WitnessUtxo = &wire.TxOut{PkScript: p2wkh}
	packet.Inputs[3].SighashType = txscript.SigHashSingle
	// Input 4 has no spent output and stays untouched.
	packet.Inputs[4].SighashType = txscript.SigHashAll

	withUnifiedSigHash(t, true, false, func() {
		OptInPsbtInputs(packet)
		want := txscript.SigHashAll | txscript.SigHashUnified
		require.Equal(t, want, packet.Inputs[0].SighashType)
		require.Equal(t, want, packet.Inputs[1].SighashType)
		require.Equal(t, want, packet.Inputs[2].SighashType)
		require.Equal(t, txscript.SigHashSingle, packet.Inputs[3].SighashType)
		require.Equal(t, txscript.SigHashAll, packet.Inputs[4].SighashType)
	})

	fresh, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	fresh.Inputs[0].WitnessUtxo = &wire.TxOut{PkScript: p2wkh}
	withUnifiedSigHash(t, false, false, func() {
		OptInPsbtInputs(fresh)
		require.Equal(t, txscript.SigHashType(0), fresh.Inputs[0].SighashType)
	})
	withUnifiedSigHash(t, true, true, func() {
		OptInPsbtInputs(fresh)
		require.Equal(t, txscript.SigHashType(0), fresh.Inputs[0].SighashType)
	})
}

// derSig builds a byte string shaped like a DER ECDSA signature with the
// given trailing hash type byte.
func derSig(hashType byte) []byte {
	body := make([]byte, 70)
	body[0] = 0x30
	body[1] = byte(len(body) - 2)
	return append(body, hashType)
}

// TestCheckFinalizedSignaturesOptIn: signatures already folded into a final
// witness or signature script, and into a raw transaction, are inspected.
func TestCheckFinalizedSignaturesOptIn(t *testing.T) {
	newPacket := func() *psbt.Packet {
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{})
		packet, err := psbt.NewFromUnsignedTx(tx)
		require.NoError(t, err)
		return packet
	}

	withUnifiedSigHash(t, true, false, func() {
		// P2WPKH final witness: [sig, pubkey].
		p := newPacket()
		p.Inputs[0].FinalScriptWitness = serializeWitness(t, wire.TxWitness{
			derSig(0x01), make([]byte, 33),
		})
		require.ErrorContains(t, CheckPsbtSigHashOptIn(p), "0x1")
		p.Inputs[0].FinalScriptWitness = serializeWitness(t, wire.TxWitness{
			derSig(0x21), make([]byte, 33),
		})
		require.NoError(t, CheckPsbtSigHashOptIn(p))

		// Taproot key spend: 64 bytes is SIGHASH_DEFAULT.
		p = newPacket()
		p.Inputs[0].FinalScriptWitness = serializeWitness(t, wire.TxWitness{
			make([]byte, 64),
		})
		require.Error(t, CheckPsbtSigHashOptIn(p))
		p.Inputs[0].FinalScriptWitness = serializeWitness(t, wire.TxWitness{
			append(make([]byte, 64), 0x21),
		})
		require.NoError(t, CheckPsbtSigHashOptIn(p))

		// Signature script (P2PKH): the pushed signature.
		p = newPacket()
		sigScript, err := txscript.NewScriptBuilder().
			AddData(derSig(0x01)).AddData(make([]byte, 33)).Script()
		require.NoError(t, err)
		p.Inputs[0].FinalScriptSig = sigScript
		require.Error(t, CheckPsbtSigHashOptIn(p))

		// A raw transaction is inspected the same way.
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{Witness: wire.TxWitness{
			derSig(0x01), make([]byte, 33),
		}})
		require.Error(t, CheckTxSigHashOptIn(tx))
		tx.TxIn[0].Witness[0] = derSig(0xa1)
		require.NoError(t, CheckTxSigHashOptIn(tx))

		// A witness script element that is not a signature is ignored.
		tx.TxIn[0].Witness = wire.TxWitness{
			derSig(0x21), nil, []byte{0x51, 0x52, 0x53},
		}
		require.NoError(t, CheckTxSigHashOptIn(tx))
	})
}

// serializeWitness encodes a witness stack as a PSBT final witness field.
func serializeWitness(t *testing.T, witness wire.TxWitness) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, psbt.WriteTxWitness(&b, witness))
	return b.Bytes()
}

// TestSigHashesFor pins which midstate an opted-in signature is hashed with.
func TestSigHashesFor(t *testing.T) {
	out := wire.NewTxOut(1000, []byte{0x00, 0x14})
	single := wire.NewMsgTx(2)
	single.AddTxIn(&wire.TxIn{})
	multi := wire.NewMsgTx(2)
	multi.AddTxIn(&wire.TxIn{})
	multi.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}})
	legacyCache := NewTxSigHashesV0Only(multi)

	// A legacy hash type keeps whatever the caller built.
	desc := &SignDescriptor{HashType: txscript.SigHashAll, SigHashes: legacyCache}
	require.Same(t, legacyCache, SigHashesFor(multi, desc))

	// Opted in with a per-outpoint fetcher: rebuilt from it.
	multiFetcher := txscript.NewMultiPrevOutFetcher(map[wire.OutPoint]*wire.TxOut{
		multi.TxIn[0].PreviousOutPoint: out,
		multi.TxIn[1].PreviousOutPoint: out,
	})
	desc = &SignDescriptor{
		HashType: 0x21, SigHashes: legacyCache, Output: out,
		PrevOutputFetcher: multiFetcher,
	}
	require.NotSame(t, legacyCache, SigHashesFor(multi, desc))

	// Opted in on a single input: the descriptor's output is enough.
	desc = &SignDescriptor{HashType: 0x21, SigHashes: legacyCache, Output: out}
	require.NotSame(t, legacyCache, SigHashesFor(single, desc))

	// Opted in on several inputs with only a canned fetcher: the caller's
	// midstate, which the digest will refuse if it was canned too.
	desc = &SignDescriptor{
		HashType: 0x21, SigHashes: legacyCache, Output: out,
		PrevOutputFetcher: txscript.NewCannedPrevOutputFetcher(nil, 0),
	}
	require.Same(t, legacyCache, SigHashesFor(multi, desc))
}
