package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// PluginConfig mirrors one webhooks.yaml plugin entry (R10-Q6).
type PluginConfig struct {
	ID                 string   `yaml:"id"`
	Type               string   `yaml:"type"` // http | logger
	URLTemplate        string   `yaml:"url_template"`
	SecretFile         string   `yaml:"secret_file"`
	Sync               bool     `yaml:"sync"`
	Repos              []string `yaml:"repos"`
	Timeout            Duration `yaml:"timeout"`
	Retries            int      `yaml:"retries"`
	InsecureSkipVerify bool     `yaml:"insecure_skip_verify"`
}

// WebhooksConfig is the parsed webhooks.yaml (R10-Q6).
type WebhooksConfig struct {
	Plugins []PluginConfig `yaml:"plugins"`
	path    string
}

// Path returns the config file path, used in every error message (R1-Q3).
func (c *WebhooksConfig) Path() string { return c.path }

// LoadWebhooks reads and validates webhooks.yaml. Plugin defaults are layered
// over missing per-plugin keys: timeout 30s, retries 3, repos ["*"].
func LoadWebhooks(path string) (*WebhooksConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg := &WebhooksConfig{}
	if err := unmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.path = path
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate applies fail-fast schema checks with the file path in every error.
func (c *WebhooksConfig) validate() error {
	var errs []error
	errf := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", c.path, fmt.Sprintf(format, args...)))
	}

	seen := make(map[string]bool)
	for i := range c.Plugins {
		p := &c.Plugins[i]

		if p.Timeout.D() == 0 {
			p.Timeout = Duration(30 * time.Second)
		}
		if p.Timeout.D() < 0 {
			errf("plugins[%d].timeout: must be positive", i)
		}
		if p.Retries == 0 {
			p.Retries = 3
		}
		if p.Retries < 0 {
			errf("plugins[%d].retries: must not be negative", i)
		}
		if len(p.Repos) == 0 {
			p.Repos = []string{"*"}
		}

		if p.ID == "" {
			errf("plugins[%d]: id must not be empty", i)
		}
		if seen[p.ID] {
			errf("plugins: duplicate plugin id %q", p.ID)
		}
		seen[p.ID] = true

		switch p.Type {
		case "http":
			if p.URLTemplate == "" {
				errf("plugins[%s].url_template: required for type http", p.ID)
			}
		case "logger":
			// No URL or secret required.
		default:
			errf("plugins[%s].type: must be http|logger, got %q", p.ID, p.Type)
		}
	}
	return errors.Join(errs...)
}
