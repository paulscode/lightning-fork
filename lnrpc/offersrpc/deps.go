package offersrpc

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/offerpay"
	"github.com/lightningnetwork/lnd/offers"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Deps is what the offers sub-server needs from the node. It is defined for
// every build so the root server can hand it over whether or not the
// sub-server is compiled in.
type Deps struct {
	// Manager holds this node's offers.
	Manager *offers.Manager

	// Client fetches invoices for other nodes' offers.
	Client *offerpay.Client

	// Invoices records the invoices issued for this node's offers.
	Invoices *offers.InvoiceStore

	// LookupInvoice returns an invoice from the registry, for the state
	// of an issued invoice.
	LookupInvoice func(ctx context.Context,
		hash [32]byte) (*invoices.Invoice, error)

	// PayInvoice makes a payment and returns the preimage and the route
	// that succeeded.
	PayInvoice func(ctx context.Context,
		payment *routing.LightningPayment) ([32]byte, *route.Route, error)

	// LookupPayment returns what became of a payment made earlier: its
	// preimage when it settled, whether it is still in flight.
	LookupPayment func(ctx context.Context,
		hash [32]byte) (preimage [32]byte, settled, inFlight bool,
		err error)

	// ResolveIntro resolves an introduction node given as a channel and
	// direction, for paying an invoice whose paths use that form.
	ResolveIntro func(ctx context.Context,
		node lnwire.IntroductionNode) (*btcec.PublicKey, error)
}
