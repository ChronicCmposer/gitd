// Package gitenv pins the deterministic environment for every gitd git exec
// (R9-Q4): LC_ALL=C, TZ=UTC, GIT_CONFIG_GLOBAL=/dev/null (user config ignored),
// GIT_CONFIG_SYSTEM=/etc/gitconfig (image system config pinned and enforced:
// fsckObjects, hooksPath, safe.directory, init.defaultBranch), GIT_TERMINAL_PROMPT=0,
// a writable HOME, and PATH from the image. Output parsing never depends on
// caller locale or stray GIT_* environment variables.
//
// GIT_DEFAULT_HASH is pinned from the configured object format (sha1 default,
// sha256 opt-in) via Runner.WithObjectFormat; it sets the hash algorithm used
// by `git init` for NEW repos. It is deliberately omitted when no format is
// configured, relying on git's built-in sha1 default.
package gitenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner builds git exec commands with the fixed environment (R9-Q4). gitBin
// is the configured git binary (config.git_binary); home must be a writable
// directory (git needs it for caches/credentials even with the global config
// disabled). path is the image PATH, passed verbatim. The system gitconfig at
// /etc/gitconfig is pinned via GIT_CONFIG_SYSTEM and enforced on every exec
// (fsckObjects, hooksPath, safe.directory, init.defaultBranch). objectFormat,
// when set, pins GIT_DEFAULT_HASH so `git init` creates new repos in that
// object format (sha1 default, sha256 opt-in); empty means the env var is
// omitted and git's built-in sha1 default applies.
type Runner struct {
	gitBin       string
	home         string
	path         string
	objectFormat string
}

// NewRunner returns a Runner for the given git binary, writable HOME, and
// PATH. An empty path falls back to the current process PATH (dev/test).
// The returned Runner carries no GIT_DEFAULT_HASH: git's built-in sha1
// default applies unless WithObjectFormat is called.
func NewRunner(gitBin, home, path string) *Runner {
	if path == "" {
		path = envPath()
	}
	return &Runner{gitBin: gitBin, home: home, path: path}
}

// WithObjectFormat returns a copy of the Runner that pins GIT_DEFAULT_HASH to
// f (sha1 or sha256) for every exec, so `git init` creates new repos in that
// object format. The receiver is never mutated. A non-sha1/sha256 (or empty)
// f is ignored: the copy keeps the receiver's object format (empty means git's
// built-in sha1 default). Callers pass the validated config object_format
// value, so the invalid case is defense-in-depth only.
func (r *Runner) WithObjectFormat(f string) *Runner {
	cp := *r
	if f == "sha1" || f == "sha256" {
		cp.objectFormat = f
	}
	return &cp
}

func envPath() string {
	// os.Getenv at construction time keeps tests deterministic: the caller
	// passes PATH explicitly when it matters.
	return os.Getenv("PATH")
}

// Git returns an exec.Cmd running the git binary with the fixed environment.
func (r *Runner) Git(ctx context.Context, args ...string) *exec.Cmd {
	return r.command(ctx, r.gitBin, args...)
}

// Command returns an exec.Cmd running the named git-family executable
// (git-upload-pack, git-receive-pack, ...) with the fixed environment.
func (r *Runner) Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	return r.command(ctx, name, args...)
}

func (r *Runner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = r.env()
	return cmd
}

// env returns the pinned environment (R9-Q4). PATH is included last so a
// caller-supplied PATH never shadows it. GIT_DEFAULT_HASH is included only
// when an object format is configured (WithObjectFormat), so `git init`
// creates new repos in the configured format; the flag --object-format on a
// command would take precedence, but no caller passes it.
func (r *Runner) env() []string {
	env := []string{
		"LC_ALL=C",
		"TZ=UTC",
		"GIT_CONFIG_SYSTEM=/etc/gitconfig",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=" + r.home,
	}
	if r.objectFormat != "" {
		env = append(env, "GIT_DEFAULT_HASH="+r.objectFormat)
	}
	env = append(env, "PATH="+r.path)
	return env
}

// Run executes the git binary with args and returns stdout. On failure the
// returned error includes git's stderr, trimmed, for a clear hook message.
func (r *Runner) Run(ctx context.Context, args ...string) ([]byte, error) {
	return r.run(ctx, r.gitBin, "", args...)
}

// RunIn executes the git binary with args in dir.
func (r *Runner) RunIn(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return r.run(ctx, r.gitBin, dir, args...)
}

func (r *Runner) run(ctx context.Context, name, dir string, args ...string) ([]byte, error) {
	cmd := r.command(ctx, name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return out, nil
}
