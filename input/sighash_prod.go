//go:build !dev && !integration

package input

// DefaultUnifiedSigHash is whether a release build opts sole-signer
// signatures into the unified signature hash before configuration is read:
// yes, since a release runs on the Bitcoin BLAKE2b chain.
func DefaultUnifiedSigHash() bool {
	return true
}
