//go:build offersrpc
// +build offersrpc

package offersrpc

import (
	"github.com/lightningnetwork/lnd/offers"
)

// Config is the primary configuration struct for the offers RPC server. It
// contains all the items required for the server to carry out its duties.
// The fields with struct tags are meant to be parsed as normal configuration
// options, while if able to be populated, the latter fields MUST also be
// specified.
type Config struct {
	// Manager mints, keeps and serves this node's offers.
	Manager *offers.Manager
}
