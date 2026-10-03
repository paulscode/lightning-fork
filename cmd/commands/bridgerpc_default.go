//go:build !bridgerpc
// +build !bridgerpc

package commands

import "github.com/urfave/cli"

// bridgeCommands will return nil for non-bridgerpc builds.
func bridgeCommands() []cli.Command {
	return nil
}
