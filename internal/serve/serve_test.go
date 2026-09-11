package serve

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

const gitBin = "/usr/bin/git"

func testGit(t *testing.T) *gitenv.Runner {
	t.Helper()
	if _, err := exec.LookPath(gitBin); err != nil {
		t.Skip("git not available")
	}
	return gitenv.NewRunner(gitBin, t.TempDir(), os.Getenv("PATH"))
}

// deliveredLog records deliverer calls race-free (the worker writes from the
// actions-channel goroutine; tests poll from the test goroutine).
type deliveredLog struct {
	mu      sync.Mutex
	entries []string
}

func (d *deliveredLog) add(s string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries = append(d.entries, s)
}

func (d *deliveredLog) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

func (d *deliveredLog) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entries
}

// testServe wires a Serve with a MemoryStore-backed mirror, a recording
// deliverer, and the given plugin list.
func testServe(t *testing.T, plugins []config.PluginConfig) (*Serve, *deliveredLog, *spool.Store) {
	srv, delivered, store, _ := testServeStore(t, plugins)
	return srv, delivered, store
}

// testServeStore is testServe plus the objectstore backing the mirror, so
// restore tests can run a real mirror-agent against the same store.
func testServeStore(t *testing.T, plugins []config.PluginConfig) (*Serve, *deliveredLog, *spool.Store, *objectstore.MemoryStore) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	reposRoot := t.TempDir()
	workDir := t.TempDir()
	spoolDir := t.TempDir()
	sockPath := filepath.Join(t.TempDir(), "gitd.sock")

	store := spool.NewStore(spoolDir, time.Now, 90*24*time.Hour, log)
	objStore := objectstore.NewMemoryStore()
	m := mirror.New(objStore, testGit(t), reposRoot, "repos", workDir, time.Now, log)

	delivered := &deliveredLog{}
	deliver := func(_ context.Context, pluginID, eventID string) error {
		delivered.add(pluginID + "/" + eventID)
		return nil
	}
	wh := &config.WebhooksConfig{Plugins: plugins}
	srv := New(Config{
		Mirror:         m,
		Spool:          store,
		Webhooks:       func() *config.WebhooksConfig { return wh },
		Deliver:        deliver,
		ReposRoot:      reposRoot,
		RestoreDir:     filepath.Join(t.TempDir(), "restore"),
		SocketPath:     sockPath,
		Now:            time.Now,
		Log:            log,
		SweepInterval:  0,
		VerifyInterval: 0,
	})
	return srv, delivered, store, objStore
}

// testRestoreAgent runs a real mirror-agent on srv's restore spool against
// the same objectstore, and returns a stop func.
func testRestoreAgent(t *testing.T, srv *Serve, store *objectstore.MemoryStore) func() {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	am := mirror.New(store, testGit(t), srv.reposRoot, "repos", t.TempDir(), time.Now, log)
	agent := mirror.NewAgent(mirror.AgentConfig{Mirror: am, WorkDir: srv.restoreDir, ReposRoot: srv.reposRoot, Log: log})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = agent.Run(ctx) }()
	return cancel
}

// makeBareRepo creates a bare sha1 repo with one commit at reposRoot/name.git.
func makeBareRepo(t *testing.T, reposRoot, name string) {
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
	run(work, "init", "-q", "-b", "main", "--object-format=sha1", ".")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a")
	run(work, "commit", "-qm", "first")
	run(root, "clone", "-q", "--bare", work, filepath.Join(reposRoot, name+".git"))
}

// runServe starts srv.Run in a goroutine and cancels + waits on cleanup. It
// blocks until the socket is accepting so tests can connect immediately.
func runServe(t *testing.T, srv *Serve) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitSocket(t, srv.socketPath)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serve did not stop")
		}
	})
}

// waitSocket polls until the unix socket accepts connections.
func waitSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s never accepted connections", path)
}

func TestServeBundleUpload(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	makeBareRepo(t, srv.reposRoot, "r")
	runServe(t, srv)

	c := socket.NewClient(srv.socketPath, 5*time.Second)
	res, err := c.Bundle(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Uploaded || res.Reason != "" {
		t.Errorf("Bundle = %+v, want uploaded", res)
	}
}

func TestServeRestoreRoundTrip(t *testing.T) {
	// End-to-end over a real unix socket with the real mirror-agent: the
	// restore client submits to the serve handler, serve stages the job,
	// the agent re-verifies the staged bundle and writes /srv/git/r.git as
	// git, and serve replies 200 after reading the result. Seed a bundle,
	// drop the live repo, then restore it back.
	srv, _, _, store := testServeStore(t, nil)
	makeBareRepo(t, srv.reposRoot, "r")
	if _, err := srv.mirror.CreateBundle(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(srv.reposRoot, "r.git")); err != nil {
		t.Fatal(err)
	}
	runServe(t, srv)
	stopAgent := testRestoreAgent(t, srv, store)
	defer stopAgent()

	c := socket.NewClient(srv.socketPath, 30*time.Second)
	if err := c.Restore(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srv.reposRoot, "r.git")); err != nil {
		t.Errorf("restored repo missing: %v", err)
	}
	// Serve consumed the job files after reading the result.
	entries, err := os.ReadDir(srv.restoreDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("restore spool not cleaned after result: %v", entries)
	}
}

func TestServeZeroRefSkip(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	git := testGit(t)
	if _, err := git.Run(context.Background(), "init", "--bare", filepath.Join(srv.reposRoot, "z.git")); err != nil {
		t.Fatal(err)
	}
	runServe(t, srv)

	c := socket.NewClient(srv.socketPath, 5*time.Second)
	res, err := c.Bundle(context.Background(), "z")
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded || res.Reason != "no refs" {
		t.Errorf("zero-ref Bundle = %+v, want {false no refs} (R9-Q1)", res)
	}
}

func TestServeDeliver(t *testing.T) {
	plugins := []config.PluginConfig{{ID: "p1", Type: "http"}}
	srv, delivered, _ := testServe(t, plugins)
	runServe(t, srv)

	c := socket.NewClient(srv.socketPath, 5*time.Second)
	if err := c.Deliver(context.Background(), "p1", "ev1"); err != nil {
		t.Fatal(err)
	}
	if len(delivered.all()) != 1 || delivered.all()[0] != "p1/ev1" {
		t.Errorf("deliverer calls = %v", delivered.all())
	}
}

func TestServeDeliverUnknownPlugin404(t *testing.T) {
	srv, delivered, _ := testServe(t, nil)
	runServe(t, srv)

	c := socket.NewClient(srv.socketPath, 5*time.Second)
	err := c.Deliver(context.Background(), "ghost", "ev1")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("unknown plugin Deliver err = %v, want 404 (R13-Q8)", err)
	}
	if len(delivered.all()) != 0 {
		t.Errorf("unknown plugin reached deliverer: %v", delivered.all())
	}
}

func TestServeBusy503(t *testing.T) {
	old := submitWait
	submitWait = 100 * time.Millisecond
	defer func() { submitWait = old }()

	srv, _, _ := testServe(t, nil)
	runServe(t, srv)

	// Block the worker on a gate, then fill the whole buffer with no-ops so
	// the next submission has no slot (R12-Q1: 10s wait -> 503).
	gate := make(chan struct{})
	started := make(chan struct{})
	srv.actions <- func(sv *Serve) { close(started); <-gate }
	<-started
	for i := 0; i < 64; i++ {
		srv.actions <- func(sv *Serve) {}
	}

	c := socket.NewClient(srv.socketPath, 2*time.Second)
	_, err := c.Bundle(context.Background(), "r")
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("Bundle with full channel = %v, want 503 busy (R12-Q1)", err)
	}
	close(gate)
}

func TestServeCatchUpAtStartup(t *testing.T) {
	plugins := []config.PluginConfig{{ID: "p1", Type: "http"}}
	srv, delivered, store := testServe(t, plugins)

	// A fresh pending record is due immediately: catch-up must deliver it
	// before socket work is accepted (R10-Q9).
	ev := &event.Event{
		SchemaVersion: event.SchemaVersion,
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          "r",
		Ref:           "refs/heads/main",
		Type:          event.TypePush,
	}
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	runServe(t, srv)

	deadline := time.Now().Add(3 * time.Second)
	for delivered.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if delivered.count() == 0 {
		t.Fatal("catch-up did not deliver the due pending event")
	}
}

func TestServeStaleSocketGuard(t *testing.T) {
	srv, _, _ := testServe(t, nil)
	// A plain file at the socket path is NOT a socket: Run must refuse to
	// unlink it and fail fast (R12-Q4 guard).
	if err := os.WriteFile(srv.socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "not a socket") {
			t.Errorf("Run with non-socket path = %v, want refusal", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run accepted a non-socket path")
	}
}

func TestBundleActionRetries(t *testing.T) {
	old := bundleBackoff
	bundleBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { bundleBackoff = old }()

	srv, _, _ := testServe(t, nil)
	// CreateBundle validates the repo name first, so an invalid name fails
	// without touching git; the retry wrapper must surface the error after
	// one initial attempt + 3 retries.
	_, err := srv.bundleAction("bad name!")
	if err == nil {
		t.Error("bundleAction with invalid repo = nil error")
	}
}
