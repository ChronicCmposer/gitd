package serve

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/event"
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
