package browse

import (
	"bytes"
	"html"
	"html/template"

	"github.com/ChronicCmposer/gitd/internal/version"
)

// escapeHTML escapes text for safe HTML insertion (server-side "none" and
// non-markdown README rendering, R2-Q6).
func escapeHTML(b []byte) []byte {
	return []byte(html.EscapeString(string(b)))
}

// pageData is the shared layout data: repo context, the render mode, and the
// version stamped into the global footer.
type pageData struct {
	Title      string
	Repo       string
	RenderMode string
	Version    string
	Body       template.HTML
}

// renderPage fills the shared layout with body and returns the HTML. The
// version is stamped here — one place — so every page carries the footer
// without threading it through the handlers.
func (h *Handler) renderPage(p pageData) ([]byte, error) {
	p.Version = version.String()
	var buf bytes.Buffer
	if err := layoutTmpl.Execute(&buf, p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// layoutTmpl is the Gruvbox-dark browse shell: a committed monospace identity,
// controlled density, and atmospheric layering (frontend-philosophy pillars).
// The app stylesheet is linked LAST so same-specificity overrides in
// style.css win over the vendored token themes (chroma gruvbox.css,
// highlight.js gruvbox-dark.css).
var layoutTmpl = template.Must(template.New("layout").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/static/vendor/chroma/gruvbox.css">
{{if eq .RenderMode "client"}}
<link rel="stylesheet" href="/static/vendor/highlight.js/gruvbox/gruvbox-dark.css">
{{end}}
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<header class="mast">
  <a class="wordmark" href="/">git.cmposer.cc</a>
  <span class="render-tag">{{.RenderMode}}</span>
</header>
<main class="stage">{{.Body}}</main>
<footer class="footer">gitd — personal git server · mTLS browse · {{.Version}}</footer>
{{if eq .RenderMode "client"}}
<script src="/static/vendor/marked/12.0.2/marked.min.js"></script>
<script src="/static/vendor/highlight.js/11.10.0/highlight.min.js"></script>
<script src="/static/render-client.js"></script>
{{end}}
</body>
</html>`))
