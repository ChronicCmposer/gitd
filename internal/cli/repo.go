package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/ChronicCmposer/gitd/internal/repo"
	"github.com/ChronicCmposer/gitd/internal/socket"
)

// runRepo manages the live repositories under /srv/git (serve-orchestrated
// deletes): list enumerates the bare repos read-only (constants only, no
// config needed), and delete --yes <repo> removes /srv/git/<repo>.git ONLY —
// S3 bundle mirrors are retained, so gitd mirror restore <repo> can still
// recreate it. Delete routes through the serve socket (POST /v1/delete):
// serve stages a delete job for the gitd-restore agent, which performs the
// /srv/git removal as git — the admin never needs elevation and serve never
// writes /srv/git. Bare `gitd repo` and `gitd repo help` print the
// subcommand reference.
func runRepo(args []string, stdout, stderr io.Writer) error {
	yes, _, rest, err := parseRepoFlags(args)
	if err != nil {
		return err
	}
	// Bare `gitd repo` is a usage error: surface the subcommand reference so
	// list/delete are discoverable (fail-fast, exit 2 on usage).
	if len(rest) == 0 {
		return errUsage("%s", repoUsageText())
	}
	sub := rest[0]
	subArgs := rest[1:]
	// `gitd repo help` is an explicit help request: print to stdout, exit 0.
	if sub == "help" {
		if len(subArgs) != 0 {
			return errUsage("usage: gitd repo help")
		}
		repoUsage(stdout)
		return nil
	}
	// Validate usage before doing anything (fail-fast, exit 2 on usage).
	switch sub {
	case "list":
		if len(subArgs) > 0 {
			return errUsage("usage: gitd repo list")
		}
	case "delete":
		// The --yes gate is mandatory: deleting a live repo is destructive
		// (only /srv/git/<repo>.git is removed — bundles are retained).
		if !yes {
			return errUsage("gitd repo delete requires the --yes confirmation gate:\n%s", repoUsageText())
		}
		if len(subArgs) != 1 {
			return errUsage("usage: gitd repo delete --yes <repo>")
		}
	default:
		return errUsage("unknown repo subcommand %q (list|delete|help)", sub)
	}

	switch sub {
	case "list":
		return repoList(stdout)
	case "delete":
		// Validate the name before submitting (fail-fast): an arbitrary
		// destination must never reach the socket.
		if !repo.ValidName(subArgs[0]) {
			return errUsage("invalid repo name %q", subArgs[0])
		}
		return repoDelete(subArgs[0])
	}
	return nil
}

// parseRepoFlags parses the shared --config and the delete --yes flag with
// stdlib flag, allowing either to appear before OR after the positional args
// (the flag package stops at the first positional, so parsing resumes past
// each one). It returns the confirmation flag, the config path (optional —
// the repo verb uses constants), and the remaining positional args.
func parseRepoFlags(args []string) (bool, string, []string, error) {
	fs := flag.NewFlagSet("gitd repo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "confirm repository deletion")
	cfg := fs.String("config", defaultConfig, "path to gitd.yaml")
	var positional []string
	remaining := args
	for {
		if err := fs.Parse(remaining); err != nil {
			return false, "", nil, errUsage("bad flags: %v", err)
		}
		got := fs.Args()
		if len(got) == 0 {
			return *yes, *cfg, positional, nil
		}
		positional = append(positional, got[0])
		remaining = got[1:]
	}
}

// repoList prints one live bare repo name per line (read-only).
func repoList(stdout io.Writer) error {
	names, err := repo.ListBare(reposRoot)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, err := fmt.Fprintln(stdout, name); err != nil {
			return err
		}
	}
	return nil
}

// repoDelete submits a repo delete of repo to the serve socket. Serve stages
// the delete job and the gitd-restore agent removes /srv/git/<repo>.git as
// git — no admin elevation, and serve never writes /srv/git. S3 bundle
// mirrors are NOT touched, so the repo stays restorable via gitd mirror
// restore. The 5-minute client timeout bounds the round-trip; serve down
// fails loudly.
func repoDelete(repoName string) error {
	return socket.NewClient(socketPath, restoreTimeout).DeleteRepo(context.Background(), repoName)
}

// repoUsageText renders the gitd repo subcommand reference: list enumerates
// the live bare repos read-only; delete always targets /srv/git/<repo>.git,
// routes through the serve socket, is performed by the gitd-restore agent
// (no dest arg, no sudo), and is gated by the mandatory --yes confirmation.
func repoUsageText() string {
	return `usage: gitd repo <command> [args]

commands:
  list                list the live bare repositories under /srv/git
                      (read-only; one repo name per line)
  delete --yes <repo> remove /srv/git/<repo>.git from the live repo store
                      (submitted to the gitd-serve socket; the gitd-restore
                      agent performs the /srv/git removal as git). S3 bundle
                      mirrors are retained, so the repo can be recreated with
                      gitd mirror restore <repo>. Requires gitd-serve +
                      gitd-restore running and the mandatory --yes gate
  help                print this reference

exit codes: 0 ok, 1 runtime error, 2 usage error
`
}

// repoUsage writes the gitd repo subcommand reference to w.
func repoUsage(w io.Writer) {
	_, _ = io.WriteString(w, repoUsageText())
}
