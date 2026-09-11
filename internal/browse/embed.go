package browse

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

// staticFS embeds the vendored client-render assets (marked + highlight.js,
// R10-Q8) plus the browse stylesheet and client render script. Everything is
// served under /static/ and covered by CSP 'self' (R3-Q6) — no CDN, no runtime
// fetch (R10-Q8).
//
//go:embed static
var staticFS embed.FS

// staticETags memoizes the SHA-256 ETag of each embedded asset. The embed
// files are immutable for the process lifetime, so a per-path hash is only
// ever computed once.
type staticETags struct {
	mu sync.Mutex
	m  map[string]string
}

func newStaticETags() *staticETags {
	return &staticETags{m: make(map[string]string)}
}

// get returns the quoted ETag for the asset named in fsys, computing and
// memoizing it on first use. ok is false when the asset cannot be read, in
// which case the caller must not advertise a validator.
func (t *staticETags) get(fsys fs.FS, name string) (etag string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if etag, ok := t.m[name]; ok {
		return etag, true
	}
	f, err := fsys.Open(name)
	if err != nil {
		return "", false
	}
	defer f.Close()
	hsh := sha256.New()
	if _, err := io.Copy(hsh, f); err != nil {
		return "", false
	}
	etag = `"` + hex.EncodeToString(hsh.Sum(nil)) + `"`
	t.m[name] = etag
	return etag, true
}

// handleStatic serves the embedded assets under /static/ (R10-Q8).
func (h *Handler) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Cache with a content-derived ETag: http.ServeContent answers a matching
	// If-None-Match with 304 (embedded files report zero ModTime, so
	// Last-Modified is never sent). Only existing assets get the headers — a
	// 404 from FileServer must not carry a misleading validator.
	if etag, ok := h.etags.get(sub, name); ok {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Header().Set("ETag", etag)
	}
	// Serve with a content type from the extension; embedded files are
	// trusted (vendored + authored), so this is not an XSS surface.
	path := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	path.ServeHTTP(w, r)
}
