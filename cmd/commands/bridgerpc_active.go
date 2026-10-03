//go:build bridgerpc
// +build bridgerpc

package commands

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/bridgerpc"
	"github.com/urfave/cli"
)

// bridgeCommands will return the set of commands to enable for bridgerpc
// builds.
func bridgeCommands() []cli.Command {
	return []cli.Command{
		{
			Name:     "bridge",
			Category: "Bridge",
			Usage: "Run a bridge that pays invoices on the SHA256 " +
				"chain's Lightning network for others.",
			Subcommands: []cli.Command{
				bridgeStatusCommand,
				bridgeInfoCommand,
				bridgeSetRateCommand,
				bridgeQuoteCommand,
				bridgeSwapCommand,
				bridgeCodeCommand,
			},
		},
	}
}

func getBridgeClient(ctx *cli.Context) (bridgerpc.BridgeClient, func()) {
	conn := getClientConn(ctx, false)
	cleanUp := func() {
		conn.Close()
	}

	return bridgerpc.NewBridgeClient(conn), cleanUp
}

var bridgeStatusCommand = cli.Command{
	Name:  "status",
	Usage: "Whether the bridge can serve swaps, and why not if it cannot.",
	Action: actionDecorator(func(ctx *cli.Context) error {
		client, cleanUp := getBridgeClient(ctx)
		defer cleanUp()

		resp, err := client.Status(getContext(), &bridgerpc.StatusRequest{})
		if err != nil {
			return err
		}
		printRespJSON(resp)

		return nil
	}),
}

var bridgeInfoCommand = cli.Command{
	Name:  "info",
	Usage: "What the bridge would charge now, per direction.",
	Description: `
	Shows what a participant's wallet sees before it asks for a quote:
	the rate, the spread at the bridge's current position, the bounds on
	an invoice, and for a direction that would refuse, why.`,
	Action: actionDecorator(func(ctx *cli.Context) error {
		client, cleanUp := getBridgeClient(ctx)
		defer cleanUp()

		resp, err := client.Info(getContext(), &bridgerpc.InfoRequest{})
		if err != nil {
			return err
		}
		printRespJSON(resp)

		return nil
	}),
}

var bridgeSetRateCommand = cli.Command{
	Name:      "setrate",
	Usage:     "Set the rate the bridge trades at.",
	ArgsUsage: "rate",
	Description: `
	The rate is SHA256 coin per BLAKE2b coin, for example 0.00483. It takes
	effect at once, is kept across restarts, and is stamped with when it
	was set: once it is older than bridgerpc.ratemaxage the bridge stops
	quoting until it is set again.`,
	Action: actionDecorator(func(ctx *cli.Context) error {
		if ctx.NArg() != 1 {
			return cli.ShowCommandHelp(ctx, "setrate")
		}
		rate, err := strconv.ParseFloat(ctx.Args().First(), 64)
		if err != nil {
			return fmt.Errorf("the rate must be a number, such as "+
				"0.00483: %w", err)
		}

		client, cleanUp := getBridgeClient(ctx)
		defer cleanUp()

		resp, err := client.SetRate(getContext(),
			&bridgerpc.SetRateRequest{Rate: rate})
		if err != nil {
			return err
		}
		printRespJSON(resp)

		return nil
	}),
}

var bridgeQuoteCommand = cli.Command{
	Name:      "quote",
	Usage:     "Quote paying an invoice on the other chain.",
	ArgsUsage: "invoice",
	Description: `
	Creates the swap and returns the hold invoice that pays for it. This
	commits the bridge's liquidity until the quote expires, so it is for
	testing; participants' wallets do this themselves.`,
	Action: actionDecorator(func(ctx *cli.Context) error {
		if ctx.NArg() != 1 {
			return cli.ShowCommandHelp(ctx, "quote")
		}

		client, cleanUp := getBridgeClient(ctx)
		defer cleanUp()

		resp, err := client.Quote(getContext(), &bridgerpc.QuoteRequest{
			Invoice: ctx.Args().First(),
		})
		if err != nil {
			return err
		}
		printRespJSON(resp)

		return nil
	}),
}

var bridgeSwapCommand = cli.Command{
	Name:      "swap",
	Usage:     "What the bridge believes about one swap.",
	ArgsUsage: "payment_hash",
	Action: actionDecorator(func(ctx *cli.Context) error {
		if ctx.NArg() != 1 {
			return cli.ShowCommandHelp(ctx, "swap")
		}
		hash, err := hex.DecodeString(ctx.Args().First())
		if err != nil {
			return fmt.Errorf("the payment hash must be hex: %w", err)
		}

		client, cleanUp := getBridgeClient(ctx)
		defer cleanUp()

		resp, err := client.LookupSwap(getContext(),
			&bridgerpc.LookupSwapRequest{Hash: hash})
		if err != nil {
			return err
		}
		printRespJSON(resp)

		return nil
	}),
}

// bridgeParticipantURIs are the calls a participant's macaroon allows, and
// nothing else: what a payer's wallet needs to show a price, ask for a quote
// and follow it.
var bridgeParticipantURIs = []string{
	"/bridgerpc.Bridge/Info",
	"/bridgerpc.Bridge/Quote",
	"/bridgerpc.Bridge/LookupSwap",
}

// firstParticipantRootKeyID is where participants' root keys start, clear of
// the node's own (0) and of the small numbers an operator may have used by
// hand for other macaroons.
const firstParticipantRootKeyID = 1000

// certPin matches the pin format the payer API uses: SHA-256 as colon
// separated hex.
var certPin = regexp.MustCompile(`^([0-9A-Fa-f]{2}:){31}[0-9A-Fa-f]{2}$`)

var bridgeCodeCommand = cli.Command{
	Name:  "code",
	Usage: "Make a bridge code for a participant.",
	Description: `
	Bakes a macaroon that can only ask this bridge for prices and quotes,
	under a root key of its own, and prints it as a bridge code for the
	participant to add in their dashboard ("Paying Bitcoin invoices").

	--url is how the participant's node reaches this node's REST port, for
	example https://abc...xyz.onion:8080. An address that is not an onion
	also needs --cert, the SHA-256 of the certificate it presents.

	Each code has its own root key id, printed with it. To revoke one:

	    lncli deletemacaroonid <root_key_id>

	A code is a credential: send it the way you would send a password.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "url",
			Usage: "the REST address participants reach this node at",
		},
		cli.StringFlag{
			Name:  "label",
			Usage: "a name the participant will see for this service",
		},
		cli.StringFlag{
			Name: "cert",
			Usage: "SHA-256 of the TLS certificate the address " +
				"presents, as colon-separated hex; required " +
				"unless the address is an onion",
		},
		cli.Uint64Flag{
			Name: "root_key_id",
			Usage: "the root key id to bake under; default is the " +
				"next unused one from 1000",
		},
	},
	Action: actionDecorator(bridgeCode),
}

func bridgeCode(ctx *cli.Context) error {
	ctxc := getContext()

	raw := ctx.String("url")
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Scheme != "https" || u.Host == "" ||
		(u.Path != "" && u.Path != "/") {

		return fmt.Errorf("--url must be https://host:port, the REST " +
			"address participants reach this node at")
	}
	onion := strings.HasSuffix(strings.ToLower(u.Hostname()), ".onion")
	cert := ctx.String("cert")
	switch {
	case cert == "" && !onion:
		return fmt.Errorf("an address that is not an onion needs " +
			"--cert, the SHA-256 of the certificate it presents")
	case cert != "" && !certPin.MatchString(cert):
		return fmt.Errorf("--cert must be SHA-256 as colon-separated " +
			"hex, as openssl x509 -fingerprint -sha256 prints it")
	}

	client, cleanUp := getClient(ctx)
	defer cleanUp()

	info, err := client.GetInfo(ctxc, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}

	id := ctx.Uint64("root_key_id")
	if !ctx.IsSet("root_key_id") {
		ids, err := client.ListMacaroonIDs(ctxc,
			&lnrpc.ListMacaroonIDsRequest{})
		if err != nil {
			return err
		}
		id = firstParticipantRootKeyID
		for _, used := range ids.GetRootKeyIds() {
			if used >= id {
				id = used + 1
			}
		}
	}
	if id == 0 {
		return fmt.Errorf("root key 0 is the node's own; a " +
			"participant needs one they can be revoked by")
	}

	perms := make([]*lnrpc.MacaroonPermission, 0, len(bridgeParticipantURIs))
	for _, uri := range bridgeParticipantURIs {
		perms = append(perms, &lnrpc.MacaroonPermission{
			Entity: "uri", Action: uri,
		})
	}
	mac, err := client.BakeMacaroon(ctxc, &lnrpc.BakeMacaroonRequest{
		Permissions: perms, RootKeyId: id,
	})
	if err != nil {
		return err
	}

	body := map[string]any{
		"v":        1,
		"label":    ctx.String("label"),
		"url":      strings.TrimSuffix(raw, "/"),
		"node":     info.GetIdentityPubkey(),
		"macaroon": mac.GetMacaroon(),
	}
	if cert != "" {
		body["cert"] = strings.ToUpper(cert)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}

	out, err := json.MarshalIndent(map[string]any{
		"code": "lfbridge:" +
			base64.RawURLEncoding.EncodeToString(encoded),
		"root_key_id": id,
		"revoke":      fmt.Sprintf("lncli deletemacaroonid %d", id),
	}, "", "    ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))

	return nil
}
