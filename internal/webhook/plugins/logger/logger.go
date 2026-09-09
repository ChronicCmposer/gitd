// Package logger implements the reference webhook plugin (4.3): it writes
// event details to the log (slog) with no network. Useful as a reference and
// for tests.
package logger

import (
	"context"
	"log/slog"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

// Name is the plugin type as it appears in webhooks.yaml.
const Name = "logger"

// Plugin logs event metadata; delivery always succeeds.
type Plugin struct {
	id  string
	log *slog.Logger
}

// New builds the logger plugin from its config.
func New(cfg config.PluginConfig, deps webhook.Deps) (webhook.Plugin, error) {
	return &Plugin{id: cfg.ID, log: deps.Log}, nil
}

// Deliver logs the event metadata. Commit subjects are never logged (R3-Q1:
// subjects live in spool payloads, the audit trail).
func (p *Plugin) Deliver(_ context.Context, ev *event.Event) error {
	p.log.Info("webhook event",
		"plugin", p.id,
		"event-id", ev.EventID,
		"repo", ev.Repo,
		"ref", ev.Ref,
		"type", ev.Type,
		"commits", len(ev.Commits),
	)
	return nil
}

func init() {
	webhook.Default.Register(Name, New)
}
