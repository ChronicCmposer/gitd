package browse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runIn runs a git command in dir with a clean global config, failing the test
// on any error.
func runIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// pushIntoBare creates a fresh work repo on branch and pushes it into the bare
// repo at bareDir, leaving the bare repo's unborn HEAD untouched.
func pushIntoBare(t *testing.T, bareDir, branch string) {
	t.Helper()
	work := t.TempDir()
	runIn(t, work, "init", "-q", "-b", branch, "--object-format=sha256", ".")
	runIn(t, work, "config", "user.email", "t@t")
	runIn(t, work, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(t, work, "add", "-A")
	runIn(t, work, "commit", "-qm", "first")
	runIn(t, work, "push", "-q", bareDir, branch)
}

// defaultRefHandler returns a Handler wired with a git Runner whose reposRoot
// points at root, so defaultRef can be exercised directly on bare repos.
func defaultRefHandler(t *testing.T, root string) *Handler {
	t.Helper()
	return &Handler{git: testGit(t), reposRoot: root}
}

// TestDefaultRefUnbornHeadMismatch reproduces the gateway bug: a bare repo's
// unborn HEAD names "master" but a client pushed "main", so defaultRef must
// fall back to the existing "main" ref instead of returning the unresolvable
// "master" (which would 500 commitLog/ls-tree).
func TestDefaultRefUnbornHeadMismatch(t *testing.T) {
	root := t.TempDir()
	bareDir := filepath.Join(root, "mismatch.git")
	runIn(t, root, "init", "-q", "--bare", "--object-format=sha256", bareDir)
	pushIntoBare(t, bareDir, "main")

	h := defaultRefHandler(t, root)
	ref, err := h.defaultRef(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("defaultRef = %v", err)
	}
	if ref != "main" {
		t.Errorf("defaultRef = %q, want %q (existing ref, not unborn %q)", ref, "main", "master")
	}
}

// TestDefaultRefUnbornHeadMatches keeps the matching case working: when the
// pushed branch equals the unborn HEAD name, defaultRef still returns it.
func TestDefaultRefUnbornHeadMatches(t *testing.T) {
	root := t.TempDir()
	bareDir := filepath.Join(root, "match.git")
	runIn(t, root, "init", "-q", "--bare", "--object-format=sha256", bareDir)
	pushIntoBare(t, bareDir, "master")

	h := defaultRefHandler(t, root)
	ref, err := h.defaultRef(context.Background(), bareDir)
	if err != nil {
		t.Fatalf("defaultRef = %v", err)
	}
	if ref != "master" {
		t.Errorf("defaultRef = %q, want %q", ref, "master")
	}
}
