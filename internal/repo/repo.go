// Package repo validates git repo names against the allowlist (R2-Q1) and
// checks bare-repo layout. It is shared by the ssh gateway (3.1), notify's
// cwd defense-in-depth (R9-Q6), and the mirror CLI's key construction.
package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// validName matches the strict allowlist [A-Za-z0-9][A-Za-z0-9._-]{0,99}
// (R2-Q1). Compiled once; the package is read-only after init.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidName reports whether name matches the repo-name allowlist (R2-Q1).
func ValidName(name string) bool { return validName.MatchString(name) }

// Normalize strips ONE optional trailing ".git" from a repo name/path and
// returns the bare name (R10-Q4). "foo.git" -> "foo", "foo" -> "foo".
func Normalize(s string) string {
	if n, ok := stripDotGit(s); ok {
		return n
	}
	return s
}

func stripDotGit(s string) (string, bool) {
	if len(s) > 4 && s[len(s)-4:] == ".git" {
		return s[:len(s)-4], true
	}
	return s, false
}

// ErrNotBare reports a directory that does not look like a bare repo.
var ErrNotBare = errors.New("not a bare git repository")

// IsBareRepo reports whether dir has the bare-repo layout: HEAD, objects/,
// and refs/ (R9-Q6).
func IsBareRepo(dir string) (bool, error) {
	for _, name := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, fmt.Errorf("stat %s: %w", filepath.Join(dir, name), err)
		}
	}
	return true, nil
}

// ListBare returns the bare repo names under root (dirs ending ".git" that
// pass the bare-repo layout check), sorted ascending. The serve verify loop
// uses it to enumerate repos (R8-Q3).
func ListBare(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list repos under %s: %w", root, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".git") {
			continue
		}
		name := Normalize(e.Name())
		if !ValidName(name) {
			continue
		}
		ok, err := IsBareRepo(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", e.Name(), err)
		}
		if ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// RealpathUnder resolves dir (EvalSymlinks) and verifies it stays under root
// (R9-Q6, R5-Q3 pattern). It returns the resolved absolute path.
func RealpathUnder(root, dir string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	clean := filepath.Clean(resolved)
	if clean != absRoot && !hasPrefix(clean, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes %s", clean, absRoot)
	}
	return clean, nil
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }
