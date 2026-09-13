package input

import (
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
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

// CheckPsbtSigHashOptIn returns an error when the opt-in is on, legacy
// signatures are not allowed, and the packet declares a hash type or carries
// a signature that does not opt in. It is the last line against finalizing
// a transaction that a signer elsewhere made replayable.
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
