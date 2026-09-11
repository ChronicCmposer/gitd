package browse

import (
	"bytes"
	"html"
	"html/template"
)

// escapeHTML escapes text for safe HTML insertion (server-side "none" and
// non-markdown README rendering, R2-Q6).
func escapeHTML(b []byte) []byte {
	return []byte(html.EscapeString(string(b)))
}

// pageData is the shared layout data: repo context and the render mode.
type pageData struct {
	Title      string
	Repo       string
	RenderMode string
	Body       template.HTML
}

// renderPage fills the shared layout with body and returns the HTML.
func (h *Handler) renderPage(p pageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := layoutTmpl.Execute(&buf, p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// layoutTmpl is the Gruvbox-dark browse shell: a committed monospace identity,
// controlled density, and atmospheric layering (frontend-philosophy pillars).
var layoutTmpl = template.Must(template.New("layout").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/static/style.css">
<link rel="stylesheet" href="/static/vendor/chroma/gruvbox.css">
{{if eq .RenderMode "client"}}
<link rel="stylesheet" href="/static/vendor/highlight.js/gruvbox/gruvbox-dark.css">
{{end}}
</head>
<body>
<header class="mast">
  <a class="wordmark" href="/">git.cmposer.cc</a>
  <span class="render-tag">{{.RenderMode}}</span>
</header>
<main class="stage">{{.Body}}</main>
{{if eq .RenderMode "client"}}
<script src="/static/vendor/marked/12.0.2/marked.min.js"></script>
<script src="/static/vendor/highlight.js/11.10.0/highlight.min.js"></script>
<script src="/static/render-client.js"></script>
{{end}}
</body>
</html>`))
