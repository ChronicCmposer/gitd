package serve

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
)

func TestSubmitExecutesAction(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	srv.startWorker() // Submit queues; the worker executes (R9-Q3)
	ran := make(chan struct{}, 1)
	if err := srv.Submit(func(*Serve) { ran <- struct{}{} }); err != nil {
		t.Fatalf("Submit = %v", err)
	}
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit action never ran")
	}
}

func TestSubmitBusy(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	restore := TestSetSubmitWait(20 * time.Millisecond)
	defer restore()

	// Fill the channel (default buffer 64, R9-Q11) so the next submit bails
	// with ErrBusy (R12-Q1).
	block := make(chan struct{})
	for i := 0; i < 64; i++ {
		srv.actions <- func(*Serve) { <-block }
	}
	close(block)
	if err := srv.Submit(func(*Serve) {}); err == nil {
		t.Fatal("Submit = nil error on a full channel, want ErrBusy (R12-Q1)")
	}
}

func TestSubmitShuttingDown(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	restore := TestSetSubmitWait(20 * time.Millisecond)
	defer restore()
	// Close done and fill the channel: submit must prefer ErrShuttingDown
	// over ErrBusy.
	for i := 0; i < 64; i++ {
		srv.actions <- func(*Serve) {}
	}
	close(srv.done)
	if err := srv.Submit(func(*Serve) {}); err == nil {
		t.Fatal("Submit after shutdown = nil error, want ErrShuttingDown")
	}
}

func TestSocketHandlerMux(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	h := srv.SocketHandler()
	if h == nil {
		t.Fatal("SocketHandler() = nil")
	}
	// Unknown path returns 404 from the mux.
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path status = %d, want 404", rec.Code)
	}
}

func TestVerifyOnceRuns(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	makeBareRepo(t, srv.reposRoot, "r")
	// Runs inside the worker; the test drives it directly (it is a plain
	// method on Serve). No error is expected: mirror.Verify logs failures.
	srv.verifyOnce()
}

func TestVerifyOnceNoRepos(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	srv.verifyOnce() // empty reposRoot must not panic
}

func TestSweepOnce(t *testing.T) {
	srv, _, store := testServe(t, nil)
	// A pending event exists: sweep purges delivered+expired and re-queues
	// due pending events through the channel. With no due events it is a
	// no-op that must not panic (R6-Q1).
	ev := &event.Event{SchemaVersion: 1, EventID: "ev1", CreatedAt: time.Now().UTC().Format(time.RFC3339), Repo: "r", Ref: "refs/heads/main", Type: "push"}
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	srv.sweepOnce()
}

func TestStartSweepAndVerifyLoops(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	srv.startSweepLoop(ctx)
	srv.startVerifyLoop(ctx)
	cancel()
}

func TestUnlinkStaleSocket(t *testing.T) {
	dir := t.TempDir()

	// No socket: nil.
	srv, _, _ := testServe(t, nil)
	srv.socketPath = filepath.Join(dir, "missing.sock")
	if err := srv.unlinkStaleSocket(); err != nil {
		t.Fatalf("unlinkStaleSocket(missing) = %v", err)
	}

	// Not a socket: refuse (R12-Q4).
	plain := filepath.Join(dir, "plainfile")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.socketPath = plain
	if err := srv.unlinkStaleSocket(); err == nil {
		t.Fatal("unlinkStaleSocket(regular file) = nil, want refusal")
	}

	// Socket owned by someone else: refuse.
	sock := filepath.Join(dir, "other.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Chown(sock, os.Getuid()+1, os.Getgid()); err != nil {
		t.Skipf("cannot chown in this sandbox: %v", err)
	}
	srv.socketPath = sock
	if err := srv.unlinkStaleSocket(); err == nil {
		t.Fatal("unlinkStaleSocket(foreign socket) = nil, want refusal")
	}
}

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, map[string]bool{"ok": true})
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestDecodeStrictRejectsUnknownField(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"repo":"r","bogus":1}`))
	rec := httptest.NewRecorder()
	var dst struct {
		Repo string `json:"repo"`
	}
	err := decodeStrict(rec, req, &dst)
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("decodeStrict err = %v, want unknown-field error (R8-Q10)", err)
	}
}

func TestUnlinkStaleSocketStatError(t *testing.T) {
	// A socket path whose parent directory is missing: Lstat returns ENOENT,
	// which unlinkStaleSocket treats as "no stale socket" (nil), not an error.
	srv, _, _ := testServe(t, nil)
	srv.socketPath = filepath.Join(t.TempDir(), "sub", "missing.sock")
	if err := srv.unlinkStaleSocket(); err != nil {
		t.Fatalf("unlinkStaleSocket(ENOENT) = %v, want nil", err)
	}
}

// captureServeLog points srv's logger at a buffer so tests can assert the
// restore-on-start summary/warning lines.
func captureServeLog(srv *Serve) *bytes.Buffer {
	var buf bytes.Buffer
	srv.log = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

// failListStore breaks the objectstore's List to simulate an unreachable S3
// at startup; the embedded store still satisfies the rest of the seam.
type failListStore struct {
	objectstore.Store
}

func (failListStore) List(_ context.Context, _ string) ([]string, error) {
	return nil, errors.New("s3 unreachable")
}

func TestRestoreMissingOnStartStagesMissingRepos(t *testing.T) {
	// The diff must be additive/non-destructive: repos mirrored in S3 but
	// missing on disk are staged for the mirror-agent; local-only repos are
	// never touched.
	srv, _, _, store := testServeStore(t, nil)
	srv.restoreOnStart = true
	buf := captureServeLog(srv)

	// "a": mirrored in S3, missing on disk -> must be restored.
	makeBareRepo(t, srv.reposRoot, "a")
	if _, err := srv.mirror.CreateBundle(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(srv.reposRoot, "a.git")); err != nil {
		t.Fatal(err)
	}
	// "b": live on disk with no mirror -> local-only, must be left alone.
	makeBareRepo(t, srv.reposRoot, "b")
	// "c": mirrored in S3, missing on disk -> must be restored.
	makeBareRepo(t, srv.reposRoot, "c")
	if _, err := srv.mirror.CreateBundle(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(srv.reposRoot, "c.git")); err != nil {
		t.Fatal(err)
	}

	srv.restoreMissingOnStart()

	// One startup summary line with the diff count.
	if !strings.Contains(buf.String(), "restoring missing repo(s)") {
		t.Errorf("summary log line missing: %q", buf.String())
	}

	// The mirror-agent picks the staged jobs up and restores a and c.
	stopAgent := testRestoreAgent(t, srv, store)
	defer stopAgent()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, errA := os.Stat(filepath.Join(srv.reposRoot, "a.git"))
		_, errC := os.Stat(filepath.Join(srv.reposRoot, "c.git"))
		if errA == nil && errC == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "a.git")); err != nil {
		t.Errorf("mirrored-and-missing repo a not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "c.git")); err != nil {
		t.Errorf("mirrored-and-missing repo c not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "b.git")); err != nil {
		t.Errorf("local-only repo b was disturbed: %v", err)
	}
}

func TestRestoreMissingOnStartDisabled(t *testing.T) {
	// restore_on_start=false must make the startup pass a no-op.
	srv, _, _, _ := testServeStore(t, nil)
	srv.restoreOnStart = false

	makeBareRepo(t, srv.reposRoot, "a")
	if _, err := srv.mirror.CreateBundle(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(srv.reposRoot, "a.git")); err != nil {
		t.Fatal(err)
	}

	srv.restoreMissingOnStart()

	entries, err := os.ReadDir(srv.restoreDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		entries = nil
	}
	if len(entries) != 0 {
		t.Errorf("restore-on-start disabled staged %d job(s): %v", len(entries), entries)
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "a.git")); err == nil {
		t.Error("repo a restored despite restore_on_start=false")
	}
}

func TestRestoreMissingOnStartS3DownSkips(t *testing.T) {
	// An unreachable objectstore must warn and skip, never block or crash the
	// startup pass, and never disturb local repos.
	srv, _, _, _ := testServeStore(t, nil)
	srv.restoreOnStart = true
	buf := captureServeLog(srv)
	srv.mirror = mirror.New(failListStore{Store: objectstore.NewMemoryStore()}, testGit(t), srv.reposRoot, "repos", t.TempDir(), time.Now, srv.log)

	makeBareRepo(t, srv.reposRoot, "a")

	srv.restoreMissingOnStart()

	if !strings.Contains(buf.String(), "restore-on-start skipped") {
		t.Errorf("expected skip warning, got: %q", buf.String())
	}
	if strings.Contains(buf.String(), "restoring missing repo(s)") {
		t.Errorf("summary logged despite S3-down skip: %q", buf.String())
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "a.git")); err != nil {
		t.Errorf("local repo disturbed on S3-down skip: %v", err)
	}
}

func TestRestoreMissingOnStartNoMissingSilent(t *testing.T) {
	// Every S3-mirrored repo is already live on disk: nothing to restore, so
	// no summary line and no staged jobs.
	srv, _, _, _ := testServeStore(t, nil)
	srv.restoreOnStart = true
	buf := captureServeLog(srv)

	makeBareRepo(t, srv.reposRoot, "a")
	if _, err := srv.mirror.CreateBundle(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}

	srv.restoreMissingOnStart()

	if strings.Contains(buf.String(), "restoring missing repo(s)") {
		t.Errorf("summary logged with nothing missing: %q", buf.String())
	}
	entries, err := os.ReadDir(srv.restoreDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		entries = nil
	}
	if len(entries) != 0 {
		t.Errorf("no-missing pass staged %d job(s): %v", len(entries), entries)
	}
}

func TestRestoreMissingOnStartCorruptBundleLogged(t *testing.T) {
	// A repo whose mirror is corrupt fails staging per-repo (no retry, no
	// hot-loop): the failure is logged, and healthy repos still restore.
	srv, _, _, store := testServeStore(t, nil)
	srv.restoreOnStart = true
	buf := captureServeLog(srv)

	// "good": a healthy mirrored repo missing on disk -> restored.
	makeBareRepo(t, srv.reposRoot, "good")
	if _, err := srv.mirror.CreateBundle(context.Background(), "good"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(srv.reposRoot, "good.git")); err != nil {
		t.Fatal(err)
	}
	// "bad": a corrupt bundle in S3 (fails bundle verify during staging).
	if err := store.Put(context.Background(), "repos/bad/2026-01-01T00-00-00.000000000Z.bundle", []byte("not a git bundle")); err != nil {
		t.Fatal(err)
	}

	srv.restoreMissingOnStart()

	if !strings.Contains(buf.String(), "restore-on-start staging failed") {
		t.Errorf("expected per-repo staging failure log, got: %q", buf.String())
	}

	// The healthy repo still flows through the agent.
	stopAgent := testRestoreAgent(t, srv, store)
	defer stopAgent()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(srv.reposRoot, "good.git")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "good.git")); err != nil {
		t.Errorf("healthy repo not restored alongside corrupt mirror: %v", err)
	}
}
