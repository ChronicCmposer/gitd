// Package cli dispatches gitd subcommands and owns the CLI conventions from
// R1-Q5: stdlib flag, exit 0 ok / 1 runtime / 2 usage, errors on stderr as
// "gitd: <err>".
//
// Each verb lives in its own file; Phase 1 ships the dispatch table plus thin
// stubs. Later phases fill the stubs with real implementations without
// changing the dispatch plumbing.
package cli

import (
	"errors"
	"fmt"
	"io"
)

// Exit codes per the R1-Q5 convention.
const (
	ExitOK    = 0 // command succeeded
	ExitError = 1 // runtime failure
	ExitUsage = 2 // usage error
)

// usageError marks an argument/usage failure; Run maps it to ExitUsage.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

// errUsage builds a usage error that Run maps to exit code 2.
func errUsage(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// errNotImplemented reports a verb that exists but has no implementation yet.
// Run prefixes the message with the verb, yielding "gitd: <verb>: not
// implemented in this phase".
func errNotImplemented() error {
	return fmt.Errorf("not implemented in this phase")
}

// command is one gitd subcommand. run receives the arguments after the verb.
type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) error
}

// commands is the gitd subcommand table. Order matters for usage output.
var commands = []command{
	{name: "serve", summary: "run the git gateway service", run: runServe},
	{name: "notify", summary: "handle a post-receive webhook notification", run: runNotify},
	{name: "pre-receive", summary: "enforce pre-receive push policies", run: runPreReceive},
	{name: "spool", summary: "inspect, replay, and purge the webhook event spool", run: runSpool},
	{name: "ddns", summary: "refresh the dynamic DNS record", run: runDDNS},
	{name: "mirror", summary: "inspect and manage S3 bundle mirrors", run: runMirror},
	{name: "version", summary: "print the gitd version", run: runVersion},
}

// Run executes the subcommand named by argv[0] (or prints usage when argv is
// empty) and returns the process exit code. It never calls os.Exit; the
// caller owns the process.
func Run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		usage(stderr)
		return ExitUsage
	}

	verb := argv[0]
	for _, cmd := range commands {
		if cmd.name == verb {
			if err := cmd.run(argv[1:], stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "gitd: %s: %v\n", verb, err)
				var ue *usageError
				if errors.As(err, &ue) {
					return ExitUsage
				}
				return ExitError
			}
			return ExitOK
		}
	}

	fmt.Fprintf(stderr, "gitd: unknown command %q\n", verb)
	usage(stderr)
	return ExitUsage
}

// usage writes the command summary to w.
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: gitd <command> [args]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "commands:")
	for _, cmd := range commands {
		fmt.Fprintf(w, "  %-12s %s\n", cmd.name, cmd.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "exit codes: 0 ok, 1 runtime error, 2 usage error")
}
