package commands

import (
	"errors"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

// spliceFlags are the flags every splice command takes.
var spliceFlags = []cli.Flag{
	cli.StringFlag{
		Name: "chan_point",
		Usage: "the channel's current or original channel point, " +
			"funding_txid:output_index",
	},
	cli.Int64Flag{
		Name:  "amt",
		Usage: "the amount in satoshis",
	},
	cli.BoolFlag{
		Name: "max",
		Usage: "add everything the wallet can spend, or move out " +
			"everything this node's balance allows, after the fee",
	},
	cli.Uint64Flag{
		Name:  "sat_per_vbyte",
		Usage: "the fee rate in sat/vbyte",
	},
	cli.Int64Flag{
		Name: "conf_target",
		Usage: "the confirmation target for the fee rate when " +
			"sat_per_vbyte isn't set (default 6)",
	},
	cli.BoolFlag{
		Name: "force_low_fee",
		Usage: "accept a fee rate below the usual floor (never " +
			"below the relay fee)",
	},
}

var spliceCommand = cli.Command{
	Name:     "splice",
	Category: "Channels",
	Usage:    "Add funds to or move funds out of an open channel.",
	Description: `
	Splicing resizes an open channel without closing it: funds from this
	node's wallet go into the channel, or funds of this node's balance go
	out to an address. The channel stays usable while the splice confirms.
	Both nodes must support splicing.`,
	Subcommands: []cli.Command{
		{
			Name:      "in",
			Usage:     "Add funds from the wallet to the channel.",
			ArgsUsage: "--chan_point=X --amt=N | --max [--utxo=O ...]",
			Flags:     append(spliceFlags, utxoFlag),
			Action:    actionDecorator(spliceIn),
		},
		{
			Name: "out",
			Usage: "Move funds out of the channel to an address, or " +
				"to the wallet.",
			ArgsUsage: "--chan_point=X --amt=N | --max [--addr=A]",
			Flags: append(spliceFlags, cli.StringFlag{
				Name: "addr",
				Usage: "where the funds go; a new address of " +
					"this node's wallet if empty",
			}),
			Action: actionDecorator(spliceOut),
		},
		{
			Name: "estimate",
			Usage: "Show what a splice would cost and allow, " +
				"without starting it.",
			ArgsUsage: "--chan_point=X in|out --amt=N | --max",
			Flags: append(estimateFlags(), cli.StringFlag{
				Name:  "addr",
				Usage: "for out: where the funds would go",
			}, utxoFlag),
			Action: actionDecorator(spliceEstimate),
		},
	},
}

// utxoFlag chooses the wallet coins a splice in may spend.
var utxoFlag = cli.StringSliceFlag{
	Name: "utxo",
	Usage: "for in: a wallet coin, txid:index, the splice may spend; " +
		"repeat it for several. Only these are spent (the amount, " +
		"or with --max all of them); without it the wallet chooses",
}

// spliceUtxos parses the --utxo flags.
func spliceUtxos(ctx *cli.Context) ([]*lnrpc.OutPoint, error) {
	if !ctx.IsSet("utxo") {
		return nil, nil
	}
	outpoints, err := lnd.UtxosToOutpoints(ctx.StringSlice("utxo"))
	if err != nil {
		return nil, fmt.Errorf("unable to decode utxos: %w", err)
	}

	return outpoints, nil
}

// estimateFlags are the splice flags an estimate takes: all but
// force_low_fee, which it can't carry; an estimate below the floor says so.
func estimateFlags() []cli.Flag {
	var flags []cli.Flag
	for _, f := range spliceFlags {
		if f.GetName() != "force_low_fee" {
			flags = append(flags, f)
		}
	}

	return flags
}

// spliceChanPoint parses the --chan_point flag.
func spliceChanPoint(ctx *cli.Context) (*lnrpc.ChannelPoint, error) {
	if !ctx.IsSet("chan_point") {
		return nil, errors.New("chan_point required")
	}

	return parseChanPoint(ctx.String("chan_point"))
}

// runSplice starts a splice and prints its steps until it ends.
func runSplice(ctx *cli.Context, dir lnrpc.SpliceDirection) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	chanPoint, err := spliceChanPoint(ctx)
	if err != nil {
		return err
	}
	if !ctx.IsSet("amt") && !ctx.Bool("max") {
		return errors.New("amt or max required")
	}
	outpoints, err := spliceUtxos(ctx)
	if err != nil {
		return err
	}

	stream, err := client.SpliceChannel(ctxc, &lnrpc.SpliceChannelRequest{
		ChannelPoint: chanPoint,
		Direction:    dir,
		AmountSat:    ctx.Int64("amt"),
		UseMax:       ctx.Bool("max"),
		SatPerVbyte:  ctx.Uint64("sat_per_vbyte"),
		TargetConf:   int32(ctx.Int64("conf_target")),
		Addr:         ctx.String("addr"),
		ForceLowFee:  ctx.Bool("force_low_fee"),
		Outpoints:    outpoints,
	})
	if err != nil {
		return err
	}

	for {
		update, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		printRespJSON(update)
	}
}

func spliceIn(ctx *cli.Context) error {
	return runSplice(ctx, lnrpc.SpliceDirection_SPLICE_IN)
}

func spliceOut(ctx *cli.Context) error {
	return runSplice(ctx, lnrpc.SpliceDirection_SPLICE_OUT)
}

func spliceEstimate(ctx *cli.Context) error {
	ctxc := getContext()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	chanPoint, err := spliceChanPoint(ctx)
	if err != nil {
		return err
	}

	dir := lnrpc.SpliceDirection_SPLICE_IN
	switch ctx.Args().First() {
	case "in":
	case "out":
		dir = lnrpc.SpliceDirection_SPLICE_OUT
	default:
		return errors.New("in or out required")
	}

	outpoints, err := spliceUtxos(ctx)
	if err != nil {
		return err
	}

	resp, err := client.EstimateSplice(ctxc, &lnrpc.EstimateSpliceRequest{
		ChannelPoint: chanPoint,
		Direction:    dir,
		AmountSat:    ctx.Int64("amt"),
		UseMax:       ctx.Bool("max"),
		SatPerVbyte:  ctx.Uint64("sat_per_vbyte"),
		TargetConf:   int32(ctx.Int64("conf_target")),
		Addr:         ctx.String("addr"),
		Outpoints:    outpoints,
	})
	if err != nil {
		return err
	}
	printRespJSON(resp)

	return nil
}
