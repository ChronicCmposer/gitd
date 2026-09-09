package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/ChronicCmposer/gitd/internal/objectstore/s3"
)

// runMirror inspects and manages S3 bundle mirrors (3.4): list <repo> emits
// NDJSON {repo, bundles:[...]} (R13-Q6), delete <repo> removes every bundle
// (R6-Q9), and fetch <repo> <dest> restores from the latest bundle (R8-Q2,
// R11-Q5).
func runMirror(args []string, stdout, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	sub := "list"
	var subArgs []string
	if len(rest) > 0 {
		sub = rest[0]
		subArgs = rest[1:]
	}
	// Validate usage before touching config (fail-fast, exit 2 on usage).
	switch sub {
	case "list", "delete":
		if len(subArgs) != 1 {
			return errUsage("usage: gitd mirror %s <repo>", sub)
		}
	case "fetch":
		if len(subArgs) != 2 {
			return errUsage("usage: gitd mirror fetch <repo> <dest>")
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
		return mirrorList(m, subArgs[0], stdout)
	case "delete":
		return m.Delete(ctx, subArgs[0])
	case "fetch":
		return m.Fetch(ctx, subArgs[0], subArgs[1])
	}
	return nil
}

// storeFor builds the configured objectstore backend (s3 in v1, R6-Q7).
func storeFor(gitd *config.GitdConfig) (objectstore.Store, error) {
	return s3.New(context.Background(), gitd.Storage.Bucket, gitd.Storage.Region)
}

// mirrorList emits one NDJSON object {repo, bundles} (R13-Q6).
func mirrorList(m *mirror.Mirror, repoName string, stdout io.Writer) error {
	keys, err := m.List(context.Background(), repoName)
	if err != nil {
		return err
	}
	out := struct {
		Repo    string   `json:"repo"`
		Bundles []string `json:"bundles"`
	}{Repo: repoName, Bundles: keys}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return fmt.Errorf("mirror list: encode: %w", err)
	}
	return nil
}
