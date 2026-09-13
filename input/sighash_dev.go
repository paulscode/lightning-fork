//go:build dev || integration

package input

// DefaultUnifiedSigHash is whether a development or integration build opts
// sole-signer signatures into the unified signature hash: no, since those
// builds run upstream's tests and integration harness against a stock
// SHA256d regtest, where an opted-in signature is invalid.
func DefaultUnifiedSigHash() bool {
	return false
}
