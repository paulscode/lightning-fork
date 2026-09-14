//go:build offersrpc
// +build offersrpc

package commands

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc/offersrpc"
	"github.com/urfave/cli"
)

// offersCommands will return the set of commands to enable for offersrpc
// builds.
func offersCommands() []cli.Command {
	return []cli.Command{
		{
			Name:     "offer",
			Category: "Offers",
			Usage:    "Mint, list and decode BOLT 12 offers.",
			Subcommands: []cli.Command{
				createOfferCommand,
				listOffersCommand,
				disableOfferCommand,
				enableOfferCommand,
				decodeBolt12Command,
				fetchInvoiceCommand,
				payOfferCommand,
				listOfferInvoicesCommand,
			},
		},
	}
}

func getOffersClient(ctx *cli.Context) (offersrpc.OffersClient, func()) {
	conn := getClientConn(ctx, false)
	cleanUp := func() {
		conn.Close()
	}

	return offersrpc.NewOffersClient(conn), cleanUp
}

var createOfferCommand = cli.Command{
	Name:     "create",
	Category: "Offers",
	Usage:    "Mint a BOLT 12 offer for this node's chain.",
	Description: `
	Mint an offer this node will serve invoices for. The offer names the
	node's chain; a payer on another chain cannot use it.

	For a mining pool that pays over BOLT 12 (OCEAN on the SHA256d chain,
	CONVOY on this one), leave the amount at zero so the pool chooses it,
	and give the description the pool mandates, for example:

	    lncli offer create --description "OCEAN Payouts for bc1q..."

	An offer with an amount must have a description. Blinded paths are
	added when the node announces no address, so payers can still reach
	it; --with_paths adds them anyway and --no_paths never does.

	The same fields always mint the same offer, so creating one again
	after a restore from seed gives back the string the pool already has;
	the response says whether the offer was created or already existed.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "description",
			Usage: "what the offer is for; required with an amount",
		},
		cli.Uint64Flag{
			Name: "amount_msat",
			Usage: "the amount asked for in millisatoshi; zero " +
				"lets the payer choose",
		},
		cli.Uint64Flag{
			Name: "expiry",
			Usage: "seconds from now until the offer expires; " +
				"zero for never",
		},
		cli.Uint64Flag{
			Name: "absolute_expiry",
			Usage: "when the offer expires, as a unix timestamp " +
				"in seconds; zero for never",
		},
		cli.StringFlag{
			Name:  "issuer",
			Usage: "free text naming who is paid",
		},
		cli.Uint64Flag{
			Name: "quantity_max",
			Usage: "the largest quantity a request may ask for; " +
				"zero leaves quantities out of the offer",
		},
		cli.BoolFlag{
			Name:  "quantity_any",
			Usage: "let a request ask for any quantity",
		},
		cli.StringFlag{
			Name:  "label",
			Usage: "a note kept with the offer on this node only",
		},
		cli.BoolFlag{
			Name: "with_paths",
			Usage: "add blinded paths even though the node " +
				"announces an address",
		},
		cli.BoolFlag{
			Name: "no_paths",
			Usage: "never add blinded paths, even when the node " +
				"announces no address",
		},
	},
	Action: actionDecorator(createOffer),
}

func createOffer(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	if ctx.IsSet("expiry") && ctx.IsSet("absolute_expiry") {
		return fmt.Errorf("expiry and absolute_expiry exclude each " +
			"other")
	}
	expiry := ctx.Uint64("absolute_expiry")
	if ctx.Uint64("expiry") != 0 {
		expiry = uint64(time.Now().Unix()) + ctx.Uint64("expiry")
	}

	resp, err := client.CreateOffer(ctxc, &offersrpc.CreateOfferRequest{
		Description:    ctx.String("description"),
		AmountMsat:     ctx.Uint64("amount_msat"),
		AbsoluteExpiry: expiry,
		Issuer:         ctx.String("issuer"),
		QuantityMax:    ctx.Uint64("quantity_max"),
		QuantityAny:    ctx.Bool("quantity_any"),
		Label:          ctx.String("label"),
		WithPaths:      ctx.Bool("with_paths"),
		NoPaths:        ctx.Bool("no_paths"),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var listOffersCommand = cli.Command{
	Name:     "list",
	Category: "Offers",
	Usage:    "List the offers this node has minted.",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "active_only",
			Usage: "list only enabled offers",
		},
	},
	Action: actionDecorator(listOffers),
}

func listOffers(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	resp, err := client.ListOffers(ctxc, &offersrpc.ListOffersRequest{
		ActiveOnly: ctx.Bool("active_only"),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var disableOfferCommand = cli.Command{
	Name:      "disable",
	Category:  "Offers",
	Usage:     "Stop serving an offer; it stays listed.",
	ArgsUsage: "offer_id",
	Action:    actionDecorator(disableOffer),
}

func disableOffer(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	id, err := offerIDArg(ctx)
	if err != nil {
		return err
	}
	resp, err := client.DisableOffer(ctxc, &offersrpc.DisableOfferRequest{
		OfferId: id,
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var enableOfferCommand = cli.Command{
	Name:      "enable",
	Category:  "Offers",
	Usage:     "Resume serving a disabled offer.",
	ArgsUsage: "offer_id",
	Action:    actionDecorator(enableOffer),
}

func enableOffer(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	id, err := offerIDArg(ctx)
	if err != nil {
		return err
	}
	resp, err := client.EnableOffer(ctxc, &offersrpc.EnableOfferRequest{
		OfferId: id,
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

func offerIDArg(ctx *cli.Context) ([]byte, error) {
	if !ctx.Args().Present() {
		return nil, fmt.Errorf("offer_id argument missing")
	}
	id, err := hex.DecodeString(strings.TrimSpace(ctx.Args().First()))
	if err != nil {
		return nil, fmt.Errorf("offer_id must be hex: %w", err)
	}

	return id, nil
}

var decodeBolt12Command = cli.Command{
	Name:     "decode",
	Category: "Offers",
	Usage: "Decode an offer (lno1...), invoice request (lnr1...) or " +
		"invoice (lni1...).",
	ArgsUsage: "bolt12_string",
	Action:    actionDecorator(decodeBolt12),
}

func decodeBolt12(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	if !ctx.Args().Present() {
		return fmt.Errorf("bolt12 string argument missing")
	}
	resp, err := client.DecodeBolt12(ctxc, &offersrpc.DecodeBolt12Request{
		Bolt12: ctx.Args().First(),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var fetchInvoiceCommand = cli.Command{
	Name:     "fetchinvoice",
	Category: "Offers",
	Usage:    "Ask an offer's issuer for an invoice, without paying it.",
	Description: `
	Send an invoice request for the offer over onion messages and wait for
	the invoice. The invoice is checked against the request and returned;
	nothing is paid. Pay it with "offer pay --invoice".`,
	ArgsUsage: "offer",
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name: "amount_msat",
			Usage: "the amount to ask to be invoiced, in " +
				"millisatoshi; required for an offer without " +
				"an amount",
		},
		cli.Uint64Flag{
			Name:  "quantity",
			Usage: "how many of the offer's item",
		},
		cli.StringFlag{
			Name:  "payer_note",
			Usage: "a note for the issuer",
		},
		cli.Uint64Flag{
			Name:  "timeout",
			Usage: "how long to wait for the invoice, in seconds",
		},
	},
	Action: actionDecorator(fetchInvoice),
}

func fetchInvoice(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	if !ctx.Args().Present() {
		return fmt.Errorf("offer argument missing")
	}
	resp, err := client.FetchInvoice(ctxc, &offersrpc.FetchInvoiceRequest{
		Offer:          ctx.Args().First(),
		AmountMsat:     ctx.Uint64("amount_msat"),
		Quantity:       ctx.Uint64("quantity"),
		PayerNote:      ctx.String("payer_note"),
		TimeoutSeconds: uint32(ctx.Uint64("timeout")),
	})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var payOfferCommand = cli.Command{
	Name:     "pay",
	Category: "Offers",
	Usage:    "Fetch an invoice for an offer and pay it.",
	Description: `
	Send an invoice request for the offer over onion messages, check the
	invoice that comes back, and pay it. With --invoice, pay an invoice
	fetched earlier instead.`,
	ArgsUsage: "offer",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "invoice",
			Usage: "an invoice fetched earlier to pay, lni1...",
		},
		cli.Uint64Flag{
			Name: "amount_msat",
			Usage: "the amount to ask to be invoiced, in " +
				"millisatoshi; required for an offer without " +
				"an amount",
		},
		cli.Uint64Flag{
			Name:  "quantity",
			Usage: "how many of the offer's item",
		},
		cli.StringFlag{
			Name:  "payer_note",
			Usage: "a note for the issuer",
		},
		cli.Uint64Flag{
			Name: "timeout",
			Usage: "how long to wait for the invoice and then " +
				"for the payment, in seconds each",
		},
		cli.Uint64Flag{
			Name: "fee_limit_msat",
			Usage: "the most to pay in routing fees, in " +
				"millisatoshi; zero means the node's default, " +
				"as for payinvoice",
		},
		cli.Uint64Flag{
			Name:  "max_parts",
			Usage: "the most parts to split the payment into",
		},
	},
	Action: actionDecorator(payOffer),
}

func payOffer(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	req := &offersrpc.PayOfferRequest{
		Invoice:        ctx.String("invoice"),
		AmountMsat:     ctx.Uint64("amount_msat"),
		Quantity:       ctx.Uint64("quantity"),
		PayerNote:      ctx.String("payer_note"),
		TimeoutSeconds: uint32(ctx.Uint64("timeout")),
		FeeLimitMsat:   ctx.Uint64("fee_limit_msat"),
		MaxParts:       uint32(ctx.Uint64("max_parts")),
	}
	if ctx.Args().Present() {
		req.Offer = ctx.Args().First()
	}
	if req.Offer == "" && req.Invoice == "" {
		return fmt.Errorf("an offer argument or --invoice is required")
	}
	resp, err := client.PayOffer(ctxc, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var listOfferInvoicesCommand = cli.Command{
	Name:     "invoices",
	Category: "Offers",
	Usage:    "List the invoices issued for this node's offers.",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "offer_id",
			Usage: "list only the invoices for this offer",
		},
	},
	Action: actionDecorator(listOfferInvoices),
}

func listOfferInvoices(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getOffersClient(ctx)
	defer cleanUp()

	var id []byte
	if ctx.IsSet("offer_id") {
		var err error
		id, err = hex.DecodeString(strings.TrimSpace(ctx.String("offer_id")))
		if err != nil {
			return fmt.Errorf("offer_id must be hex: %w", err)
		}
	}
	resp, err := client.ListOfferInvoices(ctxc,
		&offersrpc.ListOfferInvoicesRequest{OfferId: id})
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}
