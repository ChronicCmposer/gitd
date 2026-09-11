package browse

import (
	"bytes"
	"html/template"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Non-markdown blob highlighting (Q5a/Q11/Q12a/Q15a): the "server" render mode
// lexes the whole file with Chroma, the "client" mode stamps a language hint
// for the vendored highlight.js, and binary / pathological / unmatched files
// always fall back to plain escaped text.

// binaryScanBytes is how many leading bytes are scanned for a NUL byte when
// deciding whether a blob is binary (Q15a). git's own heuristics use a small
// window; 8000 bytes catches NUL-padded headers without scanning whole blobs.
const binaryScanBytes = 8000

// maxPathologicalLine is the rune budget for a single line of blob content.
// Lines longer than this (minified bundles, data dumps) would force Chroma to
// build one enormous token span, so the blob is left unhighlighted (Q11).
const maxPathologicalLine = 10000

// blobFormatter renders whole-file blobs: token CSS classes (html.WithClasses
// true — the app's CSP is style-src 'self' with no unsafe-inline, so inline
// style attributes would be blocked; the matching same-origin stylesheet is
// /static/vendor/chroma/gruvbox.css), Gruvbox colors, and a line-number
// gutter. It is deliberately separate from the markdown formatter in
// markdown.go, which must stay line-number-free.
//
// Line numbers use Chroma's inline .ln spans (html.LineNumbersInTable(false),
// the default in this version) rather than the .lnt table layout: the inline
// form emits a single <pre class="chroma"> that slots straight into the
// .blob-hl frame, whose .blob-hl pre rule already supplies the padding and
// white-space: pre. A table would nest extra <pre> cells and need its own
// frame resets. The gutter is right-aligned in style.css via
// .blob-hl .chroma .ln.
var blobFormatter = html.New(
	html.WithClasses(true),
	html.WithLineNumbers(true),
	html.LineNumbersInTable(false),
)

// resolveBlobLexer decides whether a non-markdown blob deserves syntax
// highlighting. It returns the matching Chroma lexer, or nil when the blob is
// binary, contains a pathological over-long line, or no lexer matches by name
// or by content sniffing. The caller uses nil to mean "plain text".
func resolveBlobLexer(path string, data []byte) chroma.Lexer {
	if isBinary(data) {
		return nil
	}
	if hasPathologicalLine(data) {
		return nil
	}
	if lexer := lexers.Get(filepath.Base(path)); lexer != nil {
		return lexer
	}
	return lexers.Analyse(string(data))
}

// isBinary reports whether the content is binary by scanning the first
// binaryScanBytes for a NUL byte (Q15a).
func isBinary(data []byte) bool {
	n := min(len(data), binaryScanBytes)
	return bytes.IndexByte(data[:n], 0) >= 0
}

// hasPathologicalLine reports whether any single line exceeds
// maxPathologicalLine runes (Q11).
func hasPathologicalLine(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if len([]rune(line)) > maxPathologicalLine {
			return true
		}
	}
	return false
}

// highlightBlobServer renders a lexed blob as Chroma HTML: a class-based
// <pre class="chroma"><code> block with an inline .ln line-number gutter,
// safe to inject directly into the page. The caller wraps it in the
// .blob-hl frame. Callers pass a non-nil lexer only.
func highlightBlobServer(lexer chroma.Lexer, data []byte) (template.HTML, error) {
	it, err := lexer.Tokenise(nil, string(data))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := blobFormatter.Format(&buf, styles.Get("gruvbox"), it); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}
