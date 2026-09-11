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

// blobFormatter renders whole-file blobs: inline token styles (no CSS classes,
// so no stylesheet dependency server-side), Gruvbox colors, and a line-number
// gutter. It is deliberately separate from the markdown formatter in
// markdown.go, which must stay line-number-free.
var blobFormatter = html.New(
	html.WithClasses(false),
	html.WithLineNumbers(true),
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

// highlightBlobServer renders a lexed blob as Chroma HTML: a
// <pre style=...><code> block with a line-number gutter, safe to inject
// directly into the page. Callers pass a non-nil lexer only.
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