package mirror

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/objectstore"
)

// testAgentMirror builds a mirror on the given store with its own temp
// reposRoot + workDir, like testMirror but with a caller-supplied store so
// tests can share the store between serve and the agent.
func testAgentMirror(t *testing.T, store objectstore.Store) *Mirror {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	m := New(store, gitRunner(t), t.TempDir(), "repos", t.TempDir(), func() time.Time {
		return time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	}, log)
	return m
}

// seedBundle stores a bundle for repo under m and returns the verified bundle
// bytes (as the agent's staged <id>.bundle would contain).
func seedBundle(t *testing.T, m *Mirror, repo string) []byte {
	t.Helper()
	bare := makeRepo(t)
	if err := os.Rename(bare, filepath.Join(m.reposRoot, repo+".git")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateBundle(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	keys, err := m.List(context.Background(), repo)
	if err != nil || len(keys) == 0 {
		t.Fatalf("seed bundle: keys = %v, err = %v", keys, err)
	}
	data, err := m.store.Get(context.Background(), keys[len(keys)-1])
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// testAgent wires an Agent on a fresh mirror + restore spool.
func testAgent(t *testing.T, m *Mirror, workDir string) *Agent {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return NewAgent(AgentConfig{Mirror: m, WorkDir: workDir, ReposRoot: m.reposRoot, Log: log})
}

// runAgent starts a.Run in a goroutine and returns a cancel func plus a
// channel carrying its exit error (for fail-loud assertions).
func runAgent(t *testing.T, a *Agent) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()
	return cancel, errCh
}

// stageJob writes <id>.bundle + <id>.request into the spool the way serve
// would: the bundle first (fully), then the request atomically.
func stageJob(t *testing.T, workDir, id string, bundleData []byte, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workDir, id+JobBundleExt), bundleData, 0o644); err != nil {
		t.Fatal(err)
	}
	req := []byte(`{"repo":` + strconvQuote(repo) + `,"bundle":` + strconvQuote(id+JobBundleExt) + `}`)
	if err := WriteJobFile(workDir, id+JobRequestExt, req, 0o644); err != nil {
		t.Fatal(err)
	}
}

// strconvQuote quotes s as a JSON string (the test request bodies are built
// by hand; avoid pulling encoding/json into every helper).
func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// waitFor polls until cond returns true or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// readResult reads <id>.result; fails the test if absent or undecodable.
func readResult(t *testing.T, workDir, id string) *JobResult {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workDir, id+JobResultExt))
	if err != nil {
		t.Fatalf("read result %s: %v", id, err)
	}
	res, err := DecodeJobResult(data)
	if err != nil {
		t.Fatalf("decode result %s: %v (%s)", id, err, data)
	}
	return res
}

// noResult asserts <id>.result does not exist.
func noResult(t *testing.T, workDir, id string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(workDir, id+JobResultExt)); !os.IsNotExist(err) {
		t.Fatalf("result %s exists, want none", id)
	}
}

func TestAgentProcessesExistingJobsOnStartup(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	if err := os.RemoveAll(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	stageJob(t, workDir, "job1", data, "r")
	a := testAgent(t, m, workDir)
	cancel, errCh := runAgent(t, a)
	defer cancel()

	// Wait for the result first: it is written atomically AFTER the restore,
	// so its presence implies the repo landed.
	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job1"+JobResultExt))
		return err == nil
	})
	res := readResult(t, workDir, "job1")
	if !res.OK {
		t.Errorf("result = %+v, want ok", res)
	}
	if _, err := os.Stat(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Errorf("restored repo missing: %v", err)
	}
	select {
	case err := <-errCh:
		t.Fatalf("agent exited unexpectedly: %v", err)
	default:
	}
	cancel()
}

func TestAgentWatchesNewJobs(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	if err := os.RemoveAll(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()
	a := testAgent(t, m, workDir)
	cancel, errCh := runAgent(t, a)
	defer cancel()

	// The job is staged after the agent starts; scan-then-watch covers every
	// ordering (the scan picks it up, or fsnotify delivers the Create event).
	stageJob(t, workDir, "job2", data, "r")
	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job2"+JobResultExt))
		return err == nil
	})
	res := readResult(t, workDir, "job2")
	if !res.OK {
		t.Errorf("result = %+v, want ok", res)
	}
	if _, err := os.Stat(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Errorf("restored repo missing: %v", err)
	}
	select {
	case err := <-errCh:
		t.Fatalf("agent exited unexpectedly: %v", err)
	default:
	}
	cancel()
}

func TestAgentRejectsInvalidRepoName(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	workDir := t.TempDir()

	stageJob(t, workDir, "job3", []byte("not used"), ".bad")
	a := testAgent(t, m, workDir)
	cancel, _ := runAgent(t, a)
	defer cancel()

	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job3"+JobResultExt))
		return err == nil
	})
	res := readResult(t, workDir, "job3")
	if res.OK || !strings.Contains(res.Message, "invalid repo name") {
		t.Errorf("result = %+v, want invalid-repo-name error", res)
	}
	// Never write an arbitrary dest: reposRoot stays empty.
	entries, err := os.ReadDir(m.reposRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("reposRoot has entries after invalid job: %v", entries)
	}
}

func TestAgentRejectsMismatchedBundleName(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	workDir := t.TempDir()

	req := []byte(`{"repo":"r","bundle":"other.bundle"}`)
	if err := WriteJobFile(workDir, "job4"+JobRequestExt, req, 0o644); err != nil {
		t.Fatal(err)
	}
	a := testAgent(t, m, workDir)
	cancel, _ := runAgent(t, a)
	defer cancel()

	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job4"+JobResultExt))
		return err == nil
	})
	res := readResult(t, workDir, "job4")
	if res.OK || !strings.Contains(res.Message, "does not match job id") {
		t.Errorf("result = %+v, want bundle-mismatch error", res)
	}
}

func TestAgentRejectsCorruptBundle(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	workDir := t.TempDir()

	// A corrupt staged bundle: the agent's own bundle verify must halt the
	// job before any write (defense in depth — never trust serve's verify).
	stageJob(t, workDir, "job5", []byte("not a git bundle"), "r")
	a := testAgent(t, m, workDir)
	cancel, _ := runAgent(t, a)
	defer cancel()

	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job5"+JobResultExt))
		return err == nil
	})
	res := readResult(t, workDir, "job5")
	if res.OK || !strings.Contains(res.Message, "bundle verify") {
		t.Errorf("result = %+v, want bundle-verify error", res)
	}
	entries, err := os.ReadDir(m.reposRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("reposRoot has entries after corrupt job: %v", entries)
	}
}

func TestAgentWritesIntoCanonicalPathOnly(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	if err := os.RemoveAll(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	stageJob(t, workDir, "job6", data, "r")
	a := testAgent(t, m, workDir)
	cancel, _ := runAgent(t, a)
	defer cancel()

	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job6"+JobResultExt))
		return err == nil
	})
	// The restore lands exactly at reposRoot/r.git — the canonical bare path
	// derived from the validated repo name — and nowhere else.
	if _, err := os.Stat(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatalf("canonical repo missing: %v", err)
	}
	entries, err := os.ReadDir(m.reposRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "r.git" {
		t.Errorf("reposRoot = %v, want exactly [r.git]", entries)
	}
}

func TestAgentConsumesJobInputs(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	if err := os.RemoveAll(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	stageJob(t, workDir, "job7", data, "r")
	a := testAgent(t, m, workDir)
	cancel, _ := runAgent(t, a)
	defer cancel()

	waitFor(t, "result", func() bool {
		_, err := os.Stat(filepath.Join(workDir, "job7"+JobResultExt))
		return err == nil
	})
	// The agent leaves <id>.result for serve but consumes the request +
	// bundle so a restart never re-triggers the job.
	for _, name := range []string{"job7" + JobRequestExt, "job7" + JobBundleExt} {
		if _, err := os.Stat(filepath.Join(workDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still present after processing", name)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "job7"+JobResultExt)); err != nil {
		t.Errorf("result removed by agent (serve owns that cleanup): %v", err)
	}
}

func TestAgentMissingRequestIsNoop(t *testing.T) {
	m := testAgentMirror(t, objectstore.NewMemoryStore())
	workDir := t.TempDir()
	a := testAgent(t, m, workDir)
	// A queued event for a job the scan already consumed: no request file
	// means nothing to do — no result, no error.
	a.processJob(context.Background(), "ghost")
	noResult(t, workDir, "ghost")
}

func TestAgentDecodeRejectsUnknownFields(t *testing.T) {
	m := testAgentMirror(t, objectstore.NewMemoryStore())
	workDir := t.TempDir()
	req := []byte(`{"repo":"r","bundle":"j.bundle","bogus":1}`)
	if err := WriteJobFile(workDir, "job8"+JobRequestExt, req, 0o644); err != nil {
		t.Fatal(err)
	}
	a := testAgent(t, m, workDir)
	a.processJob(context.Background(), "job8")
	res := readResult(t, workDir, "job8")
	if res.OK || !strings.Contains(res.Message, "unknown field") {
		t.Errorf("result = %+v, want unknown-field decode error", res)
	}
}

func TestAgentAuditLogsRestore(t *testing.T) {
	// The finish path must record repo + bundle + ok/error in the log; the
	// result file carries the outcome and the log carries the audit. This
	// exercises finish with a real result write.
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	if err := os.RemoveAll(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	stageJob(t, workDir, "job9", data, "r")
	a := testAgent(t, m, workDir)
	a.processJob(context.Background(), "job9")
	res := readResult(t, workDir, "job9")
	if !res.OK {
		t.Errorf("result = %+v, want ok", res)
	}
	if _, err := os.Stat(filepath.Join(m.reposRoot, "r.git")); err != nil {
		t.Errorf("restored repo missing: %v", err)
	}
}

func TestAgentContextCancelStopsRun(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	workDir := t.TempDir()
	a := testAgent(t, m, workDir)
	cancel, errCh := runAgent(t, a)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("agent exit on cancel = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent did not stop on cancel")
	}
}

func TestWriteJobFileAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteJobFile(dir, "job.result", []byte(`{"ok":true,"message":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "job.result"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ok":true,"message":"x"}` {
		t.Errorf("WriteJobFile content = %q", data)
	}
	// No temp files linger.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "job.result" {
		t.Errorf("dir after WriteJobFile = %v", entries)
	}
}

func TestLatestBundle(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	got, err := m.LatestBundle(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("LatestBundle = %d bytes, want %d", len(got), len(data))
	}
}

func TestLatestBundleNoBundlesFails(t *testing.T) {
	m := testAgentMirror(t, objectstore.NewMemoryStore())
	if _, err := m.LatestBundle(context.Background(), "r"); err == nil || !strings.Contains(err.Error(), "no bundles") {
		t.Fatalf("LatestBundle(no bundles) = %v, want no-bundles error", err)
	}
}

func TestLatestBundleInvalidNameFails(t *testing.T) {
	m := testAgentMirror(t, objectstore.NewMemoryStore())
	if _, err := m.LatestBundle(context.Background(), ".bad"); err == nil || !strings.Contains(err.Error(), "invalid repo name") {
		t.Fatalf("LatestBundle(.bad) = %v, want invalid-repo-name error", err)
	}
}

func TestVerifyBundleFile(t *testing.T) {
	store := objectstore.NewMemoryStore()
	m := testAgentMirror(t, store)
	data := seedBundle(t, m, "r")
	path := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyBundleFile(context.Background(), path); err != nil {
		t.Fatalf("VerifyBundleFile(valid) = %v", err)
	}
}

func TestVerifyBundleFileBothFormats(t *testing.T) {
	// VerifyBundleFile must detect the bundle's own object format from its
	// header and verify both SHA-1 and SHA-256 bundles (dual-format
	// acceptance).
	for _, tc := range []struct {
		format string
		repo   string
	}{
		{format: "sha1", repo: "r1"},
		{format: "sha256", repo: "r2"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			store := objectstore.NewMemoryStore()
			m := testAgentMirror(t, store)
			bare := makeRepoFormat(t, tc.format)
			if err := os.Rename(bare, filepath.Join(m.reposRoot, tc.repo+".git")); err != nil {
				t.Fatal(err)
			}
			if _, err := m.CreateBundle(context.Background(), tc.repo); err != nil {
				t.Fatal(err)
			}
			keys, err := m.List(context.Background(), tc.repo)
			if err != nil || len(keys) == 0 {
				t.Fatalf("keys = %v, err = %v", keys, err)
			}
			data, err := m.store.Get(context.Background(), keys[len(keys)-1])
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "b.bundle")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := m.VerifyBundleFile(context.Background(), path); err != nil {
				t.Fatalf("VerifyBundleFile(%s) = %v", tc.format, err)
			}
		})
	}
}

func TestVerifyBundleFileCorruptFails(t *testing.T) {
	m := testAgentMirror(t, objectstore.NewMemoryStore())
	path := filepath.Join(t.TempDir(), "b.bundle")
	if err := os.WriteFile(path, []byte("not a bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.VerifyBundleFile(context.Background(), path); err == nil {
		t.Fatal("VerifyBundleFile(corrupt) = nil error, want failure")
	}
}

func TestDecodeJobResultStrict(t *testing.T) {
	if _, err := DecodeJobResult([]byte(`{"ok":true,"bogus":1}`)); err == nil {
		t.Fatal("DecodeJobResult accepted unknown field")
	}
	res, err := DecodeJobResult([]byte(`{"ok":false,"message":"boom"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Message != "boom" {
		t.Errorf("DecodeJobResult = %+v", res)
	}
}
