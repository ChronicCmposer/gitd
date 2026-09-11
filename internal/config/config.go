// Package config implements the strict YAML config protocol from R1-Q3:
// kebab-case keys, DefaultConfig() layering, DisallowUnknownFields (typo'd
// key = error), duration strings, and fail-fast validate() with the file path
// in every error. gitd.yaml is the full R13-Q1 schema; webhooks.yaml the
// plugin schema (R10-Q6). Reload re-parses the SIGHUP-reloadable subset
// (webhooks.yaml + spool + policies, R9-Q5) fail-safely (R8-Q6).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// LogConfig mirrors the gitd.yaml log block (R1-Q2).
type LogConfig struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// StorageConfig mirrors the storage block (R6-Q7): s3 only in v1.
type StorageConfig struct {
	Type   string `yaml:"type"`
	Bucket string `yaml:"bucket"`
	Region string `yaml:"region"`
	Prefix string `yaml:"prefix"`
}

// TLSConfig mirrors the tls block: file-path references, root:root 0600.
type TLSConfig struct {
	Cert           string `yaml:"cert"`
	Key            string `yaml:"key"`
	ClientCA       string `yaml:"client_ca"`
	RevocationList string `yaml:"revocation_list"`
}

// DDNSConfig mirrors the ddns block (Namecheap dynamic DNS).
type DDNSConfig struct {
	Host         string   `yaml:"host"`
	Domain       string   `yaml:"domain"`
	PasswordFile string   `yaml:"password_file"`
	Interval     Duration `yaml:"interval"`
}

// SpoolConfig mirrors the spool block (R6-Q1): delivered-event TTL.
type SpoolConfig struct {
	Retention Duration `yaml:"retention"`
}

// ServeConfig mirrors the serve block (R9-Q11): actions-channel buffer.
type ServeConfig struct {
	ActionsBufferSize uint8 `yaml:"actions_buffer_size"` // 0-255
}

// MirrorConfig mirrors the mirror block (R8-Q3): weekly bundle verification,
// plus restore-on-start (re-stage a restore job for every repo that has S3
// mirrors but is missing on disk at serve startup).
type MirrorConfig struct {
	VerifyInterval Duration `yaml:"verify_interval"`
	RestoreOnStart bool     `yaml:"restore_on_start"`
}

// PoliciesConfig mirrors the policies block (R9-Q5): the enabled plugin list
// plus free-form per-policy config parsed by the plugins themselves. The
// inline map captures any per-policy keys without KnownFields rejecting them.
type PoliciesConfig struct {
	Enabled []string       `yaml:"enabled"`
	Config  map[string]any `yaml:",inline"`
}

// GitdConfig is the parsed gitd.yaml (full R13-Q1 schema).
type GitdConfig struct {
	Log              LogConfig      `yaml:"log"`
	Storage          StorageConfig  `yaml:"storage"`
	TLS              TLSConfig      `yaml:"tls"`
	GitBinary        string         `yaml:"git_binary"`
	ObjectFormat     string         `yaml:"object_format"`
	Render           string         `yaml:"render"`
	DDNS             DDNSConfig     `yaml:"ddns"`
	DiskMinFreeBytes uint64         `yaml:"disk_min_free_bytes"`
	Spool            SpoolConfig    `yaml:"spool"`
	Serve            ServeConfig    `yaml:"serve"`
	HostAllowlist    []string       `yaml:"host_allowlist"`
	Mirror           MirrorConfig   `yaml:"mirror"`
	Policies         PoliciesConfig `yaml:"policies"`

	path string
}

// Path returns the config file path, used in every error message (R1-Q3).
func (c *GitdConfig) Path() string { return c.path }

// DefaultConfig returns the gitd.yaml defaults from the Config Schemas
// appendix (R13-Q1). Load layers the file over these.
func DefaultConfig() *GitdConfig {
	return &GitdConfig{
		Log: LogConfig{Level: "info", Format: "text"},
		Storage: StorageConfig{
			Type:   "s3",
			Bucket: "git.cmposer.cc",
			Region: "us-east-2",
			Prefix: "repos",
		},
		TLS: TLSConfig{
			Cert:           "/etc/gitd/tls/server.crt",
			Key:            "/etc/gitd/tls/server.key",
			ClientCA:       "/etc/gitd/tls/client-ca.crt",
			RevocationList: "/etc/gitd/tls/revoked.crl",
		},
		GitBinary:        "/usr/local/bin/git",
		ObjectFormat:     "sha1",
		Render:           "server",
		DDNS:             DDNSConfig{Host: "git", Domain: "cmposer.cc", PasswordFile: "/etc/gitd/ddns-password", Interval: Duration(6 * time.Hour)},
		DiskMinFreeBytes: 512 * 1024 * 1024,
		Spool:            SpoolConfig{Retention: Duration(90 * 24 * time.Hour)},
		Serve:            ServeConfig{ActionsBufferSize: 64},
		HostAllowlist:    []string{"git.cmposer.cc", "localhost", "127.0.0.1"},
		Mirror:           MirrorConfig{VerifyInterval: Duration(7 * 24 * time.Hour), RestoreOnStart: true},
		Policies:         PoliciesConfig{Enabled: []string{}},
	}
}

// LoadGitd reads, layers, and validates gitd.yaml. Any error carries the file
// path (R1-Q3).
func LoadGitd(path string) (*GitdConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg := DefaultConfig()
	if err := unmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.path = path
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate runs the fail-fast schema checks; every error names the file.
func (c *GitdConfig) validate() error {
	var errs []error
	errf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", c.path, fmt.Sprintf(format, args...)))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errf("log.level: must be debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errf("log.format: must be text|json, got %q", c.Log.Format)
	}

	if c.Storage.Type != "s3" {
		errf("storage.type: only s3 is supported in v1, got %q", c.Storage.Type)
	}
	if c.Storage.Bucket == "" {
		errf("storage.bucket: must not be empty")
	}
	if c.Storage.Region == "" {
		errf("storage.region: must not be empty")
	}

	if c.GitBinary == "" {
		errf("git_binary: must not be empty")
	}
	switch c.ObjectFormat {
	case "sha1", "sha256":
	default:
		errf("object_format: must be sha1|sha256, got %q", c.ObjectFormat)
	}
	switch c.Render {
	case "server", "client", "none":
	default:
		errf("render: must be server|client|none, got %q", c.Render)
	}
	if c.DDNS.Interval.D() <= 0 {
		errf("ddns.interval: must be positive")
	}
	if c.Spool.Retention.D() <= 0 {
		errf("spool.retention: must be positive")
	}
	if c.Mirror.VerifyInterval.D() <= 0 {
		errf("mirror.verify_interval: must be positive")
	}
	if len(c.HostAllowlist) == 0 {
		errf("host_allowlist: must not be empty")
	}

	return errors.Join(errs...)
}

// NewLogger builds the *log/slog logger from the gitd.yaml log block (R1-Q2),
// writing to w (stderr -> journald).
func (c *GitdConfig) NewLogger(w io.Writer) (*slog.Logger, error) {
	var level slog.Level
	switch c.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("%s: log.level: invalid %q", c.path, c.Log.Level)
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if c.Log.Format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler), nil
}

// unmarshalStrict decodes YAML with DisallowUnknownFields semantics
// (yaml.v3 KnownFields) so a typo'd key is a hard error (R1-Q3).
func unmarshalStrict(data []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	// Reject trailing garbage after the first document.
	if dec.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("unexpected content after document")
	}
	return nil
}
