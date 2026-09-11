package browse

import (
	"bytes"
	"fmt"

	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
)

// mdParser renders the GFM subset (tables, strikethrough, autolinks, task
// lists, R5.3) with server-side syntax highlighting. Raw HTML is never
// rendered inline: goldmark runs with unsafe=false (R2-Q6), so any embedded
// HTML is escaped rather than emitted as markup. The chroma formatter uses
// the Gruvbox dark style for code blocks, matching the browse theme.
//
// The formatter emits CSS classes (html.WithClasses(true)) instead of inline
// style attributes: the app's CSP is style-src 'self' (no unsafe-inline), so
// inline styles would be blocked. The matching same-origin stylesheet is
// /static/vendor/chroma/gruvbox.css, linked in the page shell.
var mdParser = goldmark.New(
	goldmark.WithExtensions(extension.GFM, highlighting.NewHighlighting(
		highlighting.WithStyle("gruvbox"),
		highlighting.WithFormatOptions(
			html.WithClasses(true),
			html.WithLineNumbers(false),
		),
	)),
)

// renderMarkdownServer converts markdown to safe HTML (GFM subset + Gruvbox
// highlighted code). It caps the output at maxRenderBytes defensively; the
// caller truncates the input first for the visible-size cap (R2-Q7).
func renderMarkdownServer(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := mdParser.Convert(src, &buf); err != nil {
		return nil, fmt.Errorf("render markdown: %w", err)
	}
	out := buf.Bytes()
	if len(out) > maxRenderBytes {
		out = out[:maxRenderBytes]
	}
	return out, nil
}
