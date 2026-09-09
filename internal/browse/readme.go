package browse

import (
	"context"
	"fmt"
	"strings"
)

// readmeNames are the README filenames probed at the repo root.
var readmeNames = []string{"README.md", "README", "readme.md", "Readme.md"}

// readmeResult is the outcome of README lookup + render.
type readmeResult struct {
	found      bool
	name       string
	rendered   []byte // server: safe HTML; client: raw markdown; none: escaped text
	truncated  bool
	isMarkdown bool
}

// findReadme returns the first README blob at ref's tree root, or found=false.
func (h *Handler) findReadme(ctx context.Context, dir, ref string) (name string, content []byte, found bool, err error) {
	entries, err := h.treeEntries(ctx, dir, ref, "")
	if err != nil {
		return "", nil, false, err
	}
	for _, entry := range entries {
		// ls-tree lines are "<mode> <type> <sha>\t<name>"; extract the name.
		n := entry
		if i := strings.IndexByte(entry, '\t'); i >= 0 {
			n = entry[i+1:]
		}
		for _, want := range readmeNames {
			if n == want {
				data, err := h.blobAt(ctx, dir, ref, n)
				if err != nil {
					return "", nil, false, fmt.Errorf("read README: %w", err)
				}
				return n, data, true, nil
			}
		}
	}
	return "", nil, false, nil
}

// renderReadme builds the README block for the repo home page according to the
// render toggle (server|client|none, R3-Q4):
//
//   - server: goldmark (GFM) -> safe HTML with Gruvbox-highlighted code.
//   - client: raw markdown served to the browser, rendered by vendored marked
//     (R10-Q8); the page template escapes it and the client script handles the
//     HTML, so raw README HTML never renders inline server-side (R2-Q6).
//   - none: the raw bytes shown as plain text (escaped).
//
// Input and output are capped at maxRenderBytes (R2-Q7); truncation is flagged
// so the template can surface a download link.
func (h *Handler) renderReadme(ctx context.Context, dir, ref string) (readmeResult, error) {
	name, data, found, err := h.findReadme(ctx, dir, ref)
	if err != nil || !found {
		return readmeResult{found: found}, err
	}
	truncated := len(data) > maxRenderBytes
	if truncated {
		data = data[:maxRenderBytes]
	}
	isMarkdown := strings.HasSuffix(strings.ToLower(name), ".md")

	res := readmeResult{found: true, name: name, truncated: truncated, isMarkdown: isMarkdown}
	switch h.render {
	case "server":
		if isMarkdown {
			rendered, err := renderMarkdownServer(data)
			if err != nil {
				return res, err
			}
			res.rendered = rendered
			return res, nil
		}
		// Non-markdown README (e.g. plain README) renders as escaped text.
		res.rendered = escapeHTML(data)
		return res, nil
	case "client":
		// marked receives the raw markdown; the page template HTML-escapes it
		// before insertion, so it is safe. Non-markdown shows escaped text.
		res.rendered = data
		return res, nil
	default: // none
		res.rendered = escapeHTML(data)
		return res, nil
	}
}
