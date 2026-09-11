package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/socket"
)

// writeGitdConfig writes a minimal valid gitd.yaml for CLI entry tests.
func writeGitdConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(path, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+filepath.Join(dir, "pw")+"\n"), 0o600)
	return path
}

func TestRunMirrorUsage(t *testing.T) {
	cfg := writeGitdConfig(t)
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{"bare needs subcommand", []string{"mirror", "--config", cfg}, []string{"usage: gitd mirror <command>", "list", "delete", "restore"}},
		{"list too many args", []string{"mirror", "--config", cfg, "list", "a", "b"}, []string{"usage: gitd mirror list [<repo>]"}},
		{"delete needs repo", []string{"mirror", "--config", cfg, "delete"}, []string{"usage: gitd mirror delete <repo>"}},
		{"restore needs repo", []string{"mirror", "--config", cfg, "restore"}, []string{"usage: gitd mirror restore <repo>"}},
		{"restore too many args", []string{"mirror", "--config", cfg, "restore", "r", "a"}, []string{"usage: gitd mirror restore <repo>"}},
		{"fetch is gone", []string{"mirror", "--config", cfg, "fetch", "r"}, []string{"unknown mirror subcommand"}},
		{"unknown sub", []string{"mirror", "--config", cfg, "bogus", "r"}, []string{"unknown mirror subcommand"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args, &stdout, &stderr); code != ExitUsage {
				t.Errorf("exit = %d, want %d", code, ExitUsage)
			}
			for _, want := range tc.wants {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}

func TestRunMirrorHelp(t *testing.T) {
	// `gitd mirror help` is an explicit help request: the subcommand reference
	// goes to stdout and the exit code is 0, not a usage error.
	cfg := writeGitdConfig(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mirror", "--config", cfg, "help"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	for _, want := range []string{"usage: gitd mirror <command>", "list [<repo>]", "delete <repo>", "restore <repo>"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunMirrorBadConfig(t *testing.T) {
	// A missing config fails at runtime (exit 1), not usage.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"mirror", "--config", "/nonexistent/gitd.yaml", "list", "r"}, &stdout, &stderr)
	if code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
}

func TestRunSpoolUsage(t *testing.T) {
	cfg := writeGitdConfig(t)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"list no args", []string{"spool", "--config", cfg, "list", "x"}, "usage: gitd spool list (no arguments)"},
		{"purge no args", []string{"spool", "--config", cfg, "purge", "x"}, "usage: gitd spool purge (no arguments)"},
		{"replay needs id", []string{"spool", "--config", cfg, "replay"}, "usage: gitd spool replay <event-id>"},
		{"help too many args", []string{"spool", "--config", cfg, "help", "x"}, "usage: gitd spool help"},
		{"unknown sub", []string{"spool", "--config", cfg, "bogus"}, "unknown spool subcommand"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args, &stdout, &stderr); code != ExitUsage {
				t.Errorf("exit = %d, want %d", code, ExitUsage)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunSpoolBareUsage(t *testing.T) {
	// Bare `gitd spool` is a usage error: it must surface the subcommand
	// reference so list/replay/purge are discoverable, not silently default
	// to list (fail-fast, exit 2 on usage).
	cfg := writeGitdConfig(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfg}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	for _, want := range []string{"usage: gitd spool <command> [args]", "list", "replay", "purge"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
}

func TestRunSpoolHelp(t *testing.T) {
	// `gitd spool help` is an explicit help request: the subcommand reference
	// goes to stdout and the exit code is 0, not a usage error.
	cfg := writeGitdConfig(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"spool", "--config", cfg, "help"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	for _, want := range []string{"usage: gitd spool <command>", "list", "replay <event-id>", "purge", "help"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunDDNSMissingPasswordFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+filepath.Join(dir, "missing")+"\n"), 0o600)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitError {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "password_file") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunDDNSEmptyPassword(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("  \n"), 0o600)
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+pw+"\n"), 0o600)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr.String(), "empty") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunServeUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"serve", "extra"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestRunPreReceiveUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pre-receive", "extra"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestRunPreReceiveBadConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pre-receive", "--config", "/nonexistent/gitd.yaml"}, &stdout, &stderr); code != ExitError {
		t.Errorf("exit = %d, want %d", code, ExitError)
	}
}

func TestRunDDNSUsage(t *testing.T) {
	cfg := writeGitdConfig(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfg, "extra"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestParseConfigFlagErrors(t *testing.T) {
	if _, _, err := parseConfigFlag([]string{"--bogus"}); err == nil {
		t.Fatal("parseConfigFlag = nil error for bad flag")
	}
	if _, _, err := parseConfigFlag(nil); err != nil {
		t.Fatalf("parseConfigFlag(nil) = %v", err)
	}
}

func TestWebhooksPathFor(t *testing.T) {
	if got := webhooksPathFor("/etc/gitd/gitd.yaml"); got != "/etc/gitd/webhooks.yaml" {
		t.Errorf("webhooksPathFor = %q", got)
	}
}

// usageError must satisfy error and be As-able (Run maps it to ExitUsage).
func TestUsageErrorIsError(t *testing.T) {
	err := errUsage("boom %d", 1)
	if !errors.Is(err, err) {
		t.Fatal("errUsage not errors.Is-able with itself")
	}
}

func TestRunPreReceiveEmptyInput(t *testing.T) {
	// runPreReceive success path: empty stdin parses cleanly and the policy
	// engine accepts (R11-Q10: zero lines = accept). A real git binary is
	// required for the policy engine's git deps; skip where unavailable.
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("git_binary: "+cliGitBin+"\npolicies:\n  enabled: []\n"), 0o600)

	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pre-receive", "--config", cfgPath}, &stdout, &stderr); code != ExitOK {
		t.Errorf("pre-receive empty input exit = %d, stderr = %s", code, stderr.String())
	}
}

func TestRunMirrorListPath(t *testing.T) {
	// runMirror's list path reaches config load then storeFor (s3.New) which
	// needs AWS credentials; with a bad region config it fails loudly at
	// runtime (exit 1), not usage — proving the entry wires config before
	// the backend.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("storage:\n  type: s3\n  bucket: b\n  region: us-east-2\n"), 0o600)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mirror", "--config", cfgPath, "list", "r"}, &stdout, &stderr); code != ExitError {
		t.Errorf("mirror list exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
}

func TestRunMirrorAgentUsage(t *testing.T) {
	// mirror-agent is a daemon role: extra arguments are a usage error.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mirror-agent", "extra"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, ExitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "mirror-agent takes no arguments") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunMirrorAgentBadConfig(t *testing.T) {
	// A missing config fails at runtime (exit 1), not usage.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mirror-agent", "--config", "/nonexistent/gitd.yaml"}, &stdout, &stderr); code != ExitError {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
}

func TestRunMirrorAgentListedInUsage(t *testing.T) {
	// The daemon role appears in the top-level usage listing.
	var stdout, stderr bytes.Buffer
	if code := Run(nil, &stdout, &stderr); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "mirror-agent") {
		t.Errorf("usage = %q, want mirror-agent listed", stderr.String())
	}
}

func TestRunMirrorRestoreSubmitsToSocket(t *testing.T) {
	// restore must submit to the serve socket and must NOT build the S3
	// store: the config below has no storage section, so a storeFor attempt
	// would fail at runtime. A successful restore through a fake serve socket
	// therefore proves the socket routing.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+filepath.Join(dir, "pw")+"\n"), 0o600)

	sock := filepath.Join(t.TempDir(), "gitd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotRepo := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/restore", func(w http.ResponseWriter, r *http.Request) {
		var req socket.RestoreRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotRepo = req.Repo
		w.WriteHeader(http.StatusOK)
	})
	go http.Serve(ln, mux)

	oldSocketPath := socketPath
	socketPath = sock
	defer func() { socketPath = oldSocketPath }()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mirror", "--config", cfgPath, "restore", "r"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("restore exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	if gotRepo != "r" {
		t.Errorf("socket received repo %q, want r", gotRepo)
	}
}

func TestRunMirrorRestoreServeDown(t *testing.T) {
	// A missing serve socket must fail loudly at runtime (exit 1), not hang
	// or silently succeed.
	cfg := writeGitdConfig(t)
	oldSocketPath := socketPath
	socketPath = filepath.Join(t.TempDir(), "missing.sock")
	defer func() { socketPath = oldSocketPath }()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"mirror", "--config", cfg, "restore", "r"}, &stdout, &stderr); code != ExitError {
		t.Errorf("restore serve-down exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
}

func TestRunRepoUsage(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		wants []string
	}{
		{"bare needs subcommand", []string{"repo"}, []string{"usage: gitd repo <command>", "list", "delete"}},
		{"list too many args", []string{"repo", "list", "x"}, []string{"usage: gitd repo list"}},
		{"delete without yes", []string{"repo", "delete", "r"}, []string{"--yes"}},
		{"delete needs repo", []string{"repo", "delete", "--yes"}, []string{"usage: gitd repo delete --yes <repo>"}},
		{"delete too many args", []string{"repo", "delete", "--yes", "r", "s"}, []string{"usage: gitd repo delete --yes <repo>"}},
		{"unknown sub", []string{"repo", "bogus"}, []string{"unknown repo subcommand"}},
		{"invalid repo name", []string{"repo", "delete", "--yes", "../escape"}, []string{"invalid repo name"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(tc.args, &stdout, &stderr); code != ExitUsage {
				t.Errorf("exit = %d, want %d", code, ExitUsage)
			}
			for _, want := range tc.wants {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
				}
			}
		})
	}
}

func TestRunRepoHelp(t *testing.T) {
	// `gitd repo help` is an explicit help request: the subcommand reference
	// goes to stdout and the exit code is 0, not a usage error.
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"repo", "help"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	for _, want := range []string{"usage: gitd repo <command>", "list", "delete --yes <repo>", "mirror restore <repo>"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout.String(), want)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

// makeFakeBare creates a bare-layout dir root/name.git (HEAD, objects/,
// refs/) without git — enough for repo.IsBareRepo.
func makeFakeBare(t *testing.T, root, name string) {
	t.Helper()
	for _, sub := range []string{"HEAD", "objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(root, name+".git", sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunRepoList(t *testing.T) {
	// `gitd repo list` enumerates the live bare repos read-only (no config
	// needed): bare .git dirs are listed, non-bare and non-.git dirs are not.
	dir := t.TempDir()
	old := reposRoot
	reposRoot = dir
	defer func() { reposRoot = old }()

	for _, name := range []string{"alpha", "beta"} {
		makeFakeBare(t, dir, name)
	}
	if err := os.MkdirAll(filepath.Join(dir, "gamma.git", "worktree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"repo", "list"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("list exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	out := stdout.String()
	for _, name := range []string{"alpha", "beta"} {
		if !strings.Contains(out, name) {
			t.Errorf("list = %q, missing %s", out, name)
		}
	}
	if strings.Contains(out, "gamma") || strings.Contains(out, "notes") {
		t.Errorf("list = %q, must exclude non-bare / non-repo dirs", out)
	}
}

func TestRunRepoDeleteSubmitsToSocket(t *testing.T) {
	// delete must submit to the serve socket and must NOT need config: the
	// repo verb uses constants only, so no gitd.yaml is written. A successful
	// delete through a fake serve socket proves the socket routing.
	sock := filepath.Join(t.TempDir(), "gitd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotRepo := ""
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/delete", func(w http.ResponseWriter, r *http.Request) {
		var req socket.DeleteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotRepo = req.Repo
		w.WriteHeader(http.StatusOK)
	})
	go http.Serve(ln, mux)

	oldSocketPath := socketPath
	socketPath = sock
	defer func() { socketPath = oldSocketPath }()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"repo", "delete", "--yes", "r"}, &stdout, &stderr); code != ExitOK {
		t.Errorf("delete exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
	if gotRepo != "r" {
		t.Errorf("socket received repo %q, want r", gotRepo)
	}
}

func TestRunRepoDeleteServeDown(t *testing.T) {
	// A missing serve socket must fail loudly at runtime (exit 1), not hang
	// or silently succeed.
	oldSocketPath := socketPath
	socketPath = filepath.Join(t.TempDir(), "missing.sock")
	defer func() { socketPath = oldSocketPath }()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"repo", "delete", "--yes", "r"}, &stdout, &stderr); code != ExitError {
		t.Errorf("delete serve-down exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
}
