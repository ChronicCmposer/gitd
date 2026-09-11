// Package mirror creates, lists, deletes, verifies, and restores git bundle
// mirrors (git bundle --all -> objectstore, R6-Q7/R8-Q2). Bundle keys are
// "<prefix>/<repo>/<RFC3339 with '-' in place of ':'>, nanosecond
// precision>.bundle" (R11-Q3); the timestamp is generated inside the
// serialized serve action at execution time, so the last-executed bundle is
// always the newest repo state (R10-Q3).
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/ChronicCmposer/gitd/internal/repo"
)

// BundleResult reports the outcome of a bundle create/upload (R11-Q3):
// uploaded=false with reason "no refs" when the repo has zero refs (R9-Q1).
type BundleResult struct {
	Uploaded bool
	Reason   string
}

// Mirror bundles repos from reposRoot and stores them via the objectstore
// seam. workDir is a writable directory for bundle temp files (the image has
// no /tmp; bundle temp writes live under /var/spool/gitd, R5-Q4/R8-Q3).
type Mirror struct {
	store     objectstore.Store
	git       *gitenv.Runner
	reposRoot string
	prefix    string
	workDir   string
	now       func() time.Time
	log       *slog.Logger
}

// New returns a Mirror. reposRoot is where bare repos live (/srv/git); prefix
// is the storage prefix ("repos", R6-Q7).
func New(store objectstore.Store, git *gitenv.Runner, reposRoot, prefix, workDir string, now func() time.Time, log *slog.Logger) *Mirror {
	return &Mirror{store: store, git: git, reposRoot: reposRoot, prefix: prefix, workDir: workDir, now: now, log: log}
}

// CreateBundle bundles repo (git bundle create --all) and uploads it. It
// returns uploaded=false, reason "no refs" for zero-ref repos (R9-Q1) — the
// repo is listed but has no bundles. The bundle timestamp is generated here,
// at execution time (R10-Q3).
func (m *Mirror) CreateBundle(ctx context.Context, repoName string) (BundleResult, error) {
	if !repo.ValidName(repoName) {
		return BundleResult{}, fmt.Errorf("mirror create: invalid repo name %q", repoName)
	}
	repoDir := filepath.Join(m.reposRoot, repoName+".git")

	refs, err := m.git.RunIn(ctx, repoDir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return BundleResult{}, fmt.Errorf("mirror create %s: enumerate refs: %w", repoName, err)
	}
	if len(strings.TrimSpace(string(refs))) == 0 {
		m.log.Info("no refs, bundle skipped", "repo", repoName)
		return BundleResult{Uploaded: false, Reason: "no refs"}, nil
	}

	tmp, err := os.CreateTemp(m.workDir, "bundle-*.bundle")
	if err != nil {
		return BundleResult{}, fmt.Errorf("mirror create %s: temp: %w", repoName, err)
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := m.git.RunIn(ctx, repoDir, "bundle", "create", tmpName, "--all"); err != nil {
		return BundleResult{}, fmt.Errorf("mirror create %s: bundle: %w", repoName, err)
	}
	data, err := os.ReadFile(tmpName)
	if err != nil {
		return BundleResult{}, fmt.Errorf("mirror create %s: read bundle: %w", repoName, err)
	}

	key := m.bundleKey(repoName, m.now())
	if err := m.store.Put(ctx, key, data); err != nil {
		return BundleResult{}, fmt.Errorf("mirror create %s: upload: %w", repoName, err)
	}
	m.log.Info("bundle uploaded", "repo", repoName, "key", key, "bytes", len(data))
	return BundleResult{Uploaded: true}, nil
}

// List returns the bundle keys for repo, sorted ascending.
func (m *Mirror) List(ctx context.Context, repoName string) ([]string, error) {
	if !repo.ValidName(repoName) {
		return nil, fmt.Errorf("mirror list: invalid repo name %q", repoName)
	}
	keys, err := m.store.List(ctx, m.prefix+"/"+repoName+"/")
	if err != nil {
		return nil, fmt.Errorf("mirror list %s: %w", repoName, err)
	}
	return keys, nil
}

// ListAllRepos returns every repo that has bundles mapped to its bundle keys
// (sorted ascending, per the store.List contract), in ONE store.List call so
// listing all mirrors is not N+1. A repo with no bundles (zero-ref, R9-Q1)
// has no keys and is therefore absent. Bundle keys are
// "<prefix>/<repo>/<ts>.bundle" (R11-Q3); a key that does not parse into a
// valid repo name fails loudly (fail-closed) so corruption surfaces instead
// of being silently dropped.
func (m *Mirror) ListAllRepos(ctx context.Context) (map[string][]string, error) {
	keys, err := m.store.List(ctx, m.prefix+"/")
	if err != nil {
		return nil, fmt.Errorf("mirror list-all: %w", err)
	}
	repos := make(map[string][]string)
	for _, key := range keys {
		repoName, ok := m.repoNameFromKey(key)
		if !ok {
			return nil, fmt.Errorf("mirror list-all: malformed key %q", key)
		}
		repos[repoName] = append(repos[repoName], key)
	}
	return repos, nil
}

// repoNameFromKey extracts the repo name from a bundle key
// "<prefix>/<repo>/<ts>.bundle" (R11-Q3): the segment after the prefix slash
// up to the next slash. It reports false for anything that is not a valid
// bundle key under the prefix (missing repo segment, empty repo name, or a
// name outside the repo allowlist).
func (m *Mirror) repoNameFromKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, m.prefix+"/")
	if !ok {
		return "", false
	}
	repoName, _, ok := strings.Cut(rest, "/")
	if !ok || repoName == "" {
		return "", false
	}
	if !repo.ValidName(repoName) {
		return "", false
	}
	return repoName, true
}

// Delete removes every bundle for repo (R6-Q9; noncurrent versions expire via
// the 30d lifecycle).
func (m *Mirror) Delete(ctx context.Context, repoName string) error {
	keys, err := m.List(ctx, repoName)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := m.store.Delete(ctx, key); err != nil {
			return fmt.Errorf("mirror delete %s: %w", repoName, err)
		}
		m.log.Info("bundle deleted", "repo", repoName, "key", key)
	}
	return nil
}

// Fetch restores repo into dest from its latest bundle (R11-Q5): dest must
// not exist, then explicit git init --bare --object-format=sha256, bundle
// verify, unbundle (refs updated from the unbundle listing), audit-log. No
// git fsck in v1 — bundle verify is the fsck --full equivalent for the
// bundle (R11-Q5).
//
// Partial-failure cleanup: once this call has created dest (git init
// succeeded), any later failure removes dest again so a failed restore never
// leaves a half-initialized repo behind. A pre-existing dest fails fast above
// and is never touched.
func (m *Mirror) Fetch(ctx context.Context, repoName, dest string) error {
	if !repo.ValidName(repoName) {
		return fmt.Errorf("mirror fetch: invalid repo name %q", repoName)
	}
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("mirror fetch: destination %s already exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mirror fetch: mkdir parent: %w", err)
	}

	if _, err := m.git.Run(ctx, "init", "--bare", "--object-format=sha256", dest); err != nil {
		return fmt.Errorf("mirror fetch %s: init: %w", repoName, err)
	}
	// Fetch created dest; any step that fails after init removes it. Success
	// clears the flag just before returning, keeping the restored repo.
	removeCreatedDest := true
	defer func() {
		if removeCreatedDest {
			_ = os.RemoveAll(dest)
		}
	}()

	keys, err := m.List(ctx, repoName)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("mirror fetch %s: no bundles", repoName)
	}
	latest := keys[len(keys)-1]

	bundlePath, cleanup, err := m.download(ctx, latest)
	if err != nil {
		return err
	}
	defer cleanup()

	// bundle verify needs a repository context; dest is a fresh bare repo.
	if _, err := m.git.RunIn(ctx, dest, "bundle", "verify", bundlePath); err != nil {
		return fmt.Errorf("mirror fetch %s: verify %s: %w", repoName, latest, err)
	}
	if err := m.unbundle(ctx, dest, bundlePath); err != nil {
		return fmt.Errorf("mirror fetch %s: unbundle %s: %w", repoName, latest, err)
	}
	removeCreatedDest = false
	m.log.Info("repo restored from bundle", "repo", repoName, "key", latest, "dest", dest)
	return nil
}

// Restore restores repo into its canonical bare path under reposRoot
// (reposRoot/<repo>.git) using Fetch's default destination (R8-Q2/R11-Q5).
// It is the no-dest form of Fetch: dest fails fast if reposRoot/<repo>.git
// already exists, same as Fetch.
func (m *Mirror) Restore(ctx context.Context, repoName string) error {
	return m.Fetch(ctx, repoName, filepath.Join(m.reposRoot, repoName+".git"))
}

// unbundle unpacks the bundle into a fresh bare repo and updates the refs it
// lists (git bundle unbundle only prints the refs; the caller applies them).
// HEAD is skipped so the repo keeps its symbolic HEAD from git init; if that
// HEAD is dangling (init's default branch differs from the bundle's, e.g.
// master vs main), it is repointed at the first restored branch so the
// restored repo always has a resolvable HEAD (R11-Q5).
func (m *Mirror) unbundle(ctx context.Context, dest, bundlePath string) error {
	out, err := m.git.RunIn(ctx, dest, "bundle", "unbundle", bundlePath)
	if err != nil {
		return err
	}
	firstBranch := ""
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.HasPrefix(f[1], "refs/") {
			continue
		}
		if _, err := m.git.RunIn(ctx, dest, "update-ref", f[1], f[0]); err != nil {
			return fmt.Errorf("update-ref %s: %w", f[1], err)
		}
		if firstBranch == "" && strings.HasPrefix(f[1], "refs/heads/") {
			firstBranch = f[1]
		}
	}
	if firstBranch == "" {
		return nil
	}
	if _, err := m.git.RunIn(ctx, dest, "rev-parse", "--verify", "HEAD"); err != nil {
		if _, err := m.git.RunIn(ctx, dest, "symbolic-ref", "HEAD", firstBranch); err != nil {
			return fmt.Errorf("repoint HEAD: %w", err)
		}
		m.log.Info("restored HEAD repointed", "branch", firstBranch)
	}
	return nil
}

// Verify checks the latest bundle for repo (R8-Q3 weekly verify): Get latest,
// git bundle verify (against the live repo, which is what "applies cleanly"
// means), audit-log the result. A repo with no bundles logs and succeeds
// (R9-Q1).
func (m *Mirror) Verify(ctx context.Context, repoName string) error {
	if !repo.ValidName(repoName) {
		return fmt.Errorf("mirror verify: invalid repo name %q", repoName)
	}
	repoDir := filepath.Join(m.reposRoot, repoName+".git")
	keys, err := m.List(ctx, repoName)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		m.log.Info("no bundles to verify", "repo", repoName)
		return nil
	}
	latest := keys[len(keys)-1]
	bundlePath, cleanup, err := m.download(ctx, latest)
	if err != nil {
		return err
	}
	defer cleanup()

	if _, err := m.git.RunIn(ctx, repoDir, "bundle", "verify", bundlePath); err != nil {
		m.log.Error("bundle verify failed", "repo", repoName, "key", latest, "error", err)
		return fmt.Errorf("mirror verify %s: %w", repoName, err)
	}
	m.log.Info("bundle verified", "repo", repoName, "key", latest)
	return nil
}

// download fetches a bundle into a temp file under workDir.
func (m *Mirror) download(ctx context.Context, key string) (string, func(), error) {
	data, err := m.store.Get(ctx, key)
	if err != nil {
		return "", nil, fmt.Errorf("mirror download %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(m.workDir, "verify-*.bundle")
	if err != nil {
		return "", nil, fmt.Errorf("mirror download %s: temp: %w", key, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("mirror download %s: write: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("mirror download %s: close: %w", key, err)
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// bundleKey builds "repos/<repo>/<RFC3339 with '-' for ':'>, nanoseconds>.bundle"
// (R11-Q3).
func (m *Mirror) bundleKey(repoName string, t time.Time) string {
	ts := t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
	ts = strings.ReplaceAll(ts, ":", "-")
	return m.prefix + "/" + repoName + "/" + ts + ".bundle"
}
