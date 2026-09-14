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
