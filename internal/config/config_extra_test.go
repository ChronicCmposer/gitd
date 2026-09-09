package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(path, []byte("log:\n  level: info\n"), 0o600)
	cfg, err := LoadGitd(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path() != path {
		t.Fatalf("Path() = %q, want %q", cfg.Path(), path)
	}
}

func TestNewLoggerTextAndJSON(t *testing.T) {
	cfg := DefaultConfig()
	cfg.path = "/etc/gitd/gitd.yaml"

	var buf bytes.Buffer
	log, err := cfg.NewLogger(&buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("text log output = %q", buf.String())
	}

	buf.Reset()
	cfg.Log.Format = "json"
	log, err = cfg.NewLogger(&buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello")
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Errorf("json log output = %q", buf.String())
	}
}

func TestNewLoggerRejectsInvalidLevel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.path = "/etc/gitd/gitd.yaml"
	cfg.Log.Level = "bogus"
	if _, err := cfg.NewLogger(os.Stderr); err == nil {
		t.Fatal("NewLogger = nil error for invalid level")
	}
}

func TestDurationString(t *testing.T) {
	if got := (Duration(90 * 24 * time.Hour)).String(); got != "2160h0m0s" {
		t.Errorf("Duration.String = %q", got)
	}
}

func TestLoadGitdMissingFile(t *testing.T) {
	_, err := LoadGitd("/nonexistent/gitd.yaml")
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/gitd.yaml") {
		t.Fatalf("LoadGitd err = %v, want path in error", err)
	}
}

func TestValidateEdgeCases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gitd.yaml")
	bad := `
log:
  level: nope
  format: nope
storage:
  type: local
  bucket: ""
  region: ""
git_binary: ""
render: nope
ddns:
  interval: 0s
spool:
  retention: 0s
mirror:
  verify_interval: 0s
host_allowlist: []
`
	os.WriteFile(path, []byte(bad), 0o600)
	_, err := LoadGitd(path)
	if err == nil {
		t.Fatal("LoadGitd = nil error for many bad values")
	}
	for _, want := range []string{
		"log.level", "log.format", "storage.type", "storage.bucket",
		"storage.region", "git_binary", "render", "ddns.interval",
		"spool.retention", "mirror.verify_interval", "host_allowlist",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validate error missing %q:\n%v", want, err)
		}
	}
}

func TestUnmarshalStrictTrailingGarbage(t *testing.T) {
	var c GitdConfig
	if err := unmarshalStrict([]byte("log:\n  level: info\n---\nextra"), &c); err == nil {
		t.Fatal("unmarshalStrict accepted trailing document")
	}
}

func TestRuntimeGitdSnapshot(t *testing.T) {
	dir := t.TempDir()
	gitdPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(gitdPath, []byte("log:\n  level: info\n"), 0o600)
	whPath := filepath.Join(dir, "webhooks.yaml")
	os.WriteFile(whPath, []byte("plugins: []\n"), 0o600)

	rt, err := Load(gitdPath, whPath, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if rt.Gitd() == nil {
		t.Fatal("Gitd() = nil")
	}
	if rt.Gitd().Storage.Bucket != "git.cmposer.cc" {
		t.Errorf("Gitd() defaults not layered: bucket = %q", rt.Gitd().Storage.Bucket)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func TestParseDurationNegativeDay(t *testing.T) {
	if _, err := ParseDuration("-3d"); err == nil {
		t.Fatal("ParseDuration(-3d) = nil error")
	}
	if _, err := ParseDuration("xd"); err == nil {
		t.Fatal("ParseDuration(xd) = nil error")
	}
}
