package bolt12

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// ErrMissingBlake2b is returned when a payment artifact does not set
// option_blake2b, which means it was written by a node that has not upgraded.
var ErrMissingBlake2b = errors.New("does not set option_blake2b")

// Blake2bFeatures names option_blake2b for a reader of an artifact's feature
// vector.
//
// The artifacts this node writes carry the bit in its even form, so a reader
// which does not know it refuses them under BOLT 9's unknown-even rule and
// never attempts a payment it could not settle. That rule cuts both ways: a
// reader here has to be told the bit is known, or it would refuse its own
// artifacts. The catalogue names only this pair, so every other even bit stays
// unknown and is still refused.
var Blake2bFeatures = map[lnwire.FeatureBit]string{
	lnwire.Blake2bRequired: "blake2b",
	lnwire.Blake2bOptional: "blake2b",
}

// Blake2bVector returns the feature vector for an artifact written by this
// node: option_blake2b in its even form, plus whatever else the caller sets.
func Blake2bVector(extra ...lnwire.FeatureBit) *lnwire.RawFeatureVector {
	bits := make([]lnwire.FeatureBit, 0, len(extra)+1)
	bits = append(bits, lnwire.Blake2bRequired)
	bits = append(bits, extra...)

	return lnwire.NewRawFeatureVector(bits...)
}

// CheckBlake2b reports whether an artifact's feature vector says its writer
// follows the BLAKE2b proof of work rules.
//
// BOLT-blake2b #12:
//   - if it follows the BLAKE2b proof of work rules and the artifact's
//     features do not set `option_blake2b`:
//   - MUST NOT respond to an offer, and MUST reject an invoice_request or
//     an invoice.
//
// An absent feature vector is the case this catches: it is what a node which
// has not upgraded writes. Either form of the bit counts, because a writer
// which sets only the odd form is still telling us which rules it follows.
//
// This is the reverse direction of the unknown-even rule, and both are needed.
// A reader which has not upgraded refuses our artifacts because it cannot read
// the even bit; this is how a reader which has upgraded refuses theirs.
func CheckBlake2b[T tlv.TlvType](
	opt tlv.OptionalRecordT[T, lnwire.RawFeatureVector],
	artifact string) error {

	set := fn.MapOptionZ(
		opt.ValOpt(),
		func(fv lnwire.RawFeatureVector) bool {
			return fv.IsSet(lnwire.Blake2bRequired) ||
				fv.IsSet(lnwire.Blake2bOptional)
		},
	)
	if !set {
		return fmt.Errorf("%s %w", artifact, ErrMissingBlake2b)
	}

	return nil
}
