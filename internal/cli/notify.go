package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/repo"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

// revListMax is the commit-summary cap (R13-Q10: bounded work inside the 60s
// socket round-trip).
const revListMax = 51

// runNotify handles a post-receive notification (3.5): validates the cwd
// (R9-Q6), writes one spool event per ref line (R11-Q1), submits the bundle
// upload to serve over the unix socket, and runs sync-mode deliveries (R10-Q2).
func runNotify(args []string, _, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errUsage("notify takes no arguments")
	}

	n, err := newNotifier(cfg)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("notify: getwd: %w", err)
	}
	return n.Notify(cwd, os.Stdin)
}

// notifier holds the notify dependencies; constructed from gitd.yaml +
// webhooks.yaml so tests can build it directly with temp dirs.
type notifier struct {
	gitd      *config.GitdConfig
	webhooks  *config.WebhooksConfig
	reposRoot string
	git       *gitenv.Runner
	spool     *spool.Store
	client    *socket.Client
	now       func() time.Time
	log       *slog.Logger
}

// newNotifier wires a notifier from the on-disk configs (fail-fast startup).
func newNotifier(gitdPath string) (*notifier, error) {
	gitd, err := config.LoadGitd(gitdPath)
	if err != nil {
		return nil, err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return nil, err
	}
	webhooks, err := config.LoadWebhooks(webhooksPathFor(gitdPath))
	if err != nil {
		return nil, err
	}
	return &notifier{
		gitd:      gitd,
		webhooks:  webhooks,
		reposRoot: reposRoot,
		git:       gitenv.NewRunner(gitd.GitBinary, gitHome, os.Getenv("PATH")).WithObjectFormat(gitd.ObjectFormat),
		spool:     spool.NewStore(spoolDir, time.Now, gitd.Spool.Retention.D(), log),
		client:    socket.NewClient(socketPath, socketTimeout),
		now:       time.Now,
		log:       log,
	}, nil
}

// Notify processes one post-receive invocation whose hook cwd is the repo.
func (n *notifier) Notify(cwd string, stdin io.Reader) error {
	resolved, err := repo.RealpathUnder(n.reposRoot, cwd)
	if err != nil {
		return fmt.Errorf("notify: cwd %s: %w", cwd, err)
	}
	bare, err := repo.IsBareRepo(resolved)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	if !bare {
		return fmt.Errorf("notify: %s is not a bare repository", resolved)
	}
	name := repo.Normalize(filepath.Base(resolved))
	if !repo.ValidName(name) {
		return fmt.Errorf("notify: invalid repo name %q", name)
	}

	lines := n.parseRefLines(stdin)
	var eventIDs []string
	for _, ln := range lines {
		ev, err := n.buildEvent(resolved, name, ln)
		if err != nil {
			n.log.Error("notify: skipping ref line", "line", ln.raw, "error", err)
			continue
		}
		id, err := n.spool.Write(ev)
		if err != nil {
			return fmt.Errorf("notify: spool write: %w", err)
		}
		eventIDs = append(eventIDs, id)
	}
	n.log.Info("events spooled", "repo", name, "count", len(eventIDs))

	// Bundle upload: one per push invocation (R11-Q1), synchronous (R5-Q2).
	ctx := context.Background()
	result, err := n.client.Bundle(ctx, name)
	if err != nil {
		return fmt.Errorf("notify: bundle upload: %w", err)
	}
	n.log.Info("bundle result", "repo", name, "uploaded", result.Uploaded, "reason", result.Reason)

	// Sync-mode deliveries: serial per plugin, fail-fast (R10-Q2, R12-Q10).
	for _, p := range n.webhooks.Plugins {
		if !p.Sync {
			continue
		}
		for _, id := range eventIDs {
			if err := n.client.Deliver(ctx, p.ID, id); err != nil {
				return fmt.Errorf("notify: sync delivery plugin %s event %s: %w", p.ID, id, err)
			}
			n.log.Info("sync delivery ok", "plugin", p.ID, "event-id", id)
		}
	}
	return nil
}

// refLine is one strict hook stdin line: "<old-sha> <new-sha> <ref>".
type refLine struct {
	old, new, ref string
	raw           string
}

// parseRefLines reads post-receive stdin leniently but loudly (R9-Q7):
// malformed lines are audit-logged and skipped — a post-receive hook must
// never fail a good push. Empty input yields zero lines (no events, but the
// bundle still uploads: one /v1/bundle per push).
func (n *notifier) parseRefLines(r io.Reader) []refLine {
	var lines []refLine
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !validSHA(f[0]) || !validSHA(f[1]) || !strings.HasPrefix(f[2], "refs/") {
			n.log.Error("notify: malformed post-receive line", "line", line)
			continue
		}
		lines = append(lines, refLine{old: f[0], new: f[1], ref: f[2], raw: line})
	}
	return lines
}

// buildEvent types one ref line (R10-Q7) and attaches commit summaries
// (rev-list capped at revListMax, R13-Q10).
func (n *notifier) buildEvent(repoDir, repoName string, ln refLine) (*event.Event, error) {
	typ := event.TypePush
	switch {
	case isZeroSHA(ln.old) && !isZeroSHA(ln.new):
		typ = event.TypeRefCreated
	case !isZeroSHA(ln.old) && isZeroSHA(ln.new):
		typ = event.TypeRefDeleted
	}
	commits, err := commitSummaries(n.git, repoDir, ln.old, ln.new)
	if err != nil {
		return nil, fmt.Errorf("rev-list: %w", err)
	}
	return &event.Event{
		SchemaVersion: event.SchemaVersion,
		CreatedAt:     n.now().UTC().Format(time.RFC3339),
		Repo:          repoName,
		Ref:           ln.ref,
		Type:          typ,
		Commits:       commits,
	}, nil
}

// commitSummaries returns the commits in old..new (or new alone for a ref
// creation), capped at revListMax. A ref deletion has no commits.
func commitSummaries(git *gitenv.Runner, repoDir, oldSHA, newSHA string) ([]event.Commit, error) {
	if isZeroSHA(newSHA) {
		return nil, nil
	}
	arg := newSHA
	if !isZeroSHA(oldSHA) {
		arg = oldSHA + ".." + newSHA
	}
	out, err := git.RunIn(context.Background(), repoDir, "rev-list",
		fmt.Sprintf("--max-count=%d", revListMax), "--format=%H%x00%s%x00%an <%ae>", arg)
	if err != nil {
		return nil, err
	}
	var commits []event.Commit
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "commit ") {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected rev-list line %q", line)
		}
		commits = append(commits, event.Commit{SHA: parts[0], Subject: parts[1], Author: parts[2]})
	}
	return commits, nil
}

// validSHA reports a 40- or 64-hex object id (hook input is format-agnostic).
func validSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isZeroSHA reports the all-zero object id of either hash length.
func isZeroSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}
