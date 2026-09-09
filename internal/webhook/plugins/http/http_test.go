package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(os.Stderr, nil)) }

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name string
		tmpl string
		repo string
		ref  string
		id   string
		want string
	}{
		{
			name: "all placeholders substituted",
			tmpl: "https://example.com/hook/{repo}/{ref}/{event-id}",
			repo: "r", ref: "refs/heads/main", id: "uuid-123",
			want: "https://example.com/hook/r/refs%2Fheads%2Fmain/uuid-123",
		},
		{
			name: "slashes in ref become percent-encoded",
			tmpl: "https://example.com/hook/{ref}",
			repo: "r", ref: "refs/heads/feature/x", id: "id",
			want: "https://example.com/hook/refs%2Fheads%2Ffeature%2Fx",
		},
		{
			name: "repo is path-escaped",
			tmpl: "https://example.com/hook/{repo}",
			repo: "a b/c", ref: "refs/heads/main", id: "id",
			want: "https://example.com/hook/a%20b%2Fc",
		},
		{
			name: "template with no placeholders stays literal",
			tmpl: "https://example.com/hook",
			repo: "r", ref: "refs/heads/main", id: "id",
			want: "https://example.com/hook",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildURL(tc.tmpl, tc.repo, tc.ref, tc.id)
			if got != tc.want {
				t.Errorf("buildURL = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSignatureMatchesHMACSHA256(t *testing.T) {
	dir := t.TempDir()
	secret := "s3cret\n"
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: "https://x/{repo}", SecretFile: secretPath}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	plugin := p.(*Plugin)
	payload := []byte(`{"event-id":"uuid-123"}`)
	got, err := plugin.signature(payload)
	if err != nil {
		t.Fatal(err)
	}

	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("signature = %s, want %s", got, want)
	}
}

func TestSignatureRotatesWithSecretFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: "https://x/{repo}", SecretFile: secretPath}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	plugin := p.(*Plugin)
	first, err := plugin.signature([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	// Rotation = replace the file, no SIGHUP, no rebuild (R12-Q3).
	if err := os.WriteFile(secretPath, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := plugin.signature([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Errorf("signature did not change after secret rotation: %s", first)
	}
}

func TestNewRequiresURLTemplate(t *testing.T) {
	_, err := New(config.PluginConfig{ID: "p", Type: Name}, webhook.Deps{Log: testLog()})
	if err == nil {
		t.Error("New without url_template = nil error")
	}
}

func TestRegistration(t *testing.T) {
	reg := webhook.NewRegistry()
	reg.Register(Name, New)
	plugin, err := reg.Build(config.PluginConfig{ID: "p", Type: Name, URLTemplate: "https://x/{repo}"}, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatalf("Build = %v", err)
	}
	if plugin == nil {
		t.Fatal("Build = nil plugin")
	}
	if _, err := reg.Build(config.PluginConfig{ID: "p", Type: "bogus"}, webhook.Deps{Log: testLog()}); err == nil {
		t.Error("Build with unknown type = nil error")
	}
}

// TestRegistrationInit ensures the package's init registered into the
// process-wide default registry (self-registration, 4.1).
func TestRegistrationInit(t *testing.T) {
	plugin, err := webhook.Default.Build(config.PluginConfig{ID: "p", Type: Name, URLTemplate: "https://x/{repo}"}, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatalf("default registry Build = %v", err)
	}
	if plugin == nil {
		t.Fatal("default registry Build = nil plugin")
	}
}
