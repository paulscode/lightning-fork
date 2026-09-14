//go:build !offersrpc
// +build !offersrpc

package commands

import "github.com/urfave/cli"

// offersCommands will return nil for non-offersrpc builds.
func offersCommands() []cli.Command {
	return nil
}
