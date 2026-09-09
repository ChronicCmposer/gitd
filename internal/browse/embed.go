package browse

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// staticFS embeds the vendored client-render assets (marked + highlight.js,
// R10-Q8) plus the browse stylesheet and client render script. Everything is
// served under /static/ and covered by CSP 'self' (R3-Q6) — no CDN, no runtime
// fetch (R10-Q8).
//
//go:embed static
var staticFS embed.FS

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
	// Serve with a content type from the extension; embedded files are
	// trusted (vendored + authored), so this is not an XSS surface.
	path := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	path.ServeHTTP(w, r)
}
