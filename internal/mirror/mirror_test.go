package mirror

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
)

const gitBin = "/usr/bin/git"

// gitRunner skips the test when no git binary is available (e.g. a bare
// sandbox); the dev box and the rules_go SDK both provide it.
func gitRunner(t *testing.T) *gitenv.Runner {
	t.Helper()
	if _, err := exec.LookPath(gitBin); err != nil {
		t.Skip("git not available")
	}
	return gitenv.NewRunner(gitBin, t.TempDir(), os.Getenv("PATH"))
}

func testMirror(t *testing.T, store objectstore.Store) (*Mirror, func()) {
	t.Helper()
	reposRoot := t.TempDir()
	workDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := New(store, gitRunner(t), reposRoot, "repos", workDir, func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	}, log)
	return m, func() {}
}

func TestBundleKeyFormat(t *testing.T) {
	m, _ := testMirror(t, objectstore.NewMemoryStore())
	got := m.bundleKey("r", time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC))
	// repos/<repo>/<RFC3339 with '-' in place of ':'>, nanoseconds>.bundle (R11-Q3)
	want := "repos/r/2026-01-02T03-04-05.123456789Z.bundle"
	if got != want {
		t.Errorf("bundleKey = %q, want %q", got, want)
	}
}

func TestZeroRefSkip(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	// An empty bare repo exists at reposRoot/r.git.
	repoDir := filepath.Join(m.reposRoot, "r.git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.git.Run(context.Background(), "init", "--bare", repoDir); err != nil {
		t.Fatal(err)
	}

	result, err := m.CreateBundle(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if result.Uploaded || result.Reason != "no refs" {
		t.Errorf("zero-ref result = %+v, want {false no refs} (R9-Q1)", result)
	}
	keys, err := m.List(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("zero-ref repo has bundles: %v", keys)
	}
}

// makeRepo builds a bare sha1 repo (the service default) with one commit and
// returns its dir.
func makeRepo(t *testing.T) string {
	return makeRepoFormat(t, "sha1")
}

// makeRepoFormat builds a bare repo in the given object format with one
// commit and returns its dir. sha1 is the service default; sha256 exercises
// the dual-format acceptance path.
func makeRepoFormat(t *testing.T, format string) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(work, "init", "-q", "-b", "main", "--object-format="+format, ".")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a")
	run(work, "commit", "-qm", "first")

	bare := filepath.Join(root, "r.git")
	run(root, "clone", "-q", "--bare", work, bare)
	return bare
}

// repoObjectFormat returns the bare repo's object format via git.
func repoObjectFormat(t *testing.T, repoDir string) string {
	t.Helper()
	out, err := gitRunner(t).Run(context.Background(), "--git-dir="+repoDir, "rev-parse", "--show-object-format")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCreateBundleAndFetch(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	bare := makeRepo(t)
	if err := os.Rename(bare, filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}

	result, err := m.CreateBundle(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Uploaded {
		t.Fatalf("CreateBundle = %+v", result)
	}
	keys, err := m.List(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "repos/r/2026-01-02T03-04-05.123456789Z.bundle" {
		t.Fatalf("List = %v", keys)
	}

	// Fetch restores into a fresh bare repo (R11-Q5).
	dest := filepath.Join(t.TempDir(), "restored.git")
	if err := m.Fetch(context.Background(), "r", dest); err != nil {
		t.Fatal(err)
	}
	head, err := m.git.RunIn(context.Background(), dest, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("restored rev-parse HEAD: %v", err)
	}
	want, err := m.git.RunIn(context.Background(), filepath.Join(m.reposRoot, "r.git"), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != string(want) {
		t.Errorf("restored HEAD %q != source HEAD %q", head, want)
	}

	// Fetch into an existing dest fails fast (R11-Q5).
	if err := m.Fetch(context.Background(), "r", dest); err == nil {
		t.Error("Fetch into existing dest = nil error")
	}
}

func TestFetchRestoresBothObjectFormats(t *testing.T) {
	// The service accepts SHA-1 and SHA-256 bundles; the restored repo must
	// match the bundle's own format (sha1 is the new default, sha256 the
	// dual-format acceptance case).
	for _, tc := range []struct {
		format string
		repo   string
	}{
		{format: "sha1", repo: "r1"},
		{format: "sha256", repo: "r2"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			store := objectstore.NewMemoryStore()
			m, _ := testMirror(t, store)
			bare := makeRepoFormat(t, tc.format)
			if err := os.Rename(bare, filepath.Join(m.reposRoot, tc.repo+".git")); err != nil {
				t.Fatal(err)
			}
			if _, err := m.CreateBundle(context.Background(), tc.repo); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(t.TempDir(), "restored.git")
			if err := m.Fetch(context.Background(), tc.repo, dest); err != nil {
				t.Fatalf("Fetch(%s) = %v", tc.format, err)
			}
			if got := repoObjectFormat(t, dest); got != tc.format {
				t.Errorf("restored repo format = %q, want %q", got, tc.format)
			}
			head, err := m.git.RunIn(context.Background(), dest, "rev-parse", "HEAD")
			if err != nil {
				t.Fatalf("restored rev-parse HEAD: %v", err)
			}
			want, err := m.git.RunIn(context.Background(), filepath.Join(m.reposRoot, tc.repo+".git"), "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if string(head) != string(want) {
				t.Errorf("restored HEAD %q != source HEAD %q", head, want)
			}
		})
	}
}

func TestFetchGarbageHeaderFailsClosed(t *testing.T) {
	// A bundle whose header is unrecognized fails before git init: no dest is
	// ever created (fail closed on corrupt input).
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	if err := store.Put(context.Background(), "repos/r/2026-01-02T03-04-05.123456789Z.bundle", []byte("not a bundle")); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "restored.git")
	err := m.Fetch(context.Background(), "r", dest)
	if err == nil || !strings.Contains(err.Error(), "unrecognized") {
		t.Fatalf("Fetch(garbage header) = %v, want unrecognized-header error", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest %s exists after garbage-header failure (stat err = %v)", dest, statErr)
	}
}

func TestDelete(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m, _ := testMirror(t, store)
	bare := makeRepo(t)
	if err := os.Rename(bare, filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBundle(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	keys, err := m.List(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("Delete left bundles: %v", keys)
	}
}
