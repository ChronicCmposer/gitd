// Command gitd is the personal git server gateway.
//
// Entry point only: all dispatch lives in internal/cli so subcommands can be
// tested and reused (notify/pre-receive run as git hooks; spool/mirror run
// from the admin shell).
package main

import (
	"os"

	"github.com/ChronicCmposer/gitd/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
