package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		{"bare needs subcommand", []string{"mirror", "--config", cfg}, []string{"usage: gitd mirror <command>", "list", "delete", "fetch"}},
		{"list too many args", []string{"mirror", "--config", cfg, "list", "a", "b"}, []string{"usage: gitd mirror list [<repo>]"}},
		{"delete needs repo", []string{"mirror", "--config", cfg, "delete"}, []string{"usage: gitd mirror delete <repo>"}},
		{"fetch needs repo", []string{"mirror", "--config", cfg, "fetch"}, []string{"usage: gitd mirror fetch <repo>"}},
		{"fetch too many args", []string{"mirror", "--config", cfg, "fetch", "r", "a", "b"}, []string{"usage: gitd mirror fetch <repo> [dest]"}},
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
	for _, want := range []string{"usage: gitd mirror <command>", "list [<repo>]", "delete <repo>", "fetch <repo> [dest]"} {
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
