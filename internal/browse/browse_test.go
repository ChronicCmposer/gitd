package browse

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
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
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore"
	"github.com/ChronicCmposer/gitd/internal/serve"
	"github.com/ChronicCmposer/gitd/internal/spool"
	"github.com/ChronicCmposer/gitd/internal/version"
)

const gitBin = "/usr/bin/git"

func testGit(t *testing.T) *gitenv.Runner {
	t.Helper()
	if _, err := exec.LookPath(gitBin); err != nil {
		t.Skip("git not available")
	}
	return gitenv.NewRunner(gitBin, t.TempDir(), os.Getenv("PATH"))
}

// makeRepo creates a bare sha1 repo at root/name.git with a README, a code
// file, a nested dir, and 3 commits.
func makeRepo(t *testing.T, root, name string) {
	t.Helper()
	work := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Hello\n\n<script>alert(1)</script>\n\n- [x] task\n\n| a | b |\n|---|---|\n| 1 | 2 |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "lib", "util.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-qm", "first")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Hello again\n\n<script>alert(1)</script>\n\nsafe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-qm", "second")
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\nfunc main() { println(\"v3\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-qm", "third")
	run(root, "clone", "-q", "--bare", work, filepath.Join(root, name+".git"))
}

// emptyRepo creates a bare repo with no refs (R13-Q7).
func emptyRepo(t *testing.T, root, name string) {
	t.Helper()
	git := testGit(t)
	if _, err := git.Run(context.Background(), "init", "--bare", filepath.Join(root, name+".git")); err != nil {
		t.Fatal(err)
	}
}

// testHandler wires a running serve daemon + a browse Handler for the Mux
// tests. Returns the handler and a cleanup that stops the serve worker.
func testHandler(t *testing.T, render string, allowlist []string) *Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reposRoot := t.TempDir()
	workDir := t.TempDir()
	spoolDir := t.TempDir()
	sockPath := filepath.Join(t.TempDir(), "gitd.sock")

	store := spool.NewStore(spoolDir, time.Now, 90*24*time.Hour, log)
	m := mirror.New(objectstore.NewMemoryStore(), testGit(t), reposRoot, "repos", workDir, time.Now, log)
	srv := serve.New(serve.Config{
		Mirror:     m,
		Spool:      store,
		Webhooks:   func() *config.WebhooksConfig { return &config.WebhooksConfig{} },
		Deliver:    func(_ context.Context, _, _ string) error { return nil },
		ReposRoot:  reposRoot,
		SocketPath: sockPath,
		Now:        time.Now,
		Log:        log,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	waitSocket(t, sockPath)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serve did not stop")
		}
	})

	if allowlist == nil {
		allowlist = []string{"git.cmposer.cc", "localhost", "127.0.0.1"}
	}
	h, err := New(Config{
		ReposRoot:     reposRoot,
		Git:           testGit(t),
		Render:        render,
		Serve:         srv,
		HostAllowlist: allowlist,
		TLS:           &tls.Config{MinVersion: tls.VersionTLS13},
		Log:           log,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.reposRoot = reposRoot // tests place repos here
	return h
}

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
	t.Fatalf("socket %s never accepted", path)
}

// get is a helper issuing a GET against the handler mux with a canonical Host.
func get(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "git.cmposer.cc"
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	return rec
}

func TestHostAllowlistRejectsMismatch(t *testing.T) {
	h := testHandler(t, "server", []string{"git.cmposer.cc", "localhost", "127.0.0.1"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "evil.example.com"
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("mismatched Host = %d, want 400 (R8-Q5)", rec.Code)
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	h := testHandler(t, "server", nil)
	rec := get(t, h, "/")
	for _, hdr := range []string{
		"Content-Security-Policy",
		"X-Content-Type-Options",
		"Referrer-Policy",
		"X-Frame-Options",
	} {
		if rec.Header().Get(hdr) == "" {
			t.Errorf("missing security header %s (R3-Q6)", hdr)
		}
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("CSP = %q, want default-src 'self'", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestRepoIndexListsRepos(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "alpha")
	makeRepo(t, h.reposRoot, "beta")
	rec := get(t, h, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("index = %d, want 200", rec.Code)
	}
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(rec.Body.String(), name) {
			t.Errorf("index missing repo %s", name)
		}
	}
}

func TestEmptyRepoPageIs200(t *testing.T) {
	h := testHandler(t, "server", nil)
	emptyRepo(t, h.reposRoot, "fresh")
	rec := get(t, h, "/fresh")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty repo = %d, want 200 (R13-Q7)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty") {
		t.Errorf("empty repo page missing empty notice")
	}
	if !strings.Contains(rec.Body.String(), "git push -u origin main") {
		t.Errorf("empty repo page missing push hint (R13-Q7)")
	}
}

func TestRepoHomeRendersPages(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")
	rec := get(t, h, "/repo1")
	if rec.Code != http.StatusOK {
		t.Fatalf("repo home = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"first", "second", "third", "main.go", "lib", "commits"} {
		if !strings.Contains(body, want) {
			t.Errorf("repo home missing %q", want)
		}
	}
}

func TestBlobAndTreeAndRaw(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")

	tree := get(t, h, "/repo1/tree?ref=main")
	if tree.Code != http.StatusOK || !strings.Contains(tree.Body.String(), "main.go") {
		t.Fatalf("tree page failed: %d", tree.Code)
	}
	sub := get(t, h, "/repo1/tree?ref=main&path=lib")
	if sub.Code != http.StatusOK || !strings.Contains(sub.Body.String(), "util.go") {
		t.Fatalf("nested tree failed: %d", sub.Code)
	}
	blob := get(t, h, "/repo1/blob?ref=main&path=main.go")
	// Server mode now syntax-highlights main.go (Chroma wraps tokens in spans),
	// so assert on a token substring rather than the raw "func main" text.
	if blob.Code != http.StatusOK || !strings.Contains(blob.Body.String(), "func") {
		t.Fatalf("blob page failed: %d", blob.Code)
	}
	raw := get(t, h, "/repo1/raw?ref=main&path=main.go")
	if raw.Code != http.StatusOK {
		t.Fatalf("raw = %d", raw.Code)
	}
	if !strings.Contains(raw.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("raw missing Content-Disposition attachment (R2-Q6)")
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")
	// Create a symlink outside the repo that points to a real repo's object
	// store; a repo named through it must be rejected (R5-Q3).
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.reposRoot, "escape.git")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "/escape")
	// repoDir uses RealpathUnder which rejects the escape -> 404/400, never 200.
	if rec.Code == http.StatusOK {
		t.Errorf("symlink-escape repo served a page, want rejection (R5-Q3)")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")
	for _, p := range []string{"../etc/passwd", "/etc/passwd", "a/../../etc/passwd"} {
		rec := get(t, h, "/repo1/blob?ref=main&path="+p)
		if rec.Code == http.StatusOK {
			t.Errorf("path %q served content, want rejection", p)
		}
	}
}

func TestXSSNoRawHTMLServerSide(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1") // HEAD README contains <script>alert(1)</script>
	rec := get(t, h, "/repo1")
	if rec.Code != http.StatusOK {
		t.Fatalf("home = %d", rec.Code)
	}
	body := rec.Body.String()
	// R2-Q6: raw HTML from the README must never render inline. goldmark with
	// unsafe=false neutralizes it (omitted or escaped) — the raw tag must not
	// appear, and the README itself must still have rendered.
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw script tag leaked into server-rendered HTML (R2-Q6)")
	}
	if strings.Contains(body, "<script") {
		t.Errorf("any raw script markup leaked")
	}
	if !strings.Contains(body, "Hello again") {
		t.Errorf("README did not render")
	}
}

func TestClientRenderModeServesAssets(t *testing.T) {
	h := testHandler(t, "client", nil)
	makeRepo(t, h.reposRoot, "repo1")
	// The client-render home page must reference /static/ marked+highlight.
	rec := get(t, h, "/repo1")
	body := rec.Body.String()
	for _, asset := range []string{
		"/static/vendor/marked/12.0.2/marked.min.js",
		"/static/vendor/highlight.js/11.10.0/highlight.min.js",
		"/static/render-client.js",
		"/static/vendor/chroma/gruvbox.css",
	} {
		if !strings.Contains(body, asset) {
			t.Errorf("client home page missing %s (R10-Q8)", asset)
		}
	}
	// The vendored assets must actually be served from /static/.
	for _, asset := range []string{
		"/static/vendor/marked/12.0.2/marked.min.js",
		"/static/vendor/highlight.js/11.10.0/highlight.min.js",
		"/static/vendor/highlight.js/gruvbox/gruvbox-dark.css",
		"/static/vendor/chroma/gruvbox.css",
		"/static/style.css",
	} {
		r := get(t, h, asset)
		if r.Code != http.StatusOK {
			t.Errorf("static asset %s = %d, want 200 (R10-Q8)", asset, r.Code)
		}
	}
	// Client mode: raw script still must not leak (escaped into data-markdown).
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("raw script leaked in client mode")
	}
}

func TestStyleSheetPureBlackAppCanvas(t *testing.T) {
	// The page canvas is pure black (--bg-app #000000) and the primary
	// surfaces are pure black too (--bg0 #000000) while --bg1 stays the
	// elevated tone: the served stylesheet must declare both backgrounds,
	// apply the app background to the body, keep the two depth gradients,
	// and override the vendored #282828 code-token containers. The elevated
	// surface gradients (.mast, .panel-title) now fade from the near-black
	// --bg-elevated (#1d2021) into the pure-black canvas instead of the old
	// --bg1-based gradient.
	h := testHandler(t, "server", nil)
	rec := get(t, h, "/static/style.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("style.css = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"--bg0: #000000",
		"--bg-app: #000000",
		"--bg-elevated: linear-gradient(180deg, #1d2021, #000000);",
		"background: var(--bg-elevated);",
		"var(--bg-app)",
		"rgba(131,165,152,0.08)", // blue depth wash kept
		"rgba(254,128,25,0.06)",  // orange depth wash kept
		"pre.chroma, pre.hljs, .chroma, .hljs, .bg",
		"background-color: #000000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("style.css missing %q", want)
		}
	}
	if strings.Contains(body, "--bg0: #282828") {
		t.Errorf("style.css still declares the old bg0 (#282828)")
	}
	if strings.Contains(body, "linear-gradient(180deg, var(--bg1), var(--bg0))") {
		t.Errorf("style.css still uses the old --bg1 masthead/panel-title gradient")
	}
}

func TestFooterOnEveryPage(t *testing.T) {
	// The version footer is part of the page shell (layoutTmpl), so it
	// appears on every page exactly once — the old index-only .footer
	// fragment is gone (no doubled footer). In tests version.String() is the
	// v0.0.0-devel default.
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")
	wantFooter := `<footer class="footer">gitd — personal git server · mTLS browse · ` + version.String() + `</footer>`
	for _, path := range []string{"/", "/repo1", "/repo1/log?ref=main", "/repo1/tree?ref=main"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, wantFooter) {
			t.Errorf("%s missing version footer %q", path, wantFooter)
		}
		if got := strings.Count(body, "personal git server"); got != 1 {
			t.Errorf("%s 'personal git server' count = %d, want exactly 1 (no doubled footer)", path, got)
		}
	}
}

func TestRenderNoneMode(t *testing.T) {
	h := testHandler(t, "none", nil)
	makeRepo(t, h.reposRoot, "repo1")
	rec := get(t, h, "/repo1")
	if rec.Code != http.StatusOK {
		t.Fatalf("home = %d", rec.Code)
	}
	// In none mode the README shows escaped text, no client assets referenced.
	if strings.Contains(rec.Body.String(), "/static/render-client.js") {
		t.Errorf("none mode should not reference client render script")
	}
	if !strings.Contains(rec.Body.String(), "&lt;script&gt;") {
		t.Errorf("none mode should escape the README script tag")
	}
}

// The render-toggle table test (server/client/none) over the same repo.
func TestRenderToggle(t *testing.T) {
	makeRepoBody := func(h *Handler) string {
		rec := get(t, h, "/repo1")
		if rec.Code != http.StatusOK {
			t.Fatalf("home = %d", rec.Code)
		}
		return rec.Body.String()
	}
	cases := []struct {
		render string
		client bool // references client assets
		escape bool // raw script escaped
	}{
		{"server", false, true},
		{"client", true, true},
		{"none", false, true},
	}
	for _, tc := range cases {
		h := testHandler(t, tc.render, nil)
		makeRepo(t, h.reposRoot, "repo1")
		body := makeRepoBody(h)
		hasClient := strings.Contains(body, "/static/render-client.js")
		if hasClient != tc.client {
			t.Errorf("render=%s client-assets=%v want %v", tc.render, hasClient, tc.client)
		}
		if strings.Contains(body, "<script>alert(1)</script>") != false && tc.escape {
			t.Errorf("render=%s raw script leaked", tc.render)
		}
	}
}

func TestSocketEndpointsBehindMux(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "bundleme")

	// POST /v1/bundle behind the mux.
	body := bytes.NewReader([]byte(`{"repo":"bundleme"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", body)
	req.Host = "git.cmposer.cc"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/bundle behind mux = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"uploaded":true`) {
		t.Errorf("/v1/bundle reply = %s, want uploaded", rec.Body.String())
	}

	// POST /v1/deliver with an unknown plugin -> 404 final failure (R13-Q8).
	req2 := httptest.NewRequest(http.MethodPost, "/v1/deliver", bytes.NewReader([]byte(`{"plugin-id":"ghost","event-id":"e1"}`)))
	req2.Host = "git.cmposer.cc"
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("/v1/deliver unknown plugin = %d, want 404 (R13-Q8)", rec2.Code)
	}
}

func TestBusy503BehindMux(t *testing.T) {
	restore := serve.TestSetSubmitWait(100 * time.Millisecond)
	defer restore()

	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "bundleme")

	// Fill the actions channel: block the worker on a gate, fill the buffer.
	gate := make(chan struct{})
	started := make(chan struct{})
	h.serve.Submit(func(*serve.Serve) { close(started); <-gate })
	<-started
	for i := 0; i < 64; i++ {
		h.serve.Submit(func(*serve.Serve) {})
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/bundle", bytes.NewReader([]byte(`{"repo":"bundleme"}`)))
	req.Host = "git.cmposer.cc"
	rec := httptest.NewRecorder()
	h.Mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("full channel bundle = %d, want 503 busy (R12-Q1)", rec.Code)
	}
	close(gate)
}

// --- mTLS config tests ---

func TestTLSConfigProperties(t *testing.T) {
	pki := newTestPKI(t)
	cfg, err := NewTLSConfig(pki.paths())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want TLS 1.3 (R4-Q8)", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %d, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	// GetCertificate must load the P-256 server cert (R5-Q6).
	cert, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parseLeaf(cert.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.PublicKeyAlgorithm.String() != "ECDSA" {
		t.Errorf("server cert algorithm = %s, want ECDSA P-256", leaf.PublicKeyAlgorithm)
	}
}

func TestVerifyClientCert(t *testing.T) {
	pki := newTestPKI(t)

	validRaw := loadCertDER(t, pki.dir, "client")
	rsaRaw := loadCertDER(t, pki.dir, "rsa-client")

	// A valid CA-signed client cert passes (R9-Q8) when not revoked.
	if err := verifyClientCert(validRaw, revokedSet{}); err != nil {
		t.Errorf("valid client cert rejected: %v", err)
	}
	// The same cert is rejected when revoked (R7-Q5).
	if err := verifyClientCert(validRaw, revokedSet{pki.clientSerial.String(): true}); err == nil {
		t.Errorf("revoked client cert accepted")
	}
	// A non-P-256 (RSA) cert is rejected (ECDSA P-256 only).
	if err := verifyClientCert(rsaRaw, revokedSet{}); err == nil {
		t.Errorf("RSA client cert accepted, want ECDSA P-256 only")
	}
}

func loadCertDER(t *testing.T, dir, name string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM")
	}
	return [][]byte{block.Bytes}
}

func TestGetConfigForClientReloads(t *testing.T) {
	pki := newTestPKI(t)
	cfg, err := NewTLSConfig(pki.paths())
	if err != nil {
		t.Fatal(err)
	}
	// GetConfigForClient must return a config that enforces revocation and
	// reloads the client CA (R7-Q5). It must not be nil.
	inner, err := cfg.GetConfigForClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	if inner == nil {
		t.Fatal("GetConfigForClient returned nil")
	}
	if inner.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("inner ClientAuth = %d", inner.ClientAuth)
	}
}

// makeBigRepo creates a bare repo with a commit that adds a file larger than
// the 256KiB render cap (R2-Q7) so blob/diff truncation can be exercised.
func makeBigRepo(t *testing.T, root, name string) {
	t.Helper()
	work := t.TempDir()
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
	big := bytes.Repeat([]byte("x"), maxRenderBytes+4096)
	if err := os.WriteFile(filepath.Join(work, "big.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-qm", "add big file")
	run(root, "clone", "-q", "--bare", work, filepath.Join(root, name+".git"))
}

func TestBlobTruncationAt256KiB(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeBigRepo(t, h.reposRoot, "big")
	rec := get(t, h, "/big/blob?ref=main&path=big.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("big blob = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "truncated to 256KiB") {
		t.Errorf("blob page missing truncation notice (R2-Q7)")
	}
	if !strings.Contains(body, "/big/raw?") {
		t.Errorf("blob page missing raw download link (R2-Q7)")
	}
	// The rendered page must not contain the full 256KiB+ file.
	if strings.Count(body, "xxxxx") > 0 && len(body) > maxRenderBytes*2 {
		t.Errorf("blob page too large: %d bytes", len(body))
	}
}

// --- blob syntax highlighting (Q5a/Q11/Q12a/Q15a) ---

func TestResolveBlobLexerByPath(t *testing.T) {
	cases := []struct {
		path string
		want string // lexer Config().Name
	}{
		{"Makefile", "Makefile"},
		{"Dockerfile", "Docker"}, // alias dockerfile is the hljs name
		{"bin/run.sh", "Bash"},
	}
	for _, tc := range cases {
		lexer := resolveBlobLexer(tc.path, []byte("x = 1\n"))
		if lexer == nil {
			t.Errorf("resolveBlobLexer(%q) = nil, want %s", tc.path, tc.want)
			continue
		}
		if got := lexer.Config().Name; got != tc.want {
			t.Errorf("resolveBlobLexer(%q) lexer = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestResolveBlobLexerUnknownReturnsNil(t *testing.T) {
	// An unknown extension whose content Chroma cannot sniff -> no highlight.
	lexer := resolveBlobLexer("notes.zzz", []byte("qwerty asdfgh zxcvbn 1234567890\n"))
	if lexer != nil {
		t.Errorf("resolveBlobLexer(unknown) = %q, want nil", lexer.Config().Name)
	}
}

func TestResolveBlobLexerBinary(t *testing.T) {
	// A NUL byte inside the first 8000 bytes marks the blob binary (Q15a).
	bin := append([]byte("#!/bin/sh\necho hi\n"), 0)
	lexer := resolveBlobLexer("run.sh", bin)
	if lexer != nil {
		t.Errorf("binary blob resolved lexer %q, want nil (Q15a)", lexer.Config().Name)
	}
	// A NUL beyond the scan window must not trip the binary check.
	late := append([]byte(strings.Repeat("x", binaryScanBytes)), 0)
	if lexer := resolveBlobLexer("run.sh", late); lexer == nil {
		t.Errorf("NUL beyond scan window still treated as binary")
	}
}

func TestResolveBlobLexerPathologicalLine(t *testing.T) {
	// A single line over the rune budget -> no highlight (Q11).
	long := strings.Repeat("x", maxPathologicalLine+1) + "\n"
	if lexer := resolveBlobLexer("long.py", []byte(long)); lexer != nil {
		t.Errorf("pathological-line blob resolved lexer %q, want nil (Q11)", lexer.Config().Name)
	}
	// A long-but-under-threshold line still resolves normally.
	fine := strings.Repeat("x", maxPathologicalLine-1) + "\n"
	if lexer := resolveBlobLexer("fine.py", []byte(fine)); lexer == nil {
		t.Errorf("under-threshold line resolved nil, want a lexer")
	}
}

func TestBlobServerModeHighlighting(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")

	// main.go resolves a Go lexer: Chroma renders a class-based
	// <pre class="chroma"> (WithClasses(true) — the CSP style-src 'self'
	// blocks inline styles) with an inline .ln line-number gutter, wrapped
	// in the .blob-hl frame.
	rec := get(t, h, "/repo1/blob?ref=main&path=main.go")
	if rec.Code != http.StatusOK {
		t.Fatalf("highlighted blob = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<pre class="chroma">`) {
		t.Errorf("highlighted blob missing Chroma class-based <pre>")
	}
	// Go lexer token classes: "package" -> .kn, "func" -> .kd, string -> .s.
	if !strings.Contains(body, `class="kn"`) {
		t.Errorf("highlighted blob missing token CSS classes")
	}
	if !strings.Contains(body, `class="ln"`) {
		t.Errorf("highlighted blob missing class-based line-number gutter")
	}
	if strings.Contains(body, "style=") {
		t.Errorf("highlighted blob must not emit inline styles (CSP style-src 'self')")
	}
	if !strings.Contains(body, `<div class="blob-hl">`) {
		t.Errorf("highlighted blob missing .blob-hl frame")
	}
	if !strings.Contains(body, `<link rel="stylesheet" href="/static/vendor/chroma/gruvbox.css">`) {
		t.Errorf("highlighted blob page missing gruvbox.css stylesheet link")
	}
	if strings.Contains(body, `<pre class="blob"`) {
		t.Errorf("highlighted blob must skip the .blob wrapper")
	}
}

func TestBlobServerModePlainUnlexed(t *testing.T) {
	h := testHandler(t, "server", nil)
	// A repo with a single non-markdown file that has no lexer and cannot be
	// sniffed: it must render as plain escaped <pre class="blob">.
	work := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(work, "notes.zzz"), []byte("<script>alert(1)</script>\nqwerty asdfgh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-qm", "add notes")
	run(h.reposRoot, "clone", "-q", "--bare", work, filepath.Join(h.reposRoot, "plain.git"))

	rec := get(t, h, "/plain/blob?ref=main&path=notes.zzz")
	if rec.Code != http.StatusOK {
		t.Fatalf("plain blob = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<pre class="blob">`) {
		t.Errorf("unlexed blob missing .blob wrapper")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("unlexed blob body not escaped (R2-Q6)")
	}
	if strings.Contains(body, "<pre style=") {
		t.Errorf("unlexed blob must not render Chroma output")
	}
}

func TestBlobClientModeLanguageHint(t *testing.T) {
	h := testHandler(t, "client", nil)
	makeRepo(t, h.reposRoot, "repo1")

	// Lexed blob carries the hljs hint (Go lexer name lowercased).
	rec := get(t, h, "/repo1/blob?ref=main&path=main.go")
	body := rec.Body.String()
	if !strings.Contains(body, `<pre class="blob hljs" data-lang="go">`) {
		t.Errorf("client blob missing data-lang hint")
	}
	if !strings.Contains(body, "func main") {
		t.Errorf("client blob missing escaped raw body")
	}
	// None mode (Q14): plain text exactly as before, no hints, no Chroma.
	h2 := testHandler(t, "none", nil)
	makeRepo(t, h2.reposRoot, "repo1")
	none := get(t, h2, "/repo1/blob?ref=main&path=main.go")
	nbody := none.Body.String()
	if strings.Contains(nbody, "<pre style=") || strings.Contains(nbody, "data-lang") {
		t.Errorf("none mode must not highlight or hint")
	}
	if !strings.Contains(nbody, `<pre class="blob">`) {
		t.Errorf("none mode missing plain .blob wrapper (Q14)")
	}
}

func TestBlobDownloadButton(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeRepo(t, h.reposRoot, "repo1")
	makeBigRepo(t, h.reposRoot, "big")
	cases := []struct {
		url  string
		path string
		repo string
	}{
		{"/repo1/blob?ref=main&path=main.go", "main.go", "repo1"},
		{"/repo1/blob?ref=main&path=README.md", "README.md", "repo1"},
		{"/big/blob?ref=main&path=big.txt", "big.txt", "big"},
	}
	for _, tc := range cases {
		rec := get(t, h, tc.url)
		if rec.Code != http.StatusOK {
			t.Fatalf("blob %s = %d", tc.path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `class="blob-download"`) {
			t.Errorf("blob %s missing download button", tc.path)
		}
		want := `href="/` + tc.repo + `/raw?ref=main&amp;path=` + tc.path + `"`
		if !strings.Contains(body, want) {
			t.Errorf("blob %s download link does not point to /raw (want %s)", tc.path, want)
		}
	}
}

func TestDiffTruncationAt256KiB(t *testing.T) {
	h := testHandler(t, "server", nil)
	makeBigRepo(t, h.reposRoot, "big")
	// Find the commit SHA of the "add big file" commit via the log page.
	logRec := get(t, h, "/big/log?ref=main")
	if logRec.Code != http.StatusOK {
		t.Fatalf("log = %d", logRec.Code)
	}
	// The commit hash links appear as /big/diff?ref=main&commit=<sha>
	idx := strings.Index(logRec.Body.String(), "/big/diff?ref=main&commit=")
	if idx < 0 {
		t.Fatal("no diff link in log page")
	}
	rest := logRec.Body.String()[idx+len("/big/diff?ref=main&commit="):]
	sha := rest[:40]
	rec := get(t, h, "/big/diff?commit="+sha)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "truncated to 256KiB") {
		t.Errorf("diff page missing truncation notice (R11-Q9)")
	}
	if !strings.Contains(body, "raw=1") {
		t.Errorf("diff page missing raw diff download link (R11-Q9)")
	}
	// The raw diff endpoint must serve the FULL diff with attachment
	// disposition (R11-Q9) — larger than the 256KiB page cap.
	raw := get(t, h, "/big/diff?commit="+sha+"&raw=1")
	if raw.Code != http.StatusOK {
		t.Fatalf("raw diff = %d", raw.Code)
	}
	if !strings.Contains(raw.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("raw diff missing Content-Disposition attachment (R11-Q9)")
	}
	if len(raw.Body.Bytes()) <= maxRenderBytes {
		t.Errorf("raw diff = %d bytes, want full diff > 256KiB (R11-Q9)", len(raw.Body.Bytes()))
	}
	// The rendered page must stay under the cap.
	if len(body) > maxRenderBytes*2 {
		t.Errorf("diff page too large: %d bytes", len(body))
	}
}
