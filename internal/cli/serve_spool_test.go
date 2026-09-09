package cli

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

// spoolStoreAt builds a spool.Store at dir with default retention.
func spoolStoreAt(dir string) *spool.Store {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return spool.NewStore(dir, time.Now, 90*24*time.Hour, log)
}

// eventForTest builds a minimal valid event.
func eventForTest(id, repo string) *event.Event {
	return &event.Event{
		SchemaVersion: event.SchemaVersion,
		EventID:       id,
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          repo,
		Ref:           "refs/heads/main",
		Type:          event.TypePush,
	}
}

func TestRunServeGatewayBranch(t *testing.T) {
	// runServe with SSH_CONNECTION set must take the sshcmd gateway branch
	// (R10-Q1, R12-Q5): greeting for an empty SSH_ORIGINAL_COMMAND, no
	// daemon wiring (no S3/TLS touched). runServe writes to os.Stdout (the
	// sshd hook fd), so we capture the real process stdout.
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	setRuntimePaths(t)

	binDir := t.TempDir()
	writeScript := func(name, body string) {
		p := filepath.Join(binDir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeScript("git", `exit 0`)
	writeScript("git-upload-pack", `echo "upload-pack ok"; exit 0`)

	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("git_binary: "+filepath.Join(binDir, "git")+"\n"), 0o600)

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("SSH_CONNECTION", "1.2.3.4 51234 10.0.0.5 22")
	t.Setenv("SSH_AUTH_KEY_FP", "SHA256:abcd")
	t.Setenv("SSH_AUTH_KEY_TYPE", "ssh-ed25519")
	t.Setenv("SSH_AUTH_CERT_ID", "git@cmposer")

	capture := captureStdout(t)

	var stderr bytes.Buffer
	// Empty SSH_ORIGINAL_COMMAND (unset): greeting path, exit 0.
	if code := Run([]string{"serve", "--config", cfgPath}, io.Discard, &stderr); code != ExitOK {
		t.Errorf("serve gateway greeting exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(capture(), "no shell access") {
		t.Errorf("greeting missing hint: %q", capture())
	}

	// A valid upload-pack command execs the fake git-upload-pack.
	stderr.Reset()
	t.Setenv("SSH_ORIGINAL_COMMAND", "git-upload-pack myrepo")
	if code := Run([]string{"serve", "--config", cfgPath}, io.Discard, &stderr); code != ExitOK {
		t.Errorf("serve gateway upload exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(capture(), "upload-pack ok") {
		t.Errorf("upload stdout = %q", capture())
	}
}

// syncedBuffer is an io.Writer that serializes every write against the same
// mutex used to read the buffer, so captureStdout's reader goroutine and the
// returned closure never touch the buffer concurrently (race-free under
// `go test -race`, R3-Q8).
type syncedBuffer struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (s syncedBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// captureStdout redirects os.Stdout to a pipe and returns a func returning
// the captured bytes so far. Callers must invoke it after each Run (which
// writes os.Stdout synchronously); the closure waits until the reader
// goroutine has drained those writes instead of relying on a fixed sleep, so
// the assertions are deterministic (race-free under `go test -race`, R3-Q8).
func captureStdout(t *testing.T) func() string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	var mu sync.Mutex
	var buf bytes.Buffer
	go func() {
		io.Copy(syncedBuffer{mu: &mu, buf: &buf}, r)
	}()
	t.Cleanup(func() {
		w.Close()
		r.Close()
		os.Stdout = old
	})
	return func() string {
		// Wait until the buffer has data and stops growing: the pipe has
		// drained all writes made so far. Bounded to avoid a hang.
		for i := 0; i < 50; i++ {
			mu.Lock()
			n := buf.Len()
			mu.Unlock()
			if n > 0 {
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				if buf.Len() == n {
					mu.Unlock()
					break
				}
				mu.Unlock()
			} else {
				time.Sleep(5 * time.Millisecond)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

func TestRunSpoolListWithTempDirs(t *testing.T) {
	// runSpool list success with the spool dir var pointed at a temp dir.
	setRuntimePaths(t)
	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("log:\n  level: error\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfgPath, "list"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("spool list exit = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("spool list empty = %q, want empty NDJSON stream", stdout.String())
	}
}

func TestRunSpoolReplaySuccess(t *testing.T) {
	// runSpool replay with a real spool event + fake socket: the entry wires
	// the store + client and delivers (R12-Q2).
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	setRuntimePaths(t)
	_, delivers := fakeSocketWithRecords(t, socketPath)

	// Write a spool event directly through the store.
	store := spoolStoreAt(spoolDir)
	ev := eventForTest("ev1", "r")
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("log:\n  level: error\n"), 0o600)
	os.WriteFile(webhooksPathFor(cfgPath), []byte("plugins:\n  - id: p1\n    type: logger\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfgPath, "replay", "ev1"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("spool replay exit = %d, stderr = %s", code, stderr.String())
	}
	if len(*delivers) != 1 || (*delivers)[0] != "p1/ev1" {
		t.Errorf("delivers = %v, want [p1/ev1]", *delivers)
	}
}

func TestRunSpoolPurge(t *testing.T) {
	setRuntimePaths(t)
	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("log:\n  level: error\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfgPath, "purge"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("spool purge exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "purged 0 events") {
		t.Errorf("purge stdout = %q", stdout.String())
	}
}

func TestSpoolListEncodeError(t *testing.T) {
	// A failing writer makes spoolList return the encode error.
	setRuntimePaths(t)
	store := spoolStoreAt(spoolDir)
	ev := eventForTest("ev1", "r")
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	if err := spoolList(store, errWriter{}); err == nil {
		t.Fatal("spoolList with failing writer = nil error")
	}
}

// errWriter fails every write (io.Writer error path for NDJSON encoders).
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("disk full") }

func TestApplyReloadPropagatesRetention(t *testing.T) {
	// A SIGHUP reload must update the retention TTL actually used by the
	// spool store for purge/sweep (R6-Q1), and only on a successful reload
	// (R8-Q6 fail-safe). setRuntimePaths points spoolDir at a temp dir.
	setRuntimePaths(t)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitd.yaml")
	webhooksPath := webhooksPathFor(cfgPath)
	writeCfg := func(retention string) {
		os.WriteFile(cfgPath, []byte("spool:\n  retention: "+retention+"\n"), 0o600)
	}
	os.WriteFile(webhooksPath, []byte("plugins: []\n"), 0o600)
	writeCfg("10h")

	rt, err := config.Load(cfgPath, webhooksPath, log)
	if err != nil {
		t.Fatal(err)
	}

	// Store booted with a 90d retention; the delivered record is created 2h
	// before "now", so it survives under both 10h and 90d.
	store := spool.NewStore(spoolDir, func() time.Time { return now }, 90*24*time.Hour, log)
	ev := eventForTest("ev-ret", "r")
	ev.CreatedAt = "2026-01-01T10:00:00Z"
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetState("ev-ret", spool.StateDelivered); err != nil {
		t.Fatal(err)
	}
	if n, err := store.Purge(); err != nil || n != 0 {
		t.Fatalf("Purge with boot retention = %d, %v; want 0", n, err)
	}

	// Reload to a 1h retention; the 2h-old delivered record must now expire.
	writeCfg("1h")
	if err := applyReload(rt, store); err != nil {
		t.Fatalf("applyReload = %v", err)
	}
	if got := rt.Gitd().Spool.Retention.D(); got != time.Hour {
		t.Fatalf("reloaded retention = %v, want 1h", got)
	}
	n, err := store.Purge()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Purge after reload = %d, want 1 (reloaded 1h TTL expired the 2h-old record)", n)
	}
}

func TestApplyReloadKeepsRetentionOnFailure(t *testing.T) {
	// Fail-safe (R8-Q6): an invalid reloaded config must leave the previous
	// retention live — the store must still expire on the OLD TTL, not on the
	// unparsed candidate.
	setRuntimePaths(t)

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitd.yaml")
	webhooksPath := webhooksPathFor(cfgPath)
	os.WriteFile(webhooksPath, []byte("plugins: []\n"), 0o600)
	// Valid boot config: 10h retention.
	os.WriteFile(cfgPath, []byte("spool:\n  retention: 10h\n"), 0o600)

	rt, err := config.Load(cfgPath, webhooksPath, log)
	if err != nil {
		t.Fatal(err)
	}

	store := spool.NewStore(spoolDir, func() time.Time { return now }, 10*time.Hour, log)
	ev := eventForTest("ev-ret", "r")
	ev.CreatedAt = "2026-01-01T00:00:00Z" // 12h before now: expired under the 10h boot TTL.
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetState("ev-ret", spool.StateDelivered); err != nil {
		t.Fatal(err)
	}

	// Break the config (negative retention fails validation): reload must
	// fail loudly and the store must keep the 10h boot retention.
	os.WriteFile(cfgPath, []byte("spool:\n  retention: -5h\n"), 0o600)
	if err := applyReload(rt, store); err == nil {
		t.Fatal("applyReload = nil, want validation error on bad retention")
	}
	n, err := store.Purge()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Purge after failed reload = %d, want 1 (boot 10h TTL still live)", n)
	}
}

func TestRunServeDaemonStorageError(t *testing.T) {
	// Without SSH_CONNECTION, runServe takes the daemon branch and builds the
	// S3 store first. With no AWS credentials in the sandbox this fails
	// loudly at runtime (exit 1) — proving the daemon wiring starts, without
	// needing real AWS (R6-Q7 credential chain).
	setRuntimePaths(t)
	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("log:\n  level: error\n"), 0o600)
	os.WriteFile(webhooksPathFor(cfgPath), []byte("plugins: []\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"serve", "--config", cfgPath}, &stdout, &stderr); code != ExitError {
		t.Errorf("serve daemon exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
}

func TestSpoolReplayMissingEvent(t *testing.T) {
	setRuntimePaths(t)
	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("log:\n  level: error\n"), 0o600)
	os.WriteFile(webhooksPathFor(cfgPath), []byte("plugins:\n  - id: p1\n    type: logger\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfgPath, "replay", "missing"}, &stdout, &stderr); code != ExitError {
		t.Errorf("spool replay missing event exit = %d, want %d", code, ExitError)
	}
}

func TestSpoolReplayNoPlugins(t *testing.T) {
	setRuntimePaths(t)
	store := spoolStoreAt(spoolDir)
	ev := eventForTest("ev1", "r")
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("log:\n  level: error\n"), 0o600)
	os.WriteFile(webhooksPathFor(cfgPath), []byte("plugins: []\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfgPath, "replay", "ev1"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("spool replay no plugins exit = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunDDNSUpdateFailure(t *testing.T) {
	old := ddnsEndpoint
	ddnsEndpoint = ""
	defer func() { ddnsEndpoint = old }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ErrBadDomain")
	}))
	defer ts.Close()
	ddnsEndpoint = ts.URL

	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("s3cret\n"), 0o600)
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+pw+"\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitError {
		t.Errorf("ddns failure exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ddns") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
