package browse

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/ChronicCmposer/gitd/internal/repo"
)

// errBadPath reports a path that failed canonicalization or containment.
var errBadPath = errors.New("invalid path")

// repoDir resolves a validated repo name to its canonical on-disk directory
// with the R5-Q3 symlink-escape defense: repo name allowlist, filepath.Clean,
// prefix containment, and an EvalSymlinks re-check against reposRoot.
func (h *Handler) repoDir(name string) (string, error) {
	if !repo.ValidName(name) {
		return "", fmt.Errorf("%w: invalid repo name", errBadPath)
	}
	dir := filepath.Join(h.reposRoot, name+".git")
	resolved, err := repo.RealpathUnder(h.reposRoot, dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errBadPath, err)
	}
	return resolved, nil
}

// cleanRepoPath normalizes an in-repo path (blob/tree) and rejects escapes
// ("..", leading "/") so it cannot address objects outside the repo. Git itself
// is defensive, but parse-don't-validate keeps the boundary strict (R5-Q3).
func cleanRepoPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	p = strings.TrimPrefix(p, "/")
	clean := path.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("%w: path escapes repo", errBadPath)
	}
	return clean, nil
}

// hasRef reports whether the repo has any refs (branches/tags). A zero-ref
// repo renders the empty-repository page (R13-Q7, 200 not 404).
func (h *Handler) hasRef(ctx context.Context, dir string) (bool, error) {
	out, err := h.git.RunIn(ctx, dir, "for-each-ref", "--format=%(refname)")
	if err != nil {
		return false, fmt.Errorf("enumerate refs: %w", err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// refs lists branches and tags for the ref selector, sorted with heads first.
func (h *Handler) refs(ctx context.Context, dir string) ([]string, error) {
	out, err := h.git.RunIn(ctx, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, fmt.Errorf("list refs: %w", err)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}

// defaultRef returns the repo HEAD symbolic ref short name, or the first
// existing ref, or "main" when HEAD is unborn and no refs exist. Used when no
// ?ref= is supplied.
func (h *Handler) defaultRef(ctx context.Context, dir string) (string, error) {
	out, err := h.git.RunIn(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "main", nil // unborn HEAD -> empty repo default
	}
	ref := strings.TrimSpace(string(out))
	// A fresh bare repo's unborn HEAD (e.g. "master") names a branch with no
	// commits once a client pushes a differently-named branch (e.g. "main"):
	// symbolic-ref succeeds for an unborn HEAD, so the name alone is not a
	// resolvable ref. Verify it resolves; if not, fall back to an existing
	// branch so commitLog/ls-tree don't 500 on an ambiguous argument.
	if resolved, verr := h.git.RunIn(ctx, dir, "rev-parse", "--verify", "--quiet", ref); verr != nil || strings.TrimSpace(string(resolved)) == "" {
		refs, rerr := h.refs(ctx, dir)
		if rerr == nil && len(refs) > 0 {
			return refs[0], nil
		}
		return "main", nil
	}
	return ref, nil
}

// commitLog returns up to pageSizeCommits commits for ref, skipping skip.
func (h *Handler) commitLog(ctx context.Context, dir, ref string, skip int) ([]string, error) {
	args := []string{"log", "--format=%H%x00%h%x00%an%x00%ad%x00%s", "--date=short", "--skip", fmt.Sprint(skip), "-" + fmt.Sprint(pageSizeCommits), ref}
	out, err := h.git.RunIn(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("commit log: %w", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

// treeEntries lists the entries under path at ref (500/page, R2-Q7). Each
// returned line is "<mode> <type> <sha>\t<name>" (git ls-tree -z), so callers
// can distinguish trees from blobs.
func (h *Handler) treeEntries(ctx context.Context, dir, ref, path string) ([]string, error) {
	lsPath := ref
	if path != "" {
		lsPath = ref + ":" + path
	}
	out, err := h.git.RunIn(ctx, dir, "ls-tree", "-z", lsPath)
	if err != nil {
		return nil, fmt.Errorf("list tree: %w", err)
	}
	return splitNul(out), nil
}

// blobAt returns the raw bytes of path at ref. Paths may contain slashes; the
// caller has already sanitized them via cleanRepoPath.
func (h *Handler) blobAt(ctx context.Context, dir, ref, path string) ([]byte, error) {
	spec := ref + ":" + path
	return h.git.RunIn(ctx, dir, "show", spec)
}

// diffOf returns the FULL diff for commit (parent..commit) with a truncation
// flag. The caller truncates the page display; the raw=1 endpoint serves the
// full diff as an attachment (R11-Q9).
func (h *Handler) diffOf(ctx context.Context, dir, commit string) ([]byte, bool, error) {
	out, err := h.git.RunIn(ctx, dir, "show", "--format=fuller", "--stat", "--patch", commit)
	if err != nil {
		return nil, false, fmt.Errorf("commit diff: %w", err)
	}
	return out, len(out) > maxRenderBytes, nil
}

// splitNul splits NUL-terminated output, dropping a trailing empty element.
func splitNul(b []byte) []string {
	raw := strings.Split(string(b), "\x00")
	var out []string
	for _, s := range raw {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
