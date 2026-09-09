package browse

import (
	"html"
	"html/template"
	"strings"
)

// parseCommits parses the \x00-separated commit log lines (format
// %H%x00%h%x00%an%x00%ad%x00%s) into items.
func parseCommits(lines []string) []commitItem {
	out := make([]commitItem, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) < 5 {
			continue
		}
		out = append(out, commitItem{Hash: parts[0], Short: parts[1], Author: parts[2], Date: parts[3], Msg: parts[4]})
	}
	return out
}

// parseTree parses git ls-tree -z lines ("<mode> <type> <sha>\t<name>") into
// entries, joining each name onto the current directory base.
func parseTree(entries []string, base string) []treeEntry {
	out := make([]treeEntry, 0, len(entries))
	for _, line := range entries {
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(line[:tab])
		if len(meta) < 2 {
			continue
		}
		name := line[tab+1:]
		typ := meta[1]
		full := name
		if base != "" {
			full = base + "/" + name
		}
		out = append(out, treeEntry{Type: typ, Name: name, Path: full})
	}
	return out
}

// htmlEscape escapes text for safe HTML insertion (R2-Q6).
func htmlEscape(s string) string { return html.EscapeString(s) }

// fileBase returns the final path segment of an in-repo path.
func fileBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// highlightDiff renders a raw diff as escaped HTML with add/del/meta line
// classes (Gruvbox red/green/gray). The output is already escaped, so the
// template inserts it unescaped.
func highlightDiff(data []byte) template.HTML {
	var b strings.Builder
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for _, line := range lines {
		cls := ""
		switch {
		case strings.HasPrefix(line, "+"):
			cls = "add"
		case strings.HasPrefix(line, "-"):
			cls = "del"
		case strings.HasPrefix(line, "@@"),
			strings.HasPrefix(line, "diff "),
			strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "---"),
			strings.HasPrefix(line, "+++"):
			cls = "meta"
		}
		if cls != "" {
			b.WriteString(`<span class="` + cls + `">` + html.EscapeString(line) + `</span>` + "\n")
		} else {
			b.WriteString(html.EscapeString(line) + "\n")
		}
	}
	return template.HTML(b.String())
}
