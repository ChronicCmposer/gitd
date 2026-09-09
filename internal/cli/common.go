package cli

import (
	"flag"
	"io"
	"path/filepath"
	"time"

	"github.com/ChronicCmposer/gitd/internal/socket"
)

// Runtime paths pinned by the plan. The image bakes /srv/git (repos),
// /var/spool/gitd (spool + socket + git HOME per image/fs/etc/passwd), and
// /usr/local/lib/gitd/hooks (R6-Q2). Config defaults to the /etc/gitd layout
// used by the systemd units and hook shims (R11-Q7).
//
// The paths are vars (not consts) so tests can point them at temp dirs; the
// production values never change. This mirrors the ddnsEndpoint/submitWait
// test-seam pattern used elsewhere in the codebase.
var (
	reposRoot     = "/srv/git"
	spoolDir      = "/var/spool/gitd"
	hooksDir      = "/usr/local/lib/gitd/hooks"
	gitHome       = "/var/spool/gitd"
	defaultConfig = "/etc/gitd/gitd.yaml"
)

// socketPath is the unix socket for notify/spool replay submissions. It is a
// var so tests can point it at a temp socket; production uses
// socket.DefaultPath (/var/spool/gitd/gitd.sock, R9-Q11).
var socketPath = socket.DefaultPath

// socketTimeout is notify's outer bound on a socket round-trip (R11-Q3).
const socketTimeout = 60 * time.Second

// parseConfigFlag parses the shared --config flag (stdlib flag, R1-Q5) and
// returns the gitd.yaml path plus the remaining positional arguments.
func parseConfigFlag(args []string) (path string, rest []string, err error) {
	fs := flag.NewFlagSet("gitd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := fs.String("config", defaultConfig, "path to gitd.yaml")
	if err := fs.Parse(args); err != nil {
		return "", nil, errUsage("bad flags: %v", err)
	}
	return *cfg, fs.Args(), nil
}

// webhooksPathFor derives the webhooks.yaml path beside gitd.yaml (R6-Q5:
// webhooks.yaml lives in /etc/gitd and is git-readable).
func webhooksPathFor(gitdPath string) string {
	return filepath.Join(filepath.Dir(gitdPath), "webhooks.yaml")
}
