package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/ChronicCmposer/gitd/internal/objectstore/s3"
)

// runMirror inspects and manages S3 bundle mirrors (3.4): list [<repo>] emits
// NDJSON {repo, bundles:[...]} (R13-Q6) — with no <repo> it emits one object
// per mirrored repo — delete <repo> removes every bundle (R6-Q9), and
// fetch <repo> [dest] restores from the latest bundle (R8-Q2, R11-Q5),
// defaulting dest to reposRoot/<repo>.git when omitted. Bare `gitd mirror`
// and `gitd mirror help` print the subcommand reference.
func runMirror(args []string, stdout, stderr io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	// Bare `gitd mirror` is a usage error: surface the subcommand reference so
	// fetch/list/delete are discoverable (fail-fast, exit 2 on usage).
	if len(rest) == 0 {
		return errUsage("%s", mirrorUsageText())
	}
	sub := rest[0]
	subArgs := rest[1:]
	// `gitd mirror help` is an explicit help request: print to stdout, exit 0.
	if sub == "help" {
		if len(subArgs) != 0 {
			return errUsage("usage: gitd mirror help")
		}
		mirrorUsage(stdout)
		return nil
	}
	// Validate usage before touching config (fail-fast, exit 2 on usage).
	switch sub {
	case "list":
		if len(subArgs) > 1 {
			return errUsage("usage: gitd mirror list [<repo>]")
		}
	case "delete":
		if len(subArgs) != 1 {
			return errUsage("usage: gitd mirror %s <repo>", sub)
		}
	case "fetch":
		if len(subArgs) < 1 || len(subArgs) > 2 {
			return errUsage("usage: gitd mirror fetch <repo> [dest]")
		}
	default:
		return errUsage("unknown mirror subcommand %q (list|delete|fetch)", sub)
	}

	gitd, err := config.LoadGitd(cfg)
	if err != nil {
		return err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return err
	}
	store, err := storeFor(gitd)
	if err != nil {
		return err
	}
	m := mirror.New(store, gitenv.NewRunner(gitd.GitBinary, gitHome, os.Getenv("PATH")),
		reposRoot, gitd.Storage.Prefix, spoolDir, time.Now, log)
	ctx := context.Background()

	switch sub {
	case "list":
		if len(subArgs) == 1 {
			return mirrorList(m, subArgs[0], stdout)
		}
		return mirrorListAll(m, stdout)
	case "delete":
		return m.Delete(ctx, subArgs[0])
	case "fetch":
		if len(subArgs) == 1 {
			return m.Restore(ctx, subArgs[0])
		}
		return m.Fetch(ctx, subArgs[0], subArgs[1])
	}
	return nil
}

// mirrorUsageText renders the gitd mirror subcommand reference: list with no
// <repo> enumerates every mirrored repo; fetch dest is optional and defaults
// to reposRoot/<repo>.git (/srv/git/<repo>.git).
func mirrorUsageText() string {
	return `usage: gitd mirror <command> [args]

commands:
  list [<repo>]         list S3 bundle mirrors; with no <repo>, lists every
                        repo that has mirrors (one NDJSON object per repo)
  delete <repo>         delete all bundle mirrors for <repo>
  fetch <repo> [dest]   restore <repo> from its latest bundle into <dest>
                        (dest defaults to /srv/git/<repo>.git)

exit codes: 0 ok, 1 runtime error, 2 usage error
`
}

// mirrorUsage writes the gitd mirror subcommand reference to w.
func mirrorUsage(w io.Writer) {
	_, _ = io.WriteString(w, mirrorUsageText())
}

// storeFor builds the configured objectstore backend (s3 in v1, R6-Q7).
func storeFor(gitd *config.GitdConfig) (objectstore.Store, error) {
	return s3.New(context.Background(), gitd.Storage.Bucket, gitd.Storage.Region)
}

// mirrorList emits the NDJSON object {repo, bundles} for one repo's bundles
// (R13-Q6).
func mirrorList(m *mirror.Mirror, repoName string, stdout io.Writer) error {
	keys, err := m.List(context.Background(), repoName)
	if err != nil {
		return err
	}
	return emitMirror(repoName, keys, stdout)
}

// mirrorListAll emits one NDJSON object {repo, bundles} per mirrored repo,
// sorted by repo name, with the same shape as the single-repo list (R13-Q6).
// Repos without bundles are absent (no keys), and no repos at all emits
// nothing.
func mirrorListAll(m *mirror.Mirror, stdout io.Writer) error {
	repos, err := m.ListAllRepos(context.Background())
	if err != nil {
		return err
	}
	names := make([]string, 0, len(repos))
	for name := range repos {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := emitMirror(name, repos[name], stdout); err != nil {
			return err
		}
	}
	return nil
}

// emitMirror writes one NDJSON object {repo, bundles} (R13-Q6), shared by the
// single-repo and all-repos list paths.
func emitMirror(repoName string, bundles []string, stdout io.Writer) error {
	out := struct {
		Repo    string   `json:"repo"`
		Bundles []string `json:"bundles"`
	}{Repo: repoName, Bundles: bundles}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return fmt.Errorf("mirror list: encode: %w", err)
	}
	return nil
}
