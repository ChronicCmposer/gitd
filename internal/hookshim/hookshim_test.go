package hookshim

import (
	"bytes"
	"strings"
	"testing"
)

func TestSubcommandFor(t *testing.T) {
	tests := []struct {
		in   string
		want string
		err  bool
	}{
		{in: "/usr/local/lib/gitd/hooks/pre-receive", want: "pre-receive"},
		{in: "pre-receive", want: "pre-receive"},
		{in: "/usr/local/lib/gitd/hooks/post-receive", want: "notify"},
		{in: "post-receive", want: "notify"},
		{in: "something-else", err: true},
		{in: "", err: true},
	}
	for _, tc := range tests {
		got, err := subcommandFor(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("subcommandFor(%q) = nil error, want error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("subcommandFor(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestRunNoArgv(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(nil, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("Run(no argv) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no argv") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunUnknownHook(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"bogus"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("Run(bogus) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown hook binary") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunGitdMissing(t *testing.T) {
	// gitdBinary (/usr/local/bin/gitd) does not exist in the dev sandbox:
	// Run fails loudly with exit 1 and a clear message (fail-loud, R1-Q8).
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"pre-receive"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Errorf("Run(missing gitd) = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "hookshim") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
