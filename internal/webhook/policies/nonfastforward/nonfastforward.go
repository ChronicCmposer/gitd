// Package nonfastforward implements the example pre-receive policy (4.4,
// R4-Q1/R9-Q5): it rejects non-fast-forward updates to guarded branch refs. It
// is disabled by default (policies.enabled empty).
package nonfastforward

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"

	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

// Name is the policy name as it appears in gitd.yaml policies.enabled.
const Name = "non-fast-forward"

// ErrRejected is the sentinel surfaced on the git client's stderr when the
// guard rejects a push (R1-Q8: sentinel for policy rejections).
var ErrRejected = errors.New("rejected: ref is not a fast-forward (fetch or pull before pushing)")

// branchPrefix is the ref namespace this guard applies to.
const branchPrefix = "refs/heads/"

// Policy rejects non-fast-forward updates to branch refs matching its globs.
type Policy struct {
	git      *gitenv.Runner
	repoDir  string
	branches []string
}

// New builds the policy from its free-form config map (R9-Q5, R10-Q6):
//
//	non-fast-forward:
//	  branches: ["*"]
//
// A missing branches key defaults to ["*"] (every branch must fast-forward).
func New(cfg map[string]any, deps webhook.PolicyDeps) (webhook.Policy, error) {
	p := &Policy{git: deps.Git, repoDir: deps.RepoDir, branches: []string{"*"}}
	if cfg == nil {
		return p, nil
	}
	raw, ok := cfg["branches"]
	if !ok {
		return p, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("non-fast-forward: branches: must be a list of globs")
	}
	globs := make([]string, 0, len(list))
	for _, g := range list {
		s, ok := g.(string)
		if !ok {
			return nil, fmt.Errorf("non-fast-forward: branches: entries must be strings")
		}
		globs = append(globs, s)
	}
	p.branches = globs
	return p, nil
}

// Evaluate rejects a non-fast-forward update to a guarded ref. Ref creations
// and deletions are not fast-forward concerns and pass.
func (p *Policy) Evaluate(ctx context.Context, line webhook.RefUpdate) error {
	if isZero(line.Old) || isZero(line.New) {
		return nil
	}
	if !p.guards(line.Ref) {
		return nil
	}
	ancestor, err := isAncestor(ctx, p.git, p.repoDir, line.Old, line.New)
	if err != nil {
		return fmt.Errorf("non-fast-forward: check: %w", err)
	}
	if !ancestor {
		return ErrRejected
	}
	return nil
}

// guards reports whether ref is a branch matching one of the policy globs.
// Globs match the branch short name (the "refs/heads/" prefix is implied), so
// branches: ["*"] guards every branch.
func (p *Policy) guards(ref string) bool {
	name, ok := strings.CutPrefix(ref, branchPrefix)
	if !ok {
		return false
	}
	for _, g := range p.branches {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// isAncestor runs `git merge-base --is-ancestor old new` in the repo: exit 0
// = old is an ancestor of new (fast-forward), exit 1 = not, any other exit is
// a check error. The caller's context bounds the exec (R5-Q1).
func isAncestor(ctx context.Context, git *gitenv.Runner, dir, old, new string) (bool, error) {
	cmd := git.Git(ctx, "merge-base", "--is-ancestor", old, new)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// isZero reports the all-zero object id of either hash length (ref creation or
// deletion marker).
func isZero(s string) bool {
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

func init() {
	webhook.DefaultPolicies.Register(Name, New)
}
