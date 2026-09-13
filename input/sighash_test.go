package input

import (
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
