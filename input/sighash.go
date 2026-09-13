package input

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// unifiedSigHash is whether signatures this node makes on its own opt into
// the Bitcoin BLAKE2b chain's unified signature hash (SIGHASH_UNIFIED,
// 0x20), which binds them to that chain. On by default in a release build
// (see DefaultUnifiedSigHash); the daemon turns it off only where the
// configuration allows it, before anything is signed.
var unifiedSigHash atomic.Bool

// allowLegacySigHash is whether a PSBT whose signatures do not opt in may
// still be finalized while the opt-in is on.
var allowLegacySigHash atomic.Bool

func init() {
	unifiedSigHash.Store(DefaultUnifiedSigHash())
}

// SetUnifiedSigHash sets whether sole-signer signatures opt into the
// unified signature hash. Call it once at startup, before any signing.
func SetUnifiedSigHash(enabled bool) {
	unifiedSigHash.Store(enabled)
}

// UnifiedSigHash returns whether sole-signer signatures opt into the unified
// signature hash.
func UnifiedSigHash() bool {
	return unifiedSigHash.Load()
}

// SetAllowLegacySigHash sets whether a PSBT whose signatures do not opt in
// may be finalized while the opt-in is on.
func SetAllowLegacySigHash(allowed bool) {
	allowLegacySigHash.Store(allowed)
}

// AllowLegacySigHash returns whether legacy signatures may be finalized while
// the opt-in is on.
func AllowLegacySigHash() bool {
	return allowLegacySigHash.Load()
}

// SoleSignerSigHash returns the hash type for a signature only this node
// makes: on-chain sends, sweeps, our own funding inputs, justice and
// second-level spends we sign alone. These are the signatures that can opt
// into the unified signature hash without anyone else having to agree, so
// they do whenever the opt-in is on. Signatures a peer verifies (commitment,
// HTLC and cooperative-close signatures) are not made with this and keep
// their protocol-fixed hash types.
//
// Taproot key-path spends normally use SIGHASH_DEFAULT, which appends no hash
// type byte and so cannot carry the bit; opted in, they use ALL|UNIFIED and
// their signatures are 65 bytes.
func SoleSignerSigHash(taproot bool) txscript.SigHashType {
	if UnifiedSigHash() {
		return txscript.SigHashAll | txscript.SigHashUnified
	}

	return LegacySoleSignerSigHash(taproot)
}

// LegacySoleSignerSigHash returns the hash type a sole-signer signature
// carried before the opt-in existed: SIGHASH_ALL, or SIGHASH_DEFAULT for a
// taproot key-path spend. A watchtower reconstructs justice witnesses on
// that assumption, so the signatures handed to one are made with this.
func LegacySoleSignerSigHash(taproot bool) txscript.SigHashType {
	if taproot {
		return txscript.SigHashDefault
	}

	return txscript.SigHashAll
}

// OptInSigHash returns whether a hash type carries the unified opt-in bit.
func OptInSigHash(hashType txscript.SigHashType) bool {
	return hashType&txscript.SigHashUnified != 0
}

// SigHashesFor returns the sighash midstate a signer should hash a sign
// descriptor's input with. An opted-in (SIGHASH_UNIFIED) signature commits
// to every spent output, which a midstate built for BIP143 alone may not
// know: where the descriptor carries a fetcher that answers per outpoint,
// the midstate is rebuilt from it; for a single-input transaction the
// descriptor's own output is the whole set, whatever fetcher it carries;
// otherwise the caller's midstate must know the inputs, and if it was
// built from a canned fetcher the digest refuses rather than hashing
// wrongly. Legacy hash types use the caller's midstate as before.
func SigHashesFor(tx *wire.MsgTx,
	signDesc *SignDescriptor) *txscript.TxSigHashes {

	if !OptInSigHash(signDesc.HashType) {
		return signDesc.SigHashes
	}

	fetcher := signDesc.PrevOutputFetcher
	_, canned := fetcher.(*txscript.CannedPrevOutputFetcher)
	switch {
	case fetcher != nil && !canned:
		return txscript.NewTxSigHashes(tx, fetcher)

	case len(tx.TxIn) == 1 && signDesc.Output != nil:
		return txscript.NewTxSigHashes(
			tx, txscript.NewCannedPrevOutputFetcher(
				signDesc.Output.PkScript, signDesc.Output.Value,
			),
		)

	default:
		return signDesc.SigHashes
	}
}

// OptInPsbtInputs raises the declared hash type of every input that carries
// a spent output and declares nothing, or one of the library defaults
// (SIGHASH_ALL for witness v0, SIGHASH_DEFAULT for taproot), to the
// sole-signer hash type, so that whatever this wallet signs in the packet
// opts in. The bit is decided by the chain the wallet is on, not by the
// packet; an input declaring any other hash type was set deliberately and is
// left alone, as is everything when the opt-in is off or legacy signatures
// are allowed.
func OptInPsbtInputs(packet *psbt.Packet) {
	if !UnifiedSigHash() || AllowLegacySigHash() {
		return
	}
	for i := range packet.Inputs {
		in := &packet.Inputs[i]
		if in.WitnessUtxo == nil {
			continue
		}
		taproot := txscript.IsPayToTaproot(in.WitnessUtxo.PkScript)
		switch in.SighashType {
		case 0, txscript.SigHashAll:
			in.SighashType = SoleSignerSigHash(taproot)
		}
	}
}

// CheckPsbtSigHashOptIn returns an error when the opt-in is on, legacy
// signatures are not allowed, and the packet declares a hash type or carries
// a signature, partial or final, that does not opt in. It is the last line
// against finalizing a transaction that a signer elsewhere made replayable.
func CheckPsbtSigHashOptIn(packet *psbt.Packet) error {
	if !UnifiedSigHash() || AllowLegacySigHash() {
		return nil
	}

	for i, in := range packet.Inputs {
		if in.SighashType != 0 && !OptInSigHash(in.SighashType) {
			return fmt.Errorf("input %d declares hash type 0x%x, "+
				"which does not opt into the unified signature hash; "+
				"a signature under it would be replayable on the "+
				"SHA256d chain (set bitcoin.allow-legacy-sighash to "+
				"finalize anyway)", i, in.SighashType)
		}

		// An input another signer finalized carries its signatures in
		// the final witness or signature script only.
		witness, err := parseFinalWitness(in.FinalScriptWitness)
		if err != nil {
			return fmt.Errorf("input %d: cannot read the final "+
				"witness: %w", i, err)
		}
		for _, hashType := range witnessSigHashTypes(witness) {
			if !OptInSigHash(hashType) {
				return fmt.Errorf("input %d: a finalized witness "+
					"signature uses hash type 0x%x, which does not "+
					"opt into the unified signature hash", i, hashType)
			}
		}
		for _, hashType := range sigScriptSigHashTypes(in.FinalScriptSig) {
			if !OptInSigHash(hashType) {
				return fmt.Errorf("input %d: a finalized signature "+
					"script uses hash type 0x%x, which does not opt "+
					"into the unified signature hash", i, hashType)
			}
		}

		for _, sig := range in.PartialSigs {
			if err := checkSigOptIn(sig.Signature, false); err != nil {
				return fmt.Errorf("input %d: %w", i, err)
			}
		}
		if in.TaprootKeySpendSig != nil {
			err := checkSigOptIn(in.TaprootKeySpendSig, true)
			if err != nil {
				return fmt.Errorf("input %d: %w", i, err)
			}
		}
		for _, sig := range in.TaprootScriptSpendSig {
			if !OptInSigHash(sig.SigHash) {
				return fmt.Errorf("input %d: a tapscript signature "+
					"uses hash type 0x%x, which does not opt into the "+
					"unified signature hash", i, sig.SigHash)
			}
		}
	}

	return nil
}

// CheckTxSigHashOptIn returns an error when the opt-in is on, legacy
// signatures are not allowed, and a signature in the transaction's witnesses
// or signature scripts does not opt in.
func CheckTxSigHashOptIn(tx *wire.MsgTx) error {
	if !UnifiedSigHash() || AllowLegacySigHash() {
		return nil
	}

	for i, in := range tx.TxIn {
		for _, hashType := range witnessSigHashTypes(in.Witness) {
			if !OptInSigHash(hashType) {
				return fmt.Errorf("input %d: a witness signature uses "+
					"hash type 0x%x, which does not opt into the "+
					"unified signature hash", i, hashType)
			}
		}
		for _, hashType := range sigScriptSigHashTypes(in.SignatureScript) {
			if !OptInSigHash(hashType) {
				return fmt.Errorf("input %d: a signature script "+
					"signature uses hash type 0x%x, which does not opt "+
					"into the unified signature hash", i, hashType)
			}
		}
	}

	return nil
}

// parseFinalWitness decodes a PSBT final witness field: a compact-size
// element count followed by compact-size-prefixed elements.
func parseFinalWitness(raw []byte) (wire.TxWitness, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	r := bytes.NewReader(raw)
	count, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}
	if count > uint64(len(raw)) {
		return nil, fmt.Errorf("witness claims %d elements", count)
	}
	witness := make(wire.TxWitness, 0, count)
	for i := uint64(0); i < count; i++ {
		elem, err := wire.ReadVarBytes(r, 0, uint32(len(raw)), "witness")
		if err != nil {
			return nil, err
		}
		witness = append(witness, elem)
	}

	return witness, nil
}

// witnessSigHashTypes returns the hash type of every signature a serialized
// witness carries. A one-element witness of 64 or 65 bytes is a taproot key
// spend (SIGHASH_DEFAULT, or the trailing byte); otherwise every element
// shaped like a DER ECDSA signature contributes its trailing byte. Script
// elements that merely look like signatures are rare enough that a false
// refusal, which the operator can override, beats a missed legacy signature.
func witnessSigHashTypes(witness wire.TxWitness) []txscript.SigHashType {
	if len(witness) == 1 {
		switch len(witness[0]) {
		case schnorr.SignatureSize:
			return []txscript.SigHashType{txscript.SigHashDefault}
		case schnorr.SignatureSize + 1:
			return []txscript.SigHashType{
				txscript.SigHashType(witness[0][schnorr.SignatureSize]),
			}
		}
	}

	var types []txscript.SigHashType
	for _, elem := range witness {
		if looksLikeDERSignature(elem) {
			types = append(types, txscript.SigHashType(elem[len(elem)-1]))
		}
	}

	return types
}

// sigScriptSigHashTypes returns the hash type of every pushed element of a
// signature script that is shaped like a DER ECDSA signature.
func sigScriptSigHashTypes(sigScript []byte) []txscript.SigHashType {
	if len(sigScript) == 0 {
		return nil
	}
	pushes, err := txscript.PushedData(sigScript)
	if err != nil {
		return nil
	}

	var types []txscript.SigHashType
	for _, elem := range pushes {
		if looksLikeDERSignature(elem) {
			types = append(types, txscript.SigHashType(elem[len(elem)-1]))
		}
	}

	return types
}

// looksLikeDERSignature is true for a DER-encoded ECDSA signature with a
// trailing hash type byte: a SEQUENCE whose declared length covers exactly
// the bytes before that byte.
func looksLikeDERSignature(b []byte) bool {
	if len(b) < 9 || len(b) > 74 || b[0] != 0x30 {
		return false
	}

	return int(b[1]) == len(b)-3
}

// checkSigOptIn checks the hash type byte a serialized signature carries. A
// 64-byte Schnorr signature carries none (SIGHASH_DEFAULT) and so cannot
// have opted in.
func checkSigOptIn(sig []byte, schnorr bool) error {
	if schnorr && len(sig) == 64 {
		return fmt.Errorf("a taproot signature uses SIGHASH_DEFAULT, " +
			"which cannot opt into the unified signature hash")
	}
	if len(sig) == 0 {
		return fmt.Errorf("empty signature")
	}
	if hashType := txscript.SigHashType(sig[len(sig)-1]); !OptInSigHash(
		hashType,
	) {

		return fmt.Errorf("a signature uses hash type 0x%x, which does "+
			"not opt into the unified signature hash", hashType)
	}

	return nil
}
