package config

import (
	"log/slog"
	"sync"
)

// Runtime holds both parsed configs for a process. The gitd.yaml core (log,
// storage, tls, ...) is static per process; only the SIGHUP-reloadable subset
// (webhooks.yaml + gitd.yaml spool + policies, R9-Q5) changes at runtime.
// A mutex guards the swap so serve-side readers never observe a torn reload.
type Runtime struct {
	mu           sync.RWMutex
	gitdPath     string
	webhooksPath string
	gitd         *GitdConfig
	webhooks     *WebhooksConfig
	log          *slog.Logger
}

// Load parses gitd.yaml and webhooks.yaml strictly. Both must validate before
// the process starts (fail-fast startup, R1-Q3).
func Load(gitdPath, webhooksPath string, log *slog.Logger) (*Runtime, error) {
	gitd, err := LoadGitd(gitdPath)
	if err != nil {
		return nil, err
	}
	webhooks, err := LoadWebhooks(webhooksPath)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		gitdPath:     gitdPath,
		webhooksPath: webhooksPath,
		gitd:         gitd,
		webhooks:     webhooks,
		log:          log,
	}, nil
}

// Gitd returns the live gitd.yaml config (a consistent snapshot).
func (r *Runtime) Gitd() *GitdConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gitd
}

// Webhooks returns the live webhooks.yaml config (a consistent snapshot).
func (r *Runtime) Webhooks() *WebhooksConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.webhooks
}

// Reload re-parses the SIGHUP-reloadable subset (webhooks.yaml + gitd.yaml
// spool + policies). On any parse or validation error the previous config
// stays live and the error is audit-logged (fail-safe, R8-Q6).
func (r *Runtime) Reload() error {
	webhooks, err := LoadWebhooks(r.webhooksPath)
	if err != nil {
		r.log.Error("config reload failed; keeping previous config", "error", err)
		return err
	}
	gitd, err := LoadGitd(r.gitdPath)
	if err != nil {
		r.log.Error("config reload failed; keeping previous config", "error", err)
		return err
	}
	r.mu.Lock()
	r.webhooks = webhooks
	r.gitd = gitd
	r.mu.Unlock()
	r.log.Info("config reloaded", "webhooks", r.webhooksPath, "gitd", r.gitdPath)
	return nil
}
