package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/disk"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
	"github.com/ChronicCmposer/gitd/internal/webhook"
	// The policy under test self-registers via init (4.1).
	_ "github.com/ChronicCmposer/gitd/internal/webhook/policies/nonfastforward"
)

const cliGitBin = "/usr/bin/git"

// fakeSocketServer starts a unix-socket HTTP server answering /v1/bundle and
// /v1/deliver, returning its path and the requested bundle repo.
func fakeSocketServer(t *testing.T) (string, *[]string, *[]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitd.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var bundles []string
	var delivers []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bundle":
			var req socket.BundleRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			bundles = append(bundles, req.Repo)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"uploaded":true}`)
		case "/v1/deliver":
			var req socket.DeliverRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			delivers = append(delivers, req.PluginID+"/"+req.EventID)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return path, &bundles, &delivers
}

// cliBareRepo builds a bare sha256 repo with three commits at root/name.git.
func cliBareRepo(t *testing.T, root, name string) string {
	t.Helper()
	work := t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.Command(cliGitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(work, "init", "-q", "-b", "main", "--object-format=sha256", ".")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "T")
	for _, f := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(work, f), []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(work, "add", f)
		run(work, "commit", "-qm", "commit "+f)
	}
	repoDir := filepath.Join(root, name+".git")
	run(t.TempDir(), "clone", "-q", "--bare", work, repoDir)
	return repoDir
}

// testNotifier wires a notifier over temp dirs; the socket client points at
// the fake server.
func testNotifier(t *testing.T) (*notifier, string, *[]string, *[]string, *spool.Store) {
	t.Helper()
	root := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sockPath, bundles, delivers := fakeSocketServer(t)
	store := spool.NewStore(t.TempDir(), time.Now, 90*24*time.Hour, log)
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	n := &notifier{
		gitd:      &config.GitdConfig{GitBinary: cliGitBin, Spool: config.SpoolConfig{Retention: config.Duration(90 * 24 * time.Hour)}},
		webhooks:  &config.WebhooksConfig{},
		reposRoot: root,
		git:       gitenv.NewRunner(cliGitBin, t.TempDir(), os.Getenv("PATH")),
		spool:     store,
		client:    socket.NewClient(sockPath, 5*time.Second),
		now:       func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
		log:       log,
	}
	return n, root, bundles, delivers, store
}

func TestNotifyPushEventsAndBundle(t *testing.T) {
	n, root, bundles, _, store := testNotifier(t)
	repoDir := cliBareRepo(t, root, "r")
	old, new := refPair(t, repoDir)

	stdin := fmt.Sprintf("%s %s refs/heads/main\n", old, new)
	if err := n.Notify(repoDir, strings.NewReader(stdin)); err != nil {
		t.Fatal(err)
	}
	if len(*bundles) != 1 || (*bundles)[0] != "r" {
		t.Errorf("bundle requests = %v", *bundles)
	}
	recs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("spool records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Repo != "r" || rec.Ref != "refs/heads/main" || rec.Type != event.TypePush {
		t.Errorf("record = %+v", rec)
	}
	// old..new spans commits 2 and 3 (refPair skips the root commit).
	if len(rec.Commits) != 2 {
		t.Errorf("commits = %+v, want 2 summaries", rec.Commits)
	}
}

func TestNotifyRefCreatedAndDeleted(t *testing.T) {
	n, root, _, _, store := testNotifier(t)
	repoDir := cliBareRepo(t, root, "r")
	_, head := refPair(t, repoDir)
	z := strings.Repeat("0", 64)

	// ref-created (old = zeros) => all 3 commits reachable from head.
	if err := n.Notify(repoDir, strings.NewReader(z+" "+head+" refs/heads/dev\n")); err != nil {
		t.Fatal(err)
	}
	// ref-deleted (new = zeros) => no commits.
	if err := n.Notify(repoDir, strings.NewReader(head+" "+z+" refs/heads/dev\n")); err != nil {
		t.Fatal(err)
	}
	recs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	types := map[string]bool{}
	for i := range recs {
		types[recs[i].Type] = true
	}
	if !types[event.TypeRefCreated] || !types[event.TypeRefDeleted] {
		t.Errorf("types = %v, want ref-created + ref-deleted", types)
	}
	for i := range recs {
		switch recs[i].Type {
		case event.TypeRefCreated:
			if len(recs[i].Commits) != 3 {
				t.Errorf("ref-created commits = %+v, want 3", recs[i].Commits)
			}
		case event.TypeRefDeleted:
			if len(recs[i].Commits) != 0 {
				t.Errorf("ref-deleted commits = %+v, want 0", recs[i].Commits)
			}
		}
	}
}

func TestNotifyRejectsBadCwd(t *testing.T) {
	n, _, _, _, _ := testNotifier(t)
	// Outside reposRoot (R9-Q6 fail closed).
	if err := n.Notify(t.TempDir(), strings.NewReader("")); err == nil {
		t.Error("Notify outside reposRoot = nil error")
	}
	// Inside reposRoot but not a bare repo.
	dir := filepath.Join(n.reposRoot, "notbare")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(dir, strings.NewReader("")); err == nil {
		t.Error("Notify on non-bare dir = nil error")
	}
}

func TestNotifyMalformedLineSkipped(t *testing.T) {
	n, root, bundles, _, store := testNotifier(t)
	repoDir := cliBareRepo(t, root, "r")
	old, new := refPair(t, repoDir)

	// Malformed line + valid line: the push must not fail (R9-Q7) and the
	// malformed line must not produce an event.
	stdin := "not-a-sha refs/heads/main\n" + fmt.Sprintf("%s %s refs/heads/main\n", old, new)
	if err := n.Notify(repoDir, strings.NewReader(stdin)); err != nil {
		t.Fatal(err)
	}
	if len(*bundles) != 1 {
		t.Errorf("bundle requests = %v", *bundles)
	}
	recs, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Errorf("records = %d, want 1 (malformed skipped)", len(recs))
	}
}

func TestNotifySyncDelivery(t *testing.T) {
	n, root, _, delivers, _ := testNotifier(t)
	n.webhooks.Plugins = []config.PluginConfig{{ID: "p1", Type: "http", Sync: true}}
	repoDir := cliBareRepo(t, root, "r")
	old, new := refPair(t, repoDir)

	// Two ref lines => two events, each delivered to p1 (R11-Q1, R10-Q2).
	stdin := fmt.Sprintf("%s %s refs/heads/main\n", old, new) +
		fmt.Sprintf("%s %s refs/heads/dev\n", old, new)
	if err := n.Notify(repoDir, strings.NewReader(stdin)); err != nil {
		t.Fatal(err)
	}
	if len(*delivers) != 2 {
		t.Errorf("sync deliveries = %v, want 2", *delivers)
	}
}

// refPair returns (old, new) shas for the repo's commits such that old..new
// spans exactly the second and third commits (skipping the root commit).
func refPair(t *testing.T, repoDir string) (string, string) {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command(cliGitBin, append([]string{"--git-dir=" + repoDir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	all := strings.Split(run("rev-list", "--reverse", "refs/heads/main"), "\n")
	if len(all) < 3 {
		t.Fatalf("need 3 commits, have %v", all)
	}
	return all[0], all[len(all)-1]
}

func TestPreReceiveStrictParse(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	p := &preReceive{headroom: diskHeadroomOK(), engine: testPolicyEngine(t, ""), log: log}

	if err := p.Run(t.TempDir(), strings.NewReader("")); err != nil {
		t.Errorf("zero lines rejected: %v (R11-Q10)", err)
	}
	z := strings.Repeat("0", 64)
	sha := strings.Repeat("a", 64)
	good := fmt.Sprintf("%s %s refs/heads/main\n", z, sha)
	if err := p.Run(t.TempDir(), strings.NewReader(good)); err != nil {
		t.Errorf("valid line rejected: %v", err)
	}
	bad := []string{
		"only-two-fields\n",
		fmt.Sprintf("%s %s\n", z, sha),
		"zz " + sha + " refs/heads/main\n",
		fmt.Sprintf("%s %s heads/main\n", z, sha), // ref without refs/ prefix
	}
	for _, in := range bad {
		if err := p.Run(t.TempDir(), strings.NewReader(in)); err == nil {
			t.Errorf("malformed line %q accepted (R9-Q7)", in)
		}
	}
}

func TestPreReceiveRejectsNonFastForward(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	repoDir := cliBareRepo(t, t.TempDir(), "r")
	old, new := refPair(t, repoDir)
	// Reversing old/new is a non-fast-forward: the guard rejects the push with
	// a clear stderr message (R5-Q1, R11-Q10).
	p := &preReceive{headroom: diskHeadroomOK(), engine: testPolicyEngine(t, repoDir), log: log}
	stdin := fmt.Sprintf("%s %s refs/heads/main\n", new, old)
	err := p.Run(repoDir, strings.NewReader(stdin))
	if err == nil || !strings.Contains(err.Error(), "not a fast-forward") {
		t.Errorf("non-fast-forward push err = %v, want rejection message", err)
	}
}

func TestPreReceiveAcceptsFastForward(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	repoDir := cliBareRepo(t, t.TempDir(), "r")
	old, new := refPair(t, repoDir)
	p := &preReceive{headroom: diskHeadroomOK(), engine: testPolicyEngine(t, repoDir), log: log}
	stdin := fmt.Sprintf("%s %s refs/heads/main\n", old, new)
	if err := p.Run(repoDir, strings.NewReader(stdin)); err != nil {
		t.Errorf("fast-forward push rejected: %v", err)
	}
}

// testPolicyEngine builds a pre-receive engine with the non-fast-forward
// policy enabled against repoDir (or a throwaway dir when repoDir is nil).
func testPolicyEngine(t *testing.T, repoDir string) *webhook.PolicyEngine {
	t.Helper()
	if repoDir == "" {
		repoDir = t.TempDir()
	}
	git := gitenv.NewRunner(cliGitBin, t.TempDir(), os.Getenv("PATH"))
	e := webhook.NewPolicyEngine(webhook.DefaultPolicies, webhook.PolicyDeps{Git: git, RepoDir: repoDir})
	if err := e.Build(config.PoliciesConfig{Enabled: []string{"non-fast-forward"}}); err != nil {
		t.Fatal(err)
	}
	return e
}

// diskHeadroomOK returns a headroom that never rejects (tests the parse and
// policy gates, not the disk).
func diskHeadroomOK() disk.Headroom { return disk.Headroom{MinFree: 1, WarnFree: 1} }

func TestSpoolListNDJSON(t *testing.T) {
	store := spool.NewStore(t.TempDir(), time.Now, 90*24*time.Hour, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ev := &event.Event{SchemaVersion: event.SchemaVersion, EventID: "ev1", CreatedAt: "2026-01-01T00:00:00Z", Repo: "r", Ref: "refs/heads/main", Type: event.TypePush}
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := spoolList(store, &buf); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"event-id":"ev1"`) || !strings.Contains(line, `"state":"pending"`) {
		t.Errorf("NDJSON line = %q", line)
	}
}

func TestSpoolReplayDelivers(t *testing.T) {
	store := spool.NewStore(t.TempDir(), time.Now, 90*24*time.Hour, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ev := &event.Event{SchemaVersion: event.SchemaVersion, EventID: "ev1", CreatedAt: "2026-01-01T00:00:00Z", Repo: "r", Ref: "refs/heads/main", Type: event.TypePush}
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}

	// Point webhooks.yaml at the fake socket server via a temp gitd dir.
	dir := t.TempDir()
	whPath := filepath.Join(dir, "webhooks.yaml")
	os.WriteFile(whPath, []byte("plugins:\n  - id: p1\n    type: logger\n"), 0o600)
	sockPath, _, delivers := fakeSocketServer(t)

	if err := spoolReplay(filepath.Join(dir, "gitd.yaml"), store, "ev1", socket.NewClient(sockPath, 5*time.Second), slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil {
		t.Fatal(err)
	}
	if len(*delivers) != 1 || (*delivers)[0] != "p1/ev1" {
		t.Errorf("replay deliveries = %v", *delivers)
	}
}

func TestSpoolReplayServeDown(t *testing.T) {
	store := spool.NewStore(t.TempDir(), time.Now, 90*24*time.Hour, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ev := &event.Event{SchemaVersion: event.SchemaVersion, EventID: "ev1", CreatedAt: "2026-01-01T00:00:00Z", Repo: "r", Ref: "refs/heads/main", Type: event.TypePush}
	if _, err := store.Write(ev); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "webhooks.yaml"), []byte("plugins:\n  - id: p1\n    type: logger\n"), 0o600)
	missing := filepath.Join(t.TempDir(), "missing.sock")
	err := spoolReplay(filepath.Join(dir, "gitd.yaml"), store, "ev1", socket.NewClient(missing, time.Second), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err == nil || !strings.Contains(err.Error(), "connect") {
		t.Errorf("replay with serve down = %v, want connection error (R12-Q2)", err)
	}
}

func TestMirrorListNDJSON(t *testing.T) {
	m := &mirror.Mirror{}
	_ = m
	store := objectstore.NewMemoryStore()
	store.Put(context.Background(), "repos/r/2026-01-02T03-04-05.123456789Z.bundle", []byte("x"))
	git := gitenv.NewRunner(cliGitBin, t.TempDir(), os.Getenv("PATH"))
	m = mirror.New(store, git, t.TempDir(), "repos", t.TempDir(), time.Now, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var buf bytes.Buffer
	if err := mirrorList(m, "r", &buf); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `"repo":"r"`) || !strings.Contains(line, "2026-01-02T03-04-05.123456789Z.bundle") {
		t.Errorf("mirror list NDJSON = %q", line)
	}
}

func TestMirrorListAllNDJSON(t *testing.T) {
	// `gitd mirror list` (no arg) emits one NDJSON {repo, bundles} object per
	// mirrored repo, sorted by repo name, reusing the single-repo shape
	// (R13-Q6).
	store := objectstore.NewMemoryStore()
	ctx := context.Background()
	store.Put(ctx, "repos/alpha/2026-01-02T03-04-05.123456789Z.bundle", []byte("x"))
	store.Put(ctx, "repos/alpha/2026-01-02T04-04-05.123456789Z.bundle", []byte("x"))
	store.Put(ctx, "repos/beta/2026-01-02T03-04-05.123456789Z.bundle", []byte("x"))
	git := gitenv.NewRunner(cliGitBin, t.TempDir(), os.Getenv("PATH"))
	m := mirror.New(store, git, t.TempDir(), "repos", t.TempDir(), time.Now, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var buf bytes.Buffer
	if err := mirrorListAll(m, &buf); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("mirror list-all NDJSON lines = %d, want 2: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"repo":"alpha"`) || !strings.Contains(lines[0], "2026-01-02T03-04-05.123456789Z.bundle") || !strings.Contains(lines[0], "2026-01-02T04-04-05.123456789Z.bundle") {
		t.Errorf("first line = %q, want alpha with both bundles", lines[0])
	}
	if !strings.Contains(lines[1], `"repo":"beta"`) || !strings.Contains(lines[1], "2026-01-02T03-04-05.123456789Z.bundle") {
		t.Errorf("second line = %q, want beta NDJSON", lines[1])
	}
}

func TestMirrorListAllNDJSONEmpty(t *testing.T) {
	// No mirrored repos: `gitd mirror list` (no arg) emits nothing and
	// succeeds.
	store := objectstore.NewMemoryStore()
	git := gitenv.NewRunner(cliGitBin, t.TempDir(), os.Getenv("PATH"))
	m := mirror.New(store, git, t.TempDir(), "repos", t.TempDir(), time.Now, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var buf bytes.Buffer
	if err := mirrorListAll(m, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("mirror list-all (empty) emitted %q, want nothing", buf.String())
	}
}

func TestDDNS(t *testing.T) {
	old := ddnsEndpoint
	ddnsEndpoint = ""
	defer func() { ddnsEndpoint = old }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("host") != "git" || q.Get("domain") != "cmposer.cc" || q.Get("password") != "s3cret" {
			http.Error(w, "bad params", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "Good 1.2.3.4")
	}))
	defer ts.Close()
	ddnsEndpoint = ts.URL

	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("s3cret\n"), 0o600)
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+pw+"\n"), 0o600)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitOK {
		t.Errorf("ddns exit = %d, stderr = %s", code, stderr.String())
	}
}
