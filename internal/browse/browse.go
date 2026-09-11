// Package browse is the read-only :443 mTLS web UI (Phase 5): routing and
// handlers only — render actions submit into the serve actions channel
// (R11-Q2, R11-Q6). It serves repo pages (5.2), README rendering (5.3), the
// vendored client-render assets at /static/ (R10-Q8), and mounts the socket
// control endpoints (POST /v1/bundle + /v1/deliver) behind the same mux.
//
// Security posture (R2-Q6, R3-Q6, R4-Q8/Q9, R5-Q3, R7-Q5, R8-Q5, R9-Q8):
// mTLS with TLS 1.3 only, per-handshake reload of server cert + client-CA
// pool + revocation list, host-header allowlist, security headers on every
// response, and an slog Info audit line per request.
package browse

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/serve"
)

// Resource caps (R2-Q7, R11-Q9, R13-Q9): render/diff 256KiB, body 32MiB,
// header cap 1MiB, idle 2m, ReadHeaderTimeout 10s, per-git-call 10s.
const (
	maxRenderBytes = 256 << 10 // 256KiB (R2-Q7, R11-Q9)
	maxBodyBytes   = 32 << 20  // 32MiB (R13-Q9)
	maxHeaderBytes = 1 << 20   // 1MiB (R13-Q9)
	idleTimeout    = 2 * time.Minute
	readHeaderTO   = 10 * time.Second
	gitCallTimeout = 10 * time.Second // per-git-call (R11-Q2)

	pageSizeCommits = 50  // (R2-Q7)
	pageSizeTree    = 500 // (R2-Q7)
)

// Config wires the browse handler. All fields are required.
type Config struct {
	ReposRoot     string
	Git           *gitenv.Runner
	Render        string       // server | client | none (R3-Q4)
	Serve         *serve.Serve // render actions submit into this channel (R11-Q2)
	HostAllowlist []string     // mismatch -> 400 (R8-Q5)
	TLS           *tls.Config  // re-loading mTLS config (R5-Q6, R7-Q5)
	Log           *slog.Logger
}

// Handler is the :443 mux and its render dependencies. It is safe to serve
// concurrently; render actions serialize through the serve worker.
type Handler struct {
	reposRoot     string
	git           *gitenv.Runner
	render        string
	serve         *serve.Serve
	hostAllowlist map[string]bool
	tls           *tls.Config
	log           *slog.Logger
	etags         *staticETags // memoized content hashes for /static/ assets
}

// New returns a Handler ready to serve.
func New(cfg Config) (*Handler, error) {
	if cfg.Git == nil {
		return nil, errors.New("browse: git runner is required")
	}
	if cfg.Serve == nil {
		return nil, errors.New("browse: serve instance is required")
	}
	if cfg.TLS == nil {
		return nil, errors.New("browse: tls config is required")
	}
	switch cfg.Render {
	case "server", "client", "none":
	default:
		return nil, fmt.Errorf("browse: invalid render mode %q", cfg.Render)
	}
	allow := make(map[string]bool, len(cfg.HostAllowlist))
	for _, h := range cfg.HostAllowlist {
		allow[normalizeHost(h)] = true
	}
	return &Handler{
		reposRoot:     cfg.ReposRoot,
		git:           cfg.Git,
		render:        cfg.Render,
		serve:         cfg.Serve,
		hostAllowlist: allow,
		tls:           cfg.TLS,
		log:           cfg.Log,
		etags:         newStaticETags(),
	}, nil
}

// TLS returns the mTLS config for the :443 listener.
func (h *Handler) TLS() *tls.Config { return h.tls }

// Run serves the :443 mTLS browse UI until ctx is canceled, then shuts down
// gracefully (R2-Q12). It shares the serve actions channel for renders.
func (h *Handler) Run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("browse: listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           h.Mux(),
		ReadHeaderTimeout: readHeaderTO,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         h.tls,
	}
	h.log.Info("browse server listening", "addr", addr)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ServeTLS(ln, "", "") }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("browse: server: %w", err)
	case <-ctx.Done():
	}

	h.log.Info("browse server shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), readHeaderTO)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		h.log.Warn("browse shutdown", "error", err)
	}
	return nil
}

// Mux builds the full :443 router: browse pages + /static/ + the socket
// control endpoints (R5.x). All routes are wrapped in the security-header,
// host-allowlist, and audit middlewares. /static/ is served by a wrapper
// (not a ServeMux pattern) because it would conflict with /{repo}/... paths.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleRepoIndex)
	mux.HandleFunc("/{repo}", h.handleRepoHome)
	mux.HandleFunc("/{repo}/log", h.handleLog)
	mux.HandleFunc("/{repo}/tree", h.handleTree)
	mux.HandleFunc("/{repo}/blob", h.handleBlob)
	mux.HandleFunc("/{repo}/raw", h.handleRaw)
	mux.HandleFunc("/{repo}/diff", h.handleDiff)
	// Socket control endpoints behind the same mux (R13-Q9 discipline).
	mux.Handle("/v1/bundle", h.serve.SocketHandler())
	mux.Handle("/v1/deliver", h.serve.SocketHandler())

	// Audit outermost so even rejected (host-allowlist 400) requests are
	// logged (R4-Q9); security headers innermost so every response carries
	// them (R3-Q6).
	return h.withAudit(h.withSecurity(h.withHostAllowlist(h.withStatic(mux))))
}

// withStatic intercepts /static/ paths and serves the embedded assets before
// the mux (R10-Q8). CSP 'self' + nosniff govern the responses (R3-Q6).
func (h *Handler) withStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") {
			h.handleStatic(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// renderAction runs fn inside the serve worker and returns its output,
// waiting up to the client's deadline. A full actions channel replies with
// ErrBusy (503) via the caller (R12-Q1); per-git-call timeouts bound fn.
func (h *Handler) renderAction(r *http.Request, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	reply := make(chan struct {
		out []byte
		err error
	}, 1)
	act := func(*serve.Serve) {
		ctx, cancel := context.WithTimeout(context.Background(), gitCallTimeout)
		defer cancel()
		out, err := fn(ctx)
		reply <- struct {
			out []byte
			err error
		}{out, err}
	}
	if err := h.serve.Submit(act); err != nil {
		return nil, err
	}
	select {
	case res := <-reply:
		return res.out, res.err
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
}

// normalizeHost lowercases and strips a trailing port for comparison.
func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		// host:port, but IPv6 [::1]:443 has the colon after ']' only.
		if strings.HasPrefix(host, "[") {
			if j := strings.Index(host, "]:"); j >= 0 {
				host = host[:j+1]
			}
		} else {
			host = host[:i]
		}
	}
	return strings.TrimSuffix(host, ".")
}
