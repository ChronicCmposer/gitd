package sshcmd

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/disk"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
)

// writeScript writes an executable shell script at dir/name and returns its
// path. Scripts run in a temp dir on PATH, so the gateway's argv exec of
// git-upload-pack / git-receive-pack (dash+space forms, R2-Q1) resolves them.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// gatewayTestEnv builds a GatewayConfig whose git-family execs resolve to
// fake scripts on PATH. The git binary (for push-to-create's `git init`) is
// the same fake: it handles "init" by creating a bare layout, and ignores
// everything else.
func gatewayTestEnv(t *testing.T, hooksDir string) GatewayConfig {
	t.Helper()
	binDir := t.TempDir()
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	initScript := `if [ "$1" = "init" ]; then
  d="$3"
  mkdir -p "$d/objects" "$d/refs"
  printf 'ref: refs/heads/main\n' > "$d/HEAD"
  exit 0
fi
echo "fake-git $*"
exit 0
`
	writeScript(t, binDir, "git", initScript)
	writeScript(t, binDir, "git-upload-pack", `echo "upload-pack ok"; exit 0`)
	writeScript(t, binDir, "git-receive-pack", `echo "receive-pack ok"; exit 0`)

	home := t.TempDir()
	runner := gitenv.NewRunner(filepath.Join(binDir, "git"), home, binDir+":/usr/bin")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return GatewayConfig{
		ReposRoot: t.TempDir(),
		HooksDir:  hooksDir,
		Git:       runner,
		Headroom:  disk.Headroom{MinFree: 0, WarnFree: 0},
		Log:       log,
	}
}

func identityEnv() []string {
	return []string{
		"SSH_AUTH_KEY_FP=SHA256:abcd",
		"SSH_AUTH_KEY_TYPE=ssh-ed25519",
		"SSH_AUTH_CERT_ID=git@cmposer",
		"SSH_CONNECTION=1.2.3.4 51234 10.0.0.5 22",
	}
}

func TestServeGreetingForEmptyCommand(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	var stdout, stderr bytes.Buffer
	err := Serve(identityEnv(), strings.NewReader(""), &stdout, &stderr, cfg)
	if err != nil {
		t.Fatalf("Serve = %v", err)
	}
	got := stdout.String()
	if strings.Count(got, "\n") != 2 {
		t.Fatalf("greeting has %d lines, want 2 (R12-Q8): %q", strings.Count(got, "\n"), got)
	}
	if !strings.Contains(got, "SHA256:abcd") {
		t.Errorf("greeting missing identity: %q", got)
	}
}

func TestServeRejectsBadCommand(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=rm -rf /")
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil {
		t.Fatal("Serve = nil error for rm command")
	}
	if !strings.Contains(err.Error(), "only git-upload-pack and git-receive-pack are allowed") {
		t.Errorf("error = %v", err)
	}
}

func TestServeRejectsMalformedParse(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-upload-pack 'unterminated")
	var stdout, stderr bytes.Buffer
	if err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg); err == nil {
		t.Fatal("Serve = nil error for malformed command")
	}
}

func TestServeRejectsWrongArgCount(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-upload-pack a b")
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil || !strings.Contains(err.Error(), "exactly one repository argument") {
		t.Fatalf("Serve err = %v, want exactly-one-arg error", err)
	}
}

func TestServeRejectsInvalidRepoName(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	// ".hidden" fails the allowlist (R2-Q1): leading dot is not allowed.
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-upload-pack .hidden")
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid repository name") {
		t.Fatalf("Serve err = %v, want invalid-repo-name error", err)
	}
}

func TestServeFailsClosedWithoutIdentity(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	// Complete env minus the key fingerprint => git sessions fail closed
	// (R10-Q5); the greeting path is unaffected (tested above).
	env := []string{
		"SSH_AUTH_KEY_TYPE=ssh-ed25519",
		"SSH_AUTH_CERT_ID=git@cmposer",
		"SSH_CONNECTION=1.2.3.4 51234 10.0.0.5 22",
		"SSH_ORIGINAL_COMMAND=git-upload-pack myrepo",
	}
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil || !strings.Contains(err.Error(), "missing pusher identity") {
		t.Fatalf("Serve err = %v, want missing-identity error", err)
	}
}

func TestServeUploadPackOK(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-upload-pack myrepo")
	var stdout, stderr bytes.Buffer
	if err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if !strings.Contains(stdout.String(), "upload-pack ok") {
		t.Errorf("stdout = %q, want upload-pack output", stdout.String())
	}
}

func TestServeReceivePackCreatesRepo(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-receive-pack newrepo")
	var stdout, stderr bytes.Buffer
	if err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if !strings.Contains(stdout.String(), "receive-pack ok") {
		t.Errorf("stdout = %q, want receive-pack output", stdout.String())
	}
	// push-to-create ran git init --bare (R10-Q4): bare layout exists.
	repoDir := filepath.Join(cfg.ReposRoot, "newrepo.git")
	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err != nil {
		t.Fatalf("push-to-create did not init repo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "description")); err != nil {
		t.Fatalf("push-to-create did not write description: %v", err)
	}
}

func TestServeReceivePackLowDiskFails(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	cfg.Headroom = disk.Headroom{MinFree: ^uint64(0), WarnFree: ^uint64(0)}
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-receive-pack newrepo")
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil || !strings.Contains(err.Error(), "push rejected") {
		t.Fatalf("Serve err = %v, want push-rejected error", err)
	}
}

func TestServeReceivePackExecErrorSurfaces(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	writeScript(t, binDir, "git", `exit 0`)
	writeScript(t, binDir, "git-receive-pack", `echo "boom" >&2; exit 7`)
	cfg := GatewayConfig{
		ReposRoot: t.TempDir(),
		Git:       gitenv.NewRunner(filepath.Join(binDir, "git"), t.TempDir(), binDir),
		Headroom:  disk.Headroom{MinFree: 0},
		Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-receive-pack myrepo")
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil || !strings.Contains(err.Error(), "git-receive-pack") {
		t.Fatalf("Serve err = %v, want git-receive-pack exec error", err)
	}
}

func TestRepoNameFromArg(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "myrepo", want: "myrepo"},
		{in: "myrepo.git", want: "myrepo"}, // one trailing .git stripped (R10-Q4)
		{in: "sub/dir/repo", want: "repo"}, // basename only
		{in: ".hidden", wantErr: true},     // leading dot fails the allowlist
		{in: "-dash", wantErr: true},
		{in: "a", want: "a"},
		{in: "A.B_c-d1", want: "A.B_c-d1"},
		// path traversal is neutralized by basename extraction: "../escape"
		// yields "escape", which is a valid name (the allowlist is the gate).
		{in: "../escape", want: "escape"},
	}
	for _, tc := range tests {
		got, err := RepoNameFromArg(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("RepoNameFromArg(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("RepoNameFromArg(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestPushToCreateExistsAndBare(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	repoDir := filepath.Join(cfg.ReposRoot, "existing.git")
	if err := os.MkdirAll(filepath.Join(repoDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pushToCreate(cfg, "existing", repoDir); err != nil {
		t.Fatalf("pushToCreate existing bare = %v", err)
	}
}

func TestPushToCreateExistsNotBareFails(t *testing.T) {
	cfg := gatewayTestEnv(t, "")
	repoDir := filepath.Join(cfg.ReposRoot, "notbare.git")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := pushToCreate(cfg, "notbare", repoDir)
	if err == nil || !strings.Contains(err.Error(), "not a bare repository") {
		t.Fatalf("pushToCreate notbare = %v, want not-bare error", err)
	}
}

func TestVerifyHooks(t *testing.T) {
	if err := verifyHooks(""); err != nil {
		t.Fatalf("verifyHooks(empty) = %v, want nil (disabled)", err)
	}
	dir := t.TempDir()
	if err := verifyHooks(dir); err != nil {
		t.Fatalf("verifyHooks(dir) = %v, want nil", err)
	}
	if err := verifyHooks(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("verifyHooks(missing) = nil, want error")
	}
	filePath := filepath.Join(dir, "afile")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyHooks(filePath); err == nil {
		t.Fatal("verifyHooks(file) = nil, want error")
	}
}

func TestServeGitInitFailure(t *testing.T) {
	binDir := t.TempDir()
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	// The fake git fails init; push-to-create must fail loudly.
	writeScript(t, binDir, "git", `echo "init failed" >&2; exit 1`)
	writeScript(t, binDir, "git-receive-pack", `exit 0`)
	cfg := GatewayConfig{
		ReposRoot: t.TempDir(),
		Git:       gitenv.NewRunner(filepath.Join(binDir, "git"), t.TempDir(), binDir),
		Headroom:  disk.Headroom{MinFree: 0},
		Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-receive-pack fresh")
	var stdout, stderr bytes.Buffer
	err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg)
	if err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("Serve err = %v, want init failure surfaced", err)
	}
}

// realGitGateway builds a GatewayConfig whose git binary is the real git,
// with the Runner optionally carrying an object format (pinned via
// GIT_DEFAULT_HASH). Used by the push-to-create object-format tests.
func realGitGateway(t *testing.T, objectFormat string) GatewayConfig {
	t.Helper()
	if _, err := exec.LookPath("/usr/bin/git"); err != nil {
		t.Skip("git not available")
	}
	runner := gitenv.NewRunner("/usr/bin/git", t.TempDir(), os.Getenv("PATH")).WithObjectFormat(objectFormat)
	return GatewayConfig{
		ReposRoot: t.TempDir(),
		Git:       runner,
		Headroom:  disk.Headroom{MinFree: 0, WarnFree: 0},
		Log:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestPushToCreateObjectFormat(t *testing.T) {
	// New repos default to SHA-1 (the config default; git's built-in default
	// when no GIT_DEFAULT_HASH is set), and flip to sha256 only via the
	// configured object format — the hardcoded --object-format flag is gone.
	for _, tc := range []struct {
		name         string
		objectFormat string // "" = git's built-in sha1 default
		want         string
	}{
		{name: "unset uses git sha1 default", objectFormat: "", want: "sha1"},
		{name: "configured sha1", objectFormat: "sha1", want: "sha1"},
		{name: "configured sha256 opt-in", objectFormat: "sha256", want: "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := realGitGateway(t, tc.objectFormat)
			repoDir := filepath.Join(cfg.ReposRoot, "r.git")
			if err := pushToCreate(cfg, "r", repoDir); err != nil {
				t.Fatalf("pushToCreate = %v", err)
			}
			cmd := exec.Command("/usr/bin/git", "--git-dir="+repoDir, "rev-parse", "--show-object-format")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("rev-parse --show-object-format: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("created repo format = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestServePreExistingSHA256Repo(t *testing.T) {
	// The service still serves a pre-existing SHA-256 repo: push-to-create
	// takes the EEXIST path (bare sanity check only, never re-inits), the
	// repo keeps its sha256 format, and a real client push into it succeeds.
	if _, err := exec.LookPath("/usr/bin/git"); err != nil {
		t.Skip("git not available")
	}
	cfg := gatewayTestEnv(t, "")
	repoDir := filepath.Join(cfg.ReposRoot, "sha256repo.git")
	run := func(dir string, args ...string) {
		cmd := exec.Command("/usr/bin/git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// A real sha256 bare repo with one commit.
	work := t.TempDir()
	run(work, "init", "-q", "-b", "main", "--object-format=sha256", ".")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a")
	run(work, "commit", "-qm", "first")
	run(t.TempDir(), "clone", "-q", "--bare", work, repoDir)

	// The gateway routes receive-pack to the existing sha256 repo.
	env := append(identityEnv(), "SSH_ORIGINAL_COMMAND=git-receive-pack sha256repo")
	var stdout, stderr bytes.Buffer
	if err := Serve(env, strings.NewReader(""), &stdout, &stderr, cfg); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if !strings.Contains(stdout.String(), "receive-pack ok") {
		t.Errorf("stdout = %q, want receive-pack output", stdout.String())
	}
	// The repo's format is preserved (push-to-create never re-inits).
	cmd := exec.Command("/usr/bin/git", "--git-dir="+repoDir, "rev-parse", "--show-object-format")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse --show-object-format: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "sha256" {
		t.Errorf("pre-existing repo format = %q, want sha256", got)
	}
	// A real sha256 client push into the repo succeeds. The pusher has no
	// shared history with the existing main, so it pushes a new branch (a
	// fresh ref is always accepted; the point is that sha256 objects flow).
	pusher := t.TempDir()
	run(pusher, "init", "-q", "-b", "main", "--object-format=sha256", ".")
	run(pusher, "config", "user.email", "t@t")
	run(pusher, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(pusher, "b"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(pusher, "add", "b")
	run(pusher, "commit", "-qm", "second")
	run(pusher, "push", "-q", repoDir, "HEAD:refs/heads/dev")
}
