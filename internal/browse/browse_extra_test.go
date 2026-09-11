package browse

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/serve"
)

func TestNewValidation(t *testing.T) {
	git := testGit(t)
	// Minimal valid pieces for the nil-argument checks.
	tlsCfg := &tls.Config{}
	fakeServe := &serve.Serve{}

	tests := []struct {
		name   string
		mutate func(cfg *Config)
		want   string
	}{
		{"nil git", func(cfg *Config) { cfg.Git = nil }, "git runner is required"},
		{"nil serve", func(cfg *Config) { cfg.Serve = nil }, "serve instance is required"},
		{"nil tls", func(cfg *Config) { cfg.TLS = nil }, "tls config is required"},
		{"bad render", func(cfg *Config) { cfg.Render = "bogus" }, "invalid render mode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Git: git, Serve: fakeServe, TLS: tlsCfg, Render: "server"}
			tc.mutate(&cfg)
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNewSucceeds(t *testing.T) {
	h, err := New(Config{
		Git:           testGit(t),
		Serve:         &serve.Serve{},
		TLS:           &tls.Config{},
		Render:        "server",
		HostAllowlist: []string{"git.cmposer.cc", "127.0.0.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.TLS() == nil {
		t.Error("TLS() = nil")
	}
}

func TestNormalizeHost(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "Git.Cmposer.CC", want: "git.cmposer.cc"},
		{in: " git.cmposer.cc ", want: "git.cmposer.cc"},
		{in: "git.cmposer.cc:443", want: "git.cmposer.cc"},
		{in: "127.0.0.1:8080", want: "127.0.0.1"},
		{in: "[::1]:443", want: "[::1]"}, // port stripped, brackets preserved
		{in: "", want: ""},
	}
	for _, tc := range tests {
		if got := normalizeHost(tc.in); got != tc.want {
			t.Errorf("normalizeHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHandleStatic(t *testing.T) {
	h, err := New(Config{Git: testGit(t), Serve: &serve.Serve{}, TLS: &tls.Config{}, Render: "server"})
	if err != nil {
		t.Fatal(err)
	}

	// Method not allowed.
	req := httptest.NewRequest(http.MethodPost, "/static/x", nil)
	rec := httptest.NewRecorder()
	h.handleStatic(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}

	// Missing name -> 404.
	req = httptest.NewRequest(http.MethodGet, "/static/", nil)
	rec = httptest.NewRecorder()
	h.handleStatic(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("empty name status = %d, want 404", rec.Code)
	}

	// Existing asset serves 200.
	req = httptest.NewRequest(http.MethodGet, "/static/render-client.js", nil)
	rec = httptest.NewRecorder()
	h.handleStatic(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("render-client.js status = %d, want 200", rec.Code)
	}

	// Missing asset -> 404 from FileServer.
	req = httptest.NewRequest(http.MethodGet, "/static/nope.js", nil)
	rec = httptest.NewRecorder()
	h.handleStatic(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing asset status = %d, want 404", rec.Code)
	}
}

func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		in      string
		host    string
		port    string
		wantErr bool
	}{
		{in: "1.2.3.4:51234", host: "1.2.3.4", port: "51234"},
		{in: "[::1]:443", host: "::1", port: "443"},
		{in: "no-port", host: "no-port"},
	}
	for _, tc := range tests {
		host, port, err := splitHostPort(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitHostPort(%q) = nil error", tc.in)
			}
			continue
		}
		if err != nil || host != tc.host || port != tc.port {
			t.Errorf("splitHostPort(%q) = %q,%q,%v; want %q,%q", tc.in, host, port, err, tc.host, tc.port)
		}
	}
}

func TestClientIPAndCN(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:51234"
	if got := clientIP(req); got != "1.2.3.4" {
		t.Errorf("clientIP = %q", got)
	}
	if got := clientCN(req); got != "unknown" {
		t.Errorf("clientCN without TLS = %q, want unknown", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "no-port"
	if got := clientIP(req); got != "no-port" {
		t.Errorf("clientIP(no-port) = %q", got)
	}
}

func TestWriteErrMapping(t *testing.T) {
	h, err := New(Config{
		Git: testGit(t), Serve: &serve.Serve{}, TLS: &tls.Config{},
		Render: "server", Log: testLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		err  error
		want int
		body string
	}{
		{serve.ErrBusy, http.StatusServiceUnavailable, "busy\n"},
		{serve.ErrShuttingDown, http.StatusServiceUnavailable, "shutting down\n"},
		{errBadPath, http.StatusNotFound, "not found\n"},
		{errors.New("boom"), http.StatusInternalServerError, "boom\n"},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		h.writeErr(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("writeErr(%v) = %d, want %d", tc.err, rec.Code, tc.want)
		}
		if body := rec.Body.String(); body != tc.body {
			t.Errorf("writeErr(%v) body = %q, want %q", tc.err, body, tc.body)
		}
	}
}

func TestRunListenError(t *testing.T) {
	// An invalid listen address fails fast with a clear error (R2-Q12
	// fail-fast startup).
	h, err := New(Config{Git: testGit(t), Serve: &serve.Serve{}, TLS: &tls.Config{}, Render: "server", Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	err = h.Run(context.Background(), "256.256.256.256:0")
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Run(bad addr) = %v, want listen error", err)
	}
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFragAndParentPath(t *testing.T) {
	// frag renders a known fragment and returns safe HTML (never raw data on
	// error).
	if got := frag("repoindex", nil); got == "" {
		t.Error("frag(repoindex) = empty")
	}
	if got := frag("no-such-fragment", nil); !strings.Contains(string(got), "template error") {
		t.Errorf("frag(unknown) = %q, want escaped template error", got)
	}
	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "a/b/c", want: "a/b"},
		{in: "repo", want: ""},
	}
	for _, tc := range tests {
		if got := parentPath(tc.in); got != tc.want {
			t.Errorf("parentPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFileBaseAndEscape(t *testing.T) {
	if got := fileBase("a/b/c.txt"); got != "c.txt" {
		t.Errorf("fileBase = %q", got)
	}
	if got := fileBase("top.txt"); got != "top.txt" {
		t.Errorf("fileBase(top) = %q", got)
	}
	if got := htmlEscape(`<script>`); got != "&lt;script&gt;" {
		t.Errorf("htmlEscape = %q", got)
	}
}

func TestRenderMarkdownServer(t *testing.T) {
	out, err := renderMarkdownServer([]byte("# Hi\n\n- one\n- two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "<h1>") || !strings.Contains(string(out), "<li>") {
		t.Errorf("markdown output = %q", out)
	}
	// Raw HTML is not rendered inline (XSS policy R2-Q6): a script tag is
	// escaped, not passed through.
	out, err = renderMarkdownServer([]byte("<script>alert(1)</script>"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "<script>") {
		t.Errorf("raw HTML passed through: %q", out)
	}
}

func TestRunFullTLS(t *testing.T) {
	// Full mTLS serve path: Run binds :0 with the real test PKI and serves
	// until ctx is canceled, then shuts down gracefully (R2-Q12, R5.x).
	pki := newTestPKI(t)
	tlsCfg, err := NewTLSConfig(pki.paths())
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{Git: testGit(t), Serve: &serve.Serve{}, TLS: tlsCfg, Render: "server", Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	// Reserve an ephemeral port on 127.0.0.1 so Run can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, addr) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
