//go:build !integration

package chainreg

// AllowSHA256Regtest reports whether a regtest or simnet backend may be used
// without a BLAKE2b activation height. Release builds never allow it: the
// activation-header check is the only thing that tells the two chains apart.
func AllowSHA256Regtest() bool {
	return false
}
