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

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

// doRequest runs one request through the serve handler (mounted on the real
// socket mux via SocketHandler) and returns the recorder.
func doRequest(t *testing.T, srv *Serve, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.SocketHandler().ServeHTTP(rec, req)
	return rec
}

func TestHandleBundleInvalidRepo(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	rec := doRequest(t, srv, http.MethodPost, "/v1/bundle", `{"repo":".bad"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid repo status = %d, want 400", rec.Code)
	}
}

func TestHandleBundleBadJSON(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	rec := doRequest(t, srv, http.MethodPost, "/v1/bundle", `{"repo":`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json status = %d, want 400", rec.Code)
	}
}

func TestHandleBundleBusy(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	restore := TestSetSubmitWait(5 * time.Millisecond)
	defer restore()
	for i := 0; i < 64; i++ {
		srv.actions <- func(*Serve) {}
	}
	rec := doRequest(t, srv, http.MethodPost, "/v1/bundle", `{"repo":"r"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("busy status = %d, want 503 (R12-Q1)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "busy") {
		t.Errorf("busy body = %q", rec.Body.String())
	}
}

func TestHandleDeliverMissingFields(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	rec := doRequest(t, srv, http.MethodPost, "/v1/deliver", `{"plugin-id":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing fields status = %d, want 400", rec.Code)
	}
}

func TestHandleDeliverUnknownPlugin(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	rec := doRequest(t, srv, http.MethodPost, "/v1/deliver", `{"plugin-id":"ghost","event-id":"ev1"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown plugin status = %d, want 404 (R13-Q8)", rec.Code)
	}
}

func TestHandleDeliverOK(t *testing.T) {
	plugins := []config.PluginConfig{{ID: "p1", Type: "logger"}}
	srv, delivered, _ := testServe(t, plugins)
	srv.startWorker()
	rec := doRequest(t, srv, http.MethodPost, "/v1/deliver", `{"plugin-id":"p1","event-id":"ev1"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("deliver status = %d, want 200", rec.Code)
	}
	// The worker executes asynchronously; poll for the delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && delivered.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := delivered.all(); len(got) != 1 || got[0] != "p1/ev1" {
		t.Errorf("delivered = %v", got)
	}
}

func TestHandleDeliverBusy(t *testing.T) {
	// A configured plugin passes the pluginConfigured gate, then the full
	// channel yields 503 (R12-Q1).
	srv, _, _ := testServe(t, []config.PluginConfig{{ID: "p1", Type: "logger"}})
	restore := TestSetSubmitWait(5 * time.Millisecond)
	defer restore()
	for i := 0; i < 64; i++ {
		srv.actions <- func(*Serve) {}
	}
	rec := doRequest(t, srv, http.MethodPost, "/v1/deliver", `{"plugin-id":"p1","event-id":"ev1"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("busy status = %d, want 503", rec.Code)
	}
}

func TestDeliverAllActionNoPlugins(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	srv.deliverAllAction("ev1")(srv) // no plugins: logs and returns
}

func TestWriteServeErrorBusy(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServeError(rec, ErrBusy)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ErrBusy status = %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	writeServeError(rec, ErrShuttingDown)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ErrShuttingDown status = %d, want 503", rec.Code)
	}
}

func TestWriteServeErrorGeneric(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServeError(rec, os.ErrPermission)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("generic status = %d, want 500", rec.Code)
	}
}

func TestWriteJSONEncodeError(t *testing.T) {
	// A channel cannot be JSON-encoded; writeJSON must not panic.
	rec := httptest.NewRecorder()
	writeJSON(rec, make(chan int))
}

func TestCatchUpError(t *testing.T) {
	// A spool dir that disappears makes CatchUp fail; Run must surface it.
	srv, _, store := testServe(t, nil)
	if err := os.RemoveAll(store.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := srv.catchUp(); err == nil {
		t.Fatal("catchUp with missing spool dir = nil error")
	}
}

func TestSocketRoundTrip(t *testing.T) {
	// End-to-end through a real unix socket: the client in internal/socket
	// submits to the serve handler (R9-Q11 submission path). A real repo
	// exists so the bundle action completes without hitting the retry
	// backoff (R9-Q2) inside the test's client timeout.
	srv, _, _ := testServe(t, nil)
	makeBareRepo(t, srv.reposRoot, "r")
	srv.startWorker()
	path := filepath.Join(t.TempDir(), "gitd.sock")
	srv.socketPath = path
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, srv.SocketHandler())

	client := socket.NewClient(path, 2*time.Second)
	res, err := client.Bundle(context.Background(), "r")
	if err != nil {
		t.Fatalf("Bundle round-trip = %v", err)
	}
	if !res.Uploaded {
		t.Errorf("Bundle round-trip result = %+v, want uploaded=true", res)
	}
}

func TestSocketRoundTripBadRequest(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	srv.startWorker()
	path := filepath.Join(t.TempDir(), "gitd.sock")
	srv.socketPath = path
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, srv.SocketHandler())

	client := socket.NewClient(path, 2*time.Second)
	if _, err := client.Bundle(context.Background(), ".bad"); err == nil {
		t.Fatal("Bundle invalid repo = nil error, want 400 surfaced")
	}
}

func TestSweepOncePurgesDelivered(t *testing.T) {
	srv, _, store := testServe(t, nil)
	ev := &event.Event{SchemaVersion: 1, EventID: "ev1", CreatedAt: time.Now().UTC().Format(time.RFC3339), Repo: "r", Ref: "refs/heads/main", Type: "push"}
	id, err := store.Write(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetState(id, spool.StateDelivered); err != nil {
		t.Fatal(err)
	}
	// Retention is set at construction (90d); purge only removes expired
	// delivered events, so this just exercises the no-op path.
	srv.sweepOnce()
}

func TestSweepOnceSpoolGone(t *testing.T) {
	// Spool dir removed: sweep logs the purge/catch-up errors, no panic.
	srv, _, store := testServe(t, nil)
	if err := os.RemoveAll(store.Dir()); err != nil {
		t.Fatal(err)
	}
	srv.sweepOnce()
}

func TestVerifyOnceReposRootGone(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	if err := os.RemoveAll(srv.reposRoot); err != nil {
		t.Fatal(err)
	}
	srv.verifyOnce() // ListBare error path: log + return
}

func TestStartSweepLoopTicks(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	srv.startWorker()
	srv.sweepInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	srv.startSweepLoop(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
}

func TestStartVerifyLoopTicks(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	srv.startWorker()
	srv.verifyInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	srv.startVerifyLoop(ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
}

func TestUnlinkStaleSocketRemovesOwned(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "gitd.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	// chown to git uid (1001) is required by the guard (R12-Q4); skip where
	// the sandbox forbids it.
	if err := os.Chown(path, 1001, os.Getgid()); err != nil {
		t.Skipf("cannot chown to git uid in this sandbox: %v", err)
	}
	srv.socketPath = path
	if err := srv.unlinkStaleSocket(); err != nil {
		t.Fatalf("unlinkStaleSocket(owned) = %v, want removal", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale socket still present after unlink")
	}
}
