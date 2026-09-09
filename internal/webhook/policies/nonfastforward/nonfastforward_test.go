package nonfastforward

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

const gitBin = "/usr/bin/git"

// makeRepo builds a bare repo with main (c1, c2) and a diverged branch dev
// (c1, c3): dev is NOT a fast-forward of main and vice versa. Returns the
// repo dir and the main commit shas.
func makeRepo(t *testing.T) (string, string, string) {
	t.Helper()
	if _, err := exec.LookPath(gitBin); err != nil {
		t.Skip("git not available")
	}
	work := t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(work, "init", "-q", "-b", "main", "--object-format=sha256", ".")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a")
	run(work, "commit", "-qm", "c1")
	c1 := strings.TrimSpace(runOutput(t, work, "rev-parse", "HEAD"))
	run(work, "checkout", "-q", "-b", "dev")
	if err := os.WriteFile(filepath.Join(work, "b"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "b")
	run(work, "commit", "-qm", "c3")
	run(work, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(work, "c"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "c")
	run(work, "commit", "-qm", "c2")
	c2 := strings.TrimSpace(runOutput(t, work, "rev-parse", "HEAD"))

	repoDir := filepath.Join(t.TempDir(), "r.git")
	run(t.TempDir(), "clone", "-q", "--bare", work, repoDir)
	return repoDir, c1, c2
}

func runOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func testPolicy(t *testing.T, cfg map[string]any) webhook.Policy {
	t.Helper()
	repoDir, _, _ := makeRepo(t)
	git := gitenv.NewRunner(gitBin, t.TempDir(), os.Getenv("PATH"))
	p, err := New(cfg, webhook.PolicyDeps{Git: git, RepoDir: repoDir})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func zero() string { return strings.Repeat("0", 64) }

func TestEvaluateFastForwardAccepts(t *testing.T) {
	p := testPolicy(t, nil)
	repoDir, c1, c2 := makeRepo(t)
	_ = repoDir
	// c1..c2 is a fast-forward on main: accept.
	if err := p.Evaluate(context.Background(), webhook.RefUpdate{Old: c1, New: c2, Ref: "refs/heads/main"}); err != nil {
		t.Errorf("fast-forward update rejected: %v", err)
	}
}

func TestEvaluateNonFastForwardRejects(t *testing.T) {
	p := testPolicy(t, nil)
	repoDir, c1, c2 := makeRepo(t)
	_ = repoDir
	// Reversing main c2 -> c1 is a non-fast-forward: reject.
	err := p.Evaluate(context.Background(), webhook.RefUpdate{Old: c2, New: c1, Ref: "refs/heads/main"})
	if !errors.Is(err, ErrRejected) {
		t.Errorf("non-fast-forward err = %v, want ErrRejected", err)
	}
}

func TestEvaluateCreateDeletePass(t *testing.T) {
	p := testPolicy(t, nil)
	repoDir, _, c2 := makeRepo(t)
	_ = repoDir
	if err := p.Evaluate(context.Background(), webhook.RefUpdate{Old: zero(), New: c2, Ref: "refs/heads/new"}); err != nil {
		t.Errorf("ref creation rejected: %v", err)
	}
	if err := p.Evaluate(context.Background(), webhook.RefUpdate{Old: c2, New: zero(), Ref: "refs/heads/main"}); err != nil {
		t.Errorf("ref deletion rejected: %v", err)
	}
}

func TestEvaluateUnguardedRefPasses(t *testing.T) {
	p := testPolicy(t, nil)
	repoDir, c1, c2 := makeRepo(t)
	_ = repoDir
	// Tags are not branch refs: the guard does not apply (R4-Q1 scope).
	if err := p.Evaluate(context.Background(), webhook.RefUpdate{Old: c2, New: c1, Ref: "refs/tags/v1"}); err != nil {
		t.Errorf("tag update rejected: %v", err)
	}
}

func TestEvaluateBranchGlob(t *testing.T) {
	p := testPolicy(t, map[string]any{"branches": []any{"main"}})
	repoDir, c1, c2 := makeRepo(t)
	_ = repoDir
	// dev is unguarded by branches: ["main"].
	if err := p.Evaluate(context.Background(), webhook.RefUpdate{Old: c2, New: c1, Ref: "refs/heads/dev"}); err != nil {
		t.Errorf("unguarded branch rejected: %v", err)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	git := gitenv.NewRunner(gitBin, t.TempDir(), os.Getenv("PATH"))
	tests := []map[string]any{
		{"branches": "main"},    // not a list
		{"branches": []any{42}}, // non-string glob
	}
	for _, cfg := range tests {
		if _, err := New(cfg, webhook.PolicyDeps{Git: git, RepoDir: t.TempDir()}); err == nil {
			t.Errorf("New(%v) = nil error, want config rejection", cfg)
		}
	}
}

func TestRegistrationInit(t *testing.T) {
	repoDir, _, _ := makeRepo(t)
	git := gitenv.NewRunner(gitBin, t.TempDir(), os.Getenv("PATH"))
	engine := webhook.NewPolicyEngine(webhook.DefaultPolicies, webhook.PolicyDeps{Git: git, RepoDir: repoDir})
	pc := config.PoliciesConfig{Enabled: []string{Name}, Config: map[string]any{Name: nil}}
	if err := engine.Build(pc); err != nil {
		t.Fatalf("engine.Build = %v", err)
	}
	if err := engine.Evaluate(nil); err != nil {
		t.Fatalf("engine.Evaluate = %v", err)
	}
}
