// Package webhook implements the Phase 4 plugin architecture (4.1-4.4).
//
// Plugin delivers one webhook event to a destination (http, logger). Delivery
// runs inside the serve actions channel (R9-Q11), so it is serial FIFO per
// plugin (R8-Q7), with a per-attempt timeout and durable retry / dead-letter
// bookkeeping (R8-Q8, R11-Q4).
//
// Policy evaluates one pre-receive ref-update line; a non-nil error rejects
// the push with its message surfaced on the git client's stderr (R5-Q1).
// Evaluation is fail-closed: any plugin error rejects, and every stdin line is
// evaluated (R11-Q10).
//
// Registries hold the plugin and policy constructors, which self-register by
// name (http, logger, non-fast-forward). Plugins are rebuilt from the live,
// SIGHUP-reloadable config on every delivery so reloads and secret rotation
// take effect (R8-Q6, R12-Q3).
package webhook

import (
	"context"
	"log/slog"

	"github.com/ChronicCmposer/gitd/internal/event"
)

// Plugin delivers one webhook event. Implementations are constructed from
// config.PluginConfig and rebuilt on every delivery from the live config. A
// nil return means the event was delivered (or intentionally filtered out,
// e.g. by a repos glob); any error is a delivery failure.
type Plugin interface {
	Deliver(ctx context.Context, ev *event.Event) error
}

// Deps carries the per-process dependencies plugins need at construction.
type Deps struct {
	Log *slog.Logger
}
