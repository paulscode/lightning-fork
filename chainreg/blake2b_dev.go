//go:build integration

package chainreg

// AllowSHA256Regtest reports whether a regtest or simnet backend may be used
// without a BLAKE2b activation height. Only the integration build permits
// it, so that upstream's own integration tests, which mine SHA256d blocks
// with a btcd miner, remain runnable for the logic that has nothing to do
// with the chain. A release build never allows it.
func AllowSHA256Regtest() bool {
	return true
}
