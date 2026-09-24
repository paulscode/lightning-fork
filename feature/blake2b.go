package feature

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrMissingBlake2b is returned when a payment artifact does not set
// option_blake2b, which means it was written by a node that has not upgraded.
var ErrMissingBlake2b = errors.New("does not set option_blake2b")

// CheckBlake2bInvoice reports whether a BOLT 11 invoice says its writer
// follows the BLAKE2b proof of work rules.
//
// BOLT-blake2b #11:
//   - if it follows the BLAKE2b proof of work rules and `option_blake2b` is
//     not set:
//   - MUST NOT attempt the payment.
//
// This is the reverse direction of BOLT 9's unknown-even rule, and both are
// needed. A payer which has not upgraded refuses our invoices because it
// cannot read the even bit; this is how a payer which has upgraded refuses
// theirs. chain_hash cannot do either job, because an invoice written on
// either side carries the same one, and neither can the BOLT 11 prefix: a
// change of proof of work is not a change of currency, so both mint `lnbc`.
//
// Unconditional, because this build always sets the bit in its own invoices;
// see defaultSetDesc. Either form counts, since a writer which sets only the
// odd form is still saying which rules it follows.
//
// Without this the payment is still very unlikely to succeed, because the two
// graphs do not meet, but it fails for want of a route rather than being
// declined, which tells the payer nothing about why.
func CheckBlake2bInvoice(fv *lnwire.FeatureVector) error {
	if fv == nil {
		return fmt.Errorf("invoice %w", ErrMissingBlake2b)
	}

	if fv.IsSet(lnwire.Blake2bRequired) || fv.IsSet(lnwire.Blake2bOptional) {
		return nil
	}

	return fmt.Errorf("invoice %w", ErrMissingBlake2b)
}
