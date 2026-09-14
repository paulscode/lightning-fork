//go:build offersrpc
// +build offersrpc

package offersrpc

// Config is the primary configuration struct for the offers RPC server. It
// contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	// Deps is what the node provides.
	Deps *Deps
}
