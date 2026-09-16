//go:build bridgerpc
// +build bridgerpc

package bridgerpc

// Config is the primary configuration struct for the bridge RPC server. It
// contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	// Enabled turns the bridge on. It is off by default and stays off
	// until an operator says otherwise: a node carrying this code is not
	// the same thing as a node offering to swap other people's money.
	Enabled bool `long:"enabled" description:"Offer cross-chain swaps between this chain and Bitcoin. Requires a Bitcoin Lightning node and funded channels on both sides."`

	// Deps is what the node provides.
	Deps *Deps
}
