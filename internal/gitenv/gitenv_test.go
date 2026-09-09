package gitenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin writes an executable script at dir/git that prints its env + args;
// it is the deterministic stand-in for the real git binary (R9-Q4).
func fakeBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// printEnvScript emits every pinned var (R9-Q4) plus argv, one per line, then
// exits with $1 when set. Used to assert the exact environment construction.
const printEnvScript = `for v in LC_ALL TZ GIT_CONFIG_GLOBAL GIT_TERMINAL_PROMPT HOME PATH; do eval echo "$v=\$$v"; done; echo "args: $*"; [ -n "${EXIT_CODE:-}" ] && exit "$EXIT_CODE"; exit 0`

func TestNewRunnerDefaultsPath(t *testing.T) {
	r := NewRunner("/usr/bin/git", "/tmp/home", "")
	if r.path != os.Getenv("PATH") {
		t.Fatalf("path = %q, want process PATH", r.path)
	}
}

func TestEnvPinned(t *testing.T) {
	bin := fakeBin(t, printEnvScript)
	r := NewRunner(bin, "/writable/home", "/image/bin:/usr/bin")

	cmd := r.Git(context.Background(), "rev-parse", "--is-bare-repository")
	if cmd == nil {
		t.Fatal("Git() returned nil cmd")
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"LC_ALL=C",
		"TZ=UTC",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"HOME=/writable/home",
		"PATH=/image/bin:/usr/bin", // caller PATH, included last (R9-Q4)
		"args: rev-parse --is-bare-repository",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("env output missing %q; got:\n%s", want, got)
		}
	}
}

func TestCommandUsesGitFamilyName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	bin := filepath.Join(dir, "git")
	writeScript := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeScript("git", `echo "name: git"; exit 0`)
	// The fake git-upload-pack reports which name was exec'd, so we can prove
	// Command passes the git-family name verbatim (R2-Q1 dash+space forms).
	writeScript("git-upload-pack", `echo "name: git-upload-pack"; exit 0`)
	r := NewRunner(bin, "/home", dir)

	cmd := r.Command(context.Background(), "git-upload-pack", "/srv/git/r.git")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "name: git-upload-pack") {
		t.Errorf("Command did not exec git-upload-pack; got:\n%s", out)
	}
}

func TestRunReturnsStdout(t *testing.T) {
	bin := fakeBin(t, `echo "hello from git"`)
	r := NewRunner(bin, "/home", "/usr/bin")

	out, err := r.Run(context.Background(), "rev-parse")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello from git" {
		t.Fatalf("Run stdout = %q, want %q", got, "hello from git")
	}
}

func TestRunErrorIncludesStderr(t *testing.T) {
	bin := fakeBin(t, `echo "fatal: not a git repository" >&2; exit 1`)
	r := NewRunner(bin, "/home", "/usr/bin")

	if _, err := r.Run(context.Background(), "rev-parse"); err == nil {
		t.Fatal("Run() = nil error, want failure")
	} else if !strings.Contains(err.Error(), "fatal: not a git repository") {
		t.Errorf("error = %q, want stderr included", err)
	}
}

func TestRunErrorFallsBackToExecError(t *testing.T) {
	// A binary that dies without writing stderr: the error should still carry
	// the exec error text so the hook message is never empty.
	bin := fakeBin(t, `exit 3`)
	r := NewRunner(bin, "/home", "/usr/bin")

	_, err := r.Run(context.Background(), "rev-parse")
	if err == nil {
		t.Fatal("Run() = nil error, want failure")
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error = %q, want exec error text", err)
	}
}

func TestRunInSetsDir(t *testing.T) {
	dir := t.TempDir()
	bin := fakeBin(t, `pwd`)
	r := NewRunner(bin, "/home", "/usr/bin")

	out, err := r.RunIn(context.Background(), dir, "rev-parse")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != dir {
		t.Errorf("RunIn ran in %q, want %q", got, dir)
	}
}

func TestCommandNotExecutable(t *testing.T) {
	// A missing binary fails fast with a clear error (fail-loud, R1-Q8).
	r := NewRunner("/nonexistent/git", "/home", "/usr/bin")
	_, err := r.Run(context.Background(), "rev-parse")
	if err == nil {
		t.Fatal("Run() = nil error for missing binary")
	}
	if !strings.Contains(err.Error(), "no such file") && !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want exec not-found message", err)
	}
}

// TestCommandEnvIsolation guards against stray GIT_* env leaking into execs
// (R9-Q4: output parsing must never depend on caller environment).
func TestCommandEnvIsolation(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "leak")
	t.Setenv("GIT_DIR", "/etc")
	bin := fakeBin(t, printEnvScript)
	r := NewRunner(bin, "/home", "/usr/bin")

	cmd := r.Git(context.Background(), "status")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "GIT_AUTHOR_NAME") || strings.Contains(string(out), "GIT_DIR") {
		t.Errorf("stray GIT_* env leaked into the git exec:\n%s", out)
	}
}
