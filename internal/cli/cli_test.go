package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/version"
)

// runResult captures Run's exit code and output streams.
type runResult struct {
	code   int
	stdout string
	stderr string
}

// invoke runs Run with args and returns its exit code plus captured output.
func invoke(args ...string) runResult {
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{
			name:     "version prints version and exits ok",
			args:     []string{"version"},
			wantCode: ExitOK,
			wantOut:  version.String() + "\n",
		},
		{
			name:     "version rejects arguments with usage exit",
			args:     []string{"version", "extra"},
			wantCode: ExitUsage,
			wantErr:  "gitd: version: version takes no arguments",
		},
		{
			name:     "no verb prints usage",
			args:     nil,
			wantCode: ExitUsage,
			wantErr:  "usage: gitd <command> [args]",
		},
		{
			name:     "unknown verb prints error and usage",
			args:     []string{"bogus"},
			wantCode: ExitUsage,
			wantErr:  "gitd: unknown command \"bogus\"",
		},
		{
			name:     "spool without config fails at runtime not usage",
			args:     []string{"spool", "list"},
			wantCode: ExitError,
			wantErr:  "gitd: spool:",
		},
		{
			name:     "usage lists every command",
			args:     nil,
			wantCode: ExitUsage,
			wantErr:  "spool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := invoke(tt.args...)
			if got.code != tt.wantCode {
				t.Errorf("Run(%v) code = %d, want %d", tt.args, got.code, tt.wantCode)
			}
			if tt.wantOut != "" && got.stdout != tt.wantOut {
				t.Errorf("Run(%v) stdout = %q, want %q", tt.args, got.stdout, tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(got.stderr, tt.wantErr) {
				t.Errorf("Run(%v) stderr = %q, want it to contain %q", tt.args, got.stderr, tt.wantErr)
			}
		})
	}
}
