package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBundle writes the given header lines (plus a fake pack terminator so
// the file is not empty) to a temp file and returns its path.
func writeBundle(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "b.bundle")
	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "\nPACK" // header terminator + pack marker
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBundleObjectFormat(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{name: "v2 is sha1", lines: []string{"# v2 git bundle", "deadbeef refs/heads/main"}, want: "sha1"},
		{name: "v3 sha256", lines: []string{"# v3 git bundle", "@object-format=sha256", strings.Repeat("a", 64) + " refs/heads/main"}, want: "sha256"},
		{name: "v3 sha1 explicit", lines: []string{"# v3 git bundle", "@object-format=sha1", "deadbeef refs/heads/main"}, want: "sha1"},
		{name: "v3 no capability defaults sha1", lines: []string{"# v3 git bundle", "deadbeef refs/heads/main"}, want: "sha1"},
		{name: "v3 other capabilities ignored", lines: []string{"# v3 git bundle", "@some-capability=1", "deadbeef refs/heads/main"}, want: "sha1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bundleObjectFormat(writeBundle(t, tc.lines...))
			if err != nil {
				t.Fatalf("bundleObjectFormat = %v, want %q", err, tc.want)
			}
			if got != tc.want {
				t.Errorf("bundleObjectFormat = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBundleObjectFormatFailsClosed(t *testing.T) {
	garbage := writeBundle(t, "not a bundle")
	badFormat := writeBundle(t, "# v3 git bundle", "@object-format=sha512")
	empty := filepath.Join(t.TempDir(), "empty.bundle")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
	}{
		{name: "garbage first line", path: garbage},
		{name: "unsupported object format capability", path: badFormat},
		{name: "empty file", path: empty},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bundleObjectFormat(tc.path)
			if err == nil {
				t.Fatal("bundleObjectFormat = nil error, want failure (fail closed)")
			}
		})
	}
}

func TestBundleObjectFormatMissingFile(t *testing.T) {
	_, err := bundleObjectFormat(filepath.Join(t.TempDir(), "nope.bundle"))
	if err == nil || !strings.Contains(err.Error(), "nope.bundle") {
		t.Fatalf("bundleObjectFormat(missing) = %v, want error naming the file", err)
	}
}
