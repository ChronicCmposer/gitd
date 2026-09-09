package cli

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/disk"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/webhook"
	// Policy packages self-register their constructors into
	// webhook.DefaultPolicies via init (4.1); the blank import keeps them live
	// in the gitd binary.
	_ "github.com/ChronicCmposer/gitd/internal/webhook/policies/nonfastforward"
)

// runPreReceive enforces pre-receive push gates (3.2, 4.4): strict stdin parse
// (R9-Q7 — a malformed line rejects the push), the statfs disk headroom check
// at the objects-received chokepoint (R7-Q4), and the policy engine
// (fail-closed, R5-Q1).
func runPreReceive(args []string, _, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errUsage("pre-receive takes no arguments")
	}

	gitd, err := config.LoadGitd(cfg)
	if err != nil {
		return err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("pre-receive: getwd: %w", err)
	}
	git := gitenv.NewRunner(gitd.GitBinary, gitHome, os.Getenv("PATH"))
	engine := webhook.NewPolicyEngine(webhook.DefaultPolicies, webhook.PolicyDeps{Git: git, RepoDir: cwd})
	if err := engine.Build(gitd.Policies); err != nil {
		return err
	}
	p := &preReceive{
		headroom: disk.Headroom{MinFree: gitd.DiskMinFreeBytes, WarnFree: 1 << 30},
		engine:   engine,
		log:      log,
	}
	return p.Run(cwd, os.Stdin)
}

// preReceive holds the pre-receive gates; tests build it directly. A nil
// engine means no policies are configured (accept).
type preReceive struct {
	headroom disk.Headroom
	engine   *webhook.PolicyEngine
	log      *slog.Logger
}

// Run executes the gates in order: strict parse, disk headroom, policies.
func (p *preReceive) Run(cwd string, stdin io.Reader) error {
	lines, err := p.parseStrict(stdin)
	if err != nil {
		return err
	}
	if err := p.headroom.Check(cwd, p.log); err != nil {
		return fmt.Errorf("push rejected: %w", err)
	}
	if p.engine == nil {
		return nil
	}
	refs := make([]webhook.RefUpdate, len(lines))
	for i, ln := range lines {
		refs[i] = webhook.RefUpdate{Old: ln.old, New: ln.new, Ref: ln.ref}
	}
	if err := p.engine.Evaluate(refs); err != nil {
		return fmt.Errorf("push rejected: %w", err)
	}
	return nil
}

// parseStrict reads pre-receive stdin; any malformed line rejects the push
// (R9-Q7). Zero lines parse cleanly (R11-Q10: zero lines = accept).
func (p *preReceive) parseStrict(r io.Reader) ([]refLine, error) {
	var lines []refLine
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || !validSHA(f[0]) || !validSHA(f[1]) || !strings.HasPrefix(f[2], "refs/") {
			return nil, fmt.Errorf("push rejected: malformed pre-receive line %q", line)
		}
		lines = append(lines, refLine{old: f[0], new: f[1], ref: f[2], raw: line})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("pre-receive: read stdin: %w", err)
	}
	return lines, nil
}
