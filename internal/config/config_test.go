package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Log.Level != "info" || c.Log.Format != "text" {
		t.Errorf("log defaults: %+v", c.Log)
	}
	if c.Storage.Type != "s3" || c.Storage.Bucket != "git.cmposer.cc" || c.Storage.Region != "us-east-2" || c.Storage.Prefix != "repos" {
		t.Errorf("storage defaults: %+v", c.Storage)
	}
	if c.DiskMinFreeBytes != 512*1024*1024 {
		t.Errorf("disk_min_free_bytes = %d", c.DiskMinFreeBytes)
	}
	if c.Spool.Retention.D() != 90*24*time.Hour {
		t.Errorf("spool.retention = %v", c.Spool.Retention)
	}
	if c.Serve.ActionsBufferSize != 64 {
		t.Errorf("actions_buffer_size = %d", c.Serve.ActionsBufferSize)
	}
	if c.Mirror.VerifyInterval.D() != 7*24*time.Hour {
		t.Errorf("mirror.verify_interval = %v", c.Mirror.VerifyInterval)
	}
	if !c.Mirror.RestoreOnStart {
		t.Error("mirror.restore_on_start default = false, want true")
	}
	if c.ObjectFormat != "sha1" {
		t.Errorf("object_format default = %q, want sha1", c.ObjectFormat)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitd.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadGitdLayersOverDefaults(t *testing.T) {
	path := writeTemp(t, "disk_min_free_bytes: 1073741824\nserve:\n  actions_buffer_size: 8\n")
	c, err := LoadGitd(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DiskMinFreeBytes != 1073741824 {
		t.Errorf("disk = %d", c.DiskMinFreeBytes)
	}
	if c.Serve.ActionsBufferSize != 8 {
		t.Errorf("buffer = %d", c.Serve.ActionsBufferSize)
	}
	if c.Storage.Bucket != "git.cmposer.cc" {
		t.Errorf("bucket default lost: %q", c.Storage.Bucket)
	}
}

func TestLoadGitdRejectsUnknownKey(t *testing.T) {
	path := writeTemp(t, "disck_min_free_bytes: 1\n")
	_, err := LoadGitd(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want unknown-key error naming %s", err, path)
	}
}

func TestLoadGitdAcceptsObjectFormat(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		path := writeTemp(t, "object_format: "+format+"\n")
		c, err := LoadGitd(path)
		if err != nil {
			t.Fatalf("LoadGitd(object_format=%s) = %v", format, err)
		}
		if c.ObjectFormat != format {
			t.Errorf("ObjectFormat = %q, want %q", c.ObjectFormat, format)
		}
	}
}

func TestLoadGitdLayersRestoreOnStart(t *testing.T) {
	// Default is true; an explicit false must override the default.
	path := writeTemp(t, "mirror:\n  restore_on_start: false\n")
	c, err := LoadGitd(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mirror.RestoreOnStart {
		t.Error("mirror.restore_on_start = true, want false override")
	}
	if c.Mirror.VerifyInterval.D() != 7*24*time.Hour {
		t.Errorf("mirror.verify_interval default lost: %v", c.Mirror.VerifyInterval)
	}
}

func TestLoadGitdRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "bad log level", content: "log:\n  level: loud\n"},
		{name: "bad storage type", content: "storage:\n  type: gcs\n"},
		{name: "bad render", content: "render: svg\n"},
		{name: "bad object format", content: "object_format: sha512\n"},
		{name: "buffer out of range", content: "serve:\n  actions_buffer_size: 300\n"},
		{name: "bad duration", content: "spool:\n  retention: nope\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.content)
			if _, err := LoadGitd(path); err == nil {
				t.Errorf("LoadGitd(%s) = nil error, want error", tc.name)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{in: "90d", want: 90 * 24 * time.Hour},
		{in: "6h", want: 6 * time.Hour},
		{in: "30s", want: 30 * time.Second},
		{in: "1m", want: time.Minute},
		{in: "0s", want: 0},
		{in: "x", err: true},
		{in: "-5d", err: true},
	}
	for _, tc := range tests {
		got, err := ParseDuration(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseDuration(%q) = nil error", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestLoadWebhooksDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhooks.yaml")
	content := "plugins:\n  - id: p1\n    type: http\n    url_template: https://x/{repo}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWebhooks(path)
	if err != nil {
		t.Fatal(err)
	}
	p := w.Plugins[0]
	if p.Timeout.D() != 30*time.Second || p.Retries != 3 || len(p.Repos) != 1 || p.Repos[0] != "*" {
		t.Errorf("plugin defaults not layered: %+v", p)
	}
}

func TestLoadWebhooksRejectsDuplicateAndUnknown(t *testing.T) {
	dir := t.TempDir()
	dup := filepath.Join(dir, "dup.yaml")
	os.WriteFile(dup, []byte("plugins:\n  - id: a\n    type: logger\n  - id: a\n    type: logger\n"), 0o600)
	if _, err := LoadWebhooks(dup); err == nil {
		t.Error("duplicate plugin id accepted")
	}
	unknown := filepath.Join(dir, "unknown.yaml")
	os.WriteFile(unknown, []byte("plugins:\n  - id: a\n    type: logger\n    bogus_key: 1\n"), 0o600)
	if _, err := LoadWebhooks(unknown); err == nil {
		t.Error("unknown plugin key accepted")
	}
}

func TestPoliciesInlineConfig(t *testing.T) {
	path := writeTemp(t, "policies:\n  enabled: [non-fast-forward]\n  non-fast-forward:\n    branches: [\"*\"]\n")
	c, err := LoadGitd(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policies.Enabled) != 1 || c.Policies.Enabled[0] != "non-fast-forward" {
		t.Errorf("enabled = %v", c.Policies.Enabled)
	}
	cfg, ok := c.Policies.Config["non-fast-forward"].(map[string]any)
	if !ok {
		t.Fatalf("per-policy config not captured: %v", c.Policies.Config)
	}
	if _, ok := cfg["branches"]; !ok {
		t.Errorf("branches key missing: %v", cfg)
	}
}

func TestReloadFailSafe(t *testing.T) {
	dir := t.TempDir()
	gitdPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(gitdPath, []byte("spool:\n  retention: 1d\n"), 0o600)
	whPath := filepath.Join(dir, "webhooks.yaml")
	os.WriteFile(whPath, []byte("plugins: []\n"), 0o600)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	rt, err := Load(gitdPath, whPath, log)
	if err != nil {
		t.Fatal(err)
	}
	old := rt.Webhooks()

	// Break webhooks.yaml; reload must keep the old config (R8-Q6).
	os.WriteFile(whPath, []byte("plugins: [\n"), 0o600)
	if err := rt.Reload(); err == nil {
		t.Fatal("Reload = nil error on broken webhooks.yaml")
	}
	if got := rt.Webhooks(); got != old {
		t.Error("broken reload swapped the config")
	}

	// Fix it; reload swaps.
	os.WriteFile(whPath, []byte("plugins:\n  - id: a\n    type: logger\n"), 0o600)
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	if got := rt.Webhooks(); got == old || len(got.Plugins) != 1 {
		t.Error("good reload did not swap the config")
	}
}
