package sshcmd

import (
	"strings"
	"testing"
)

func TestIdentityFromEnv(t *testing.T) {
	env := []string{
		"SSH_AUTH_KEY_FP=SHA256:abcd",
		"SSH_AUTH_KEY_TYPE=ssh-ed25519",
		"SSH_AUTH_CERT_ID=git@cmposer",
		"SSH_CONNECTION=1.2.3.4 51234 10.0.0.5 22",
		"PATH=/usr/bin",
	}
	got := IdentityFromEnv(env)
	if got.KeyFP != "SHA256:abcd" || got.KeyType != "ssh-ed25519" || got.CertID != "git@cmposer" || got.ClientIP != "1.2.3.4" {
		t.Fatalf("IdentityFromEnv = %+v", got)
	}
	if !got.Complete() {
		t.Fatal("Complete() = false with key fp + cert id set")
	}
}

func TestIdentityIncomplete(t *testing.T) {
	// Missing SSH_AUTH_KEY_FP or SSH_AUTH_CERT_ID => git sessions fail
	// closed (R10-Q5); the greeting still renders (identity defaults).
	tests := []struct {
		name string
		env  []string
	}{
		{name: "empty env", env: nil},
		{name: "key only", env: []string{"SSH_AUTH_KEY_FP=SHA256:abcd"}},
		{name: "cert only", env: []string{"SSH_AUTH_CERT_ID=git@cmposer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ident := IdentityFromEnv(tc.env)
			if ident.Complete() {
				t.Fatalf("Complete() = true for %v", tc.env)
			}
			if g := Greeting(ident); strings.Count(g, "\n") != 2 {
				t.Fatalf("Greeting has %d lines: %q", strings.Count(g, "\n"), g)
			}
		})
	}
}

func TestGreetingTwoLines(t *testing.T) {
	ident := IdentityFromEnv([]string{
		"SSH_AUTH_KEY_FP=SHA256:abcd",
		"SSH_AUTH_CERT_ID=git@cmposer",
		"SSH_CONNECTION=9.9.9.9 1 2 3",
	})
	got := Greeting(ident)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("Greeting = %d lines, want exactly 2 (R12-Q8): %q", len(lines), got)
	}
	if !strings.Contains(lines[0], "git@cmposer") || !strings.Contains(lines[0], "SHA256:abcd") || !strings.Contains(lines[0], "9.9.9.9") {
		t.Errorf("line 1 missing identity: %q", lines[0])
	}
	if !strings.Contains(lines[1], "no shell access") || !strings.Contains(lines[1], "Push to create") {
		t.Errorf("line 2 missing hint: %q", lines[1])
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "1.2.3.4 51234 10.0.0.5 22", want: "1.2.3.4"},
		{in: "", want: ""},
		{in: "1.2.3.4", want: "1.2.3.4"},
	}
	for _, tc := range tests {
		if got := clientIP(tc.in); got != tc.want {
			t.Errorf("clientIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEnvGet(t *testing.T) {
	env := []string{"A=1", "AB=2"}
	if got := envGet(env, "A"); got != "1" {
		t.Errorf("envGet(A) = %q", got)
	}
	if got := envGet(env, "AB"); got != "2" {
		t.Errorf("envGet(AB) = %q", got)
	}
	if got := envGet(env, "missing"); got != "" {
		t.Errorf("envGet(missing) = %q", got)
	}
}
