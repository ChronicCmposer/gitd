package mirror

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// bundleObjectFormat reads the header of the git bundle at path and returns
// the object format it encodes ("sha1" or "sha256"), with no repository
// needed. The format drives `git init --object-format=<format>` before
// bundle verify/unbundle, so a restore always rebuilds the repo in the
// bundle's own format (the service accepts both SHA-1 and SHA-256 repos).
//
// The header is plain text and small; the parser stops at the first blank
// line (the header terminator) so it never scans into the binary pack.
//
// Recognized headers (fail closed on anything else):
//
//	"# v2 git bundle"                       -> sha1 (v2 can never be sha256)
//	"# v3 git bundle" + "@object-format=sha256" -> sha256
//	"# v3 git bundle" + "@object-format=sha1"   -> sha1
//	"# v3 git bundle" with no object-format     -> sha1 (v3 default)
//	anything else                              -> error (unrecognized/corrupt)
func bundleObjectFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("bundle header: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// The header is a handful of short lines; a generous cap still bounds
	// work and cannot be hit by a real header (a binary line would be a
	// corrupt bundle, which fails closed below or in git itself).
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)

	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("bundle header %s: read: %w", path, err)
		}
		return "", fmt.Errorf("bundle header %s: empty file", path)
	}
	switch firstLine := sc.Text(); firstLine {
	case "# v2 git bundle":
		return "sha1", nil
	case "# v3 git bundle":
		// Fall through to the capability scan below.
	default:
		return "", fmt.Errorf("bundle header %s: unrecognized first line %q", path, firstLine)
	}

	// v3 capability lines are "@"-prefixed and appear before the first blank
	// line; ref lines (and anything else) are skipped.
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break // end of header
		}
		if !strings.HasPrefix(line, "@") {
			continue // ref line, not a capability
		}
		switch line {
		case "@object-format=sha256":
			return "sha256", nil
		case "@object-format=sha1":
			return "sha1", nil
		default:
			if strings.HasPrefix(line, "@object-format=") {
				return "", fmt.Errorf("bundle header %s: unsupported object format capability %q", path, line)
			}
			// Unknown non-object-format capability: ignore.
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("bundle header %s: read: %w", path, err)
	}
	// A v3 bundle without an object-format capability defaults to sha1.
	return "sha1", nil
}
