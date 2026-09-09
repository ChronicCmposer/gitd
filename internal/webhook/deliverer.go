package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

// ErrPluginNotFound reports a delivery to a plugin-id absent from the live
// webhooks config (R13-Q8): a final failure that dead-letters the event.
var ErrPluginNotFound = errors.New("webhook: plugin-id not configured")

// defaultTimeout is the per-attempt bound when a plugin config omits a
// timeout (config layers 30s, but hand-rolled configs must behave too).
const defaultTimeout = 30 * time.Second

// Deliverer wires the serve.Deliver seam (4.2): it rebuilds the plugin from
// the live webhooks.yaml on each delivery (so SIGHUP reloads and secret
// rotation take effect, R8-Q6, R12-Q3), runs it with the per-attempt timeout,
// and updates spool state — delivered on success, dead-lettered when the
// retry budget is exhausted (R11-Q4). Every delivery runs inside the serve
// actions channel, so it is serial FIFO per plugin (R8-Q7).
type Deliverer struct {
	webhooks func() *config.WebhooksConfig
	spool    *spool.Store
	registry *Registry
	now      func() time.Time
	log      *slog.Logger
}

// NewDeliverer wires a Deliverer. webhooks must return the live (SIGHUP-
// reloadable) webhooks config; registry supplies the plugin constructors.
func NewDeliverer(webhooks func() *config.WebhooksConfig, spool *spool.Store, registry *Registry, now func() time.Time, log *slog.Logger) *Deliverer {
	return &Deliverer{webhooks: webhooks, spool: spool, registry: registry, now: now, log: log}
}

// Deliver delivers one event to one plugin, updating spool state. It backs
// both the serve socket /v1/deliver (sync + replay, R10-Q2, R12-Q2) and the
// catch-up / sweep async path (R10-Q9).
func (d *Deliverer) Deliver(ctx context.Context, pluginID, eventID string) error {
	cfg, ok := d.pluginConfig(pluginID)
	if !ok {
		d.log.Error("plugin-id not configured", "plugin-id", pluginID, "event-id", eventID)
		return d.deadLetter(eventID, ErrPluginNotFound)
	}

	plugin, err := d.registry.Build(cfg, Deps{Log: d.log})
	if err != nil {
		return d.deadLetter(eventID, fmt.Errorf("build plugin: %w", err))
	}

	rec, err := d.spool.Read(eventID)
	if err != nil {
		return fmt.Errorf("deliver %s/%s: read event: %w", pluginID, eventID, err)
	}

	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	deliverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := d.now()
	if err := plugin.Deliver(deliverCtx, &rec.Event); err != nil {
		d.log.Warn("delivery failed",
			"plugin", pluginID, "event-id", eventID,
			"repo", rec.Repo, "ref", rec.Ref, "type", rec.Type,
			"latency", d.now().Sub(start), "error", err)
		return d.recordFailure(eventID, err)
	}
	d.log.Info("delivery ok",
		"plugin", pluginID, "event-id", eventID,
		"repo", rec.Repo, "ref", rec.Ref, "type", rec.Type,
		"latency", d.now().Sub(start))
	if _, err := d.spool.SetState(eventID, spool.StateDelivered); err != nil {
		return fmt.Errorf("deliver %s/%s: mark delivered: %w", pluginID, eventID, err)
	}
	return nil
}

func (d *Deliverer) pluginConfig(id string) (config.PluginConfig, bool) {
	for _, p := range d.webhooks().Plugins {
		if p.ID == id {
			return p, true
		}
	}
	return config.PluginConfig{}, false
}

// deadLetter marks the event dead immediately: a final failure (unknown
// plugin-id, R13-Q8, or an unbuildable plugin) that retrying cannot fix. Dead
// events are never auto-purged (R11-Q4); recovery is re-add/fix the plugin +
// SIGHUP + gitd spool replay.
func (d *Deliverer) deadLetter(eventID string, cause error) error {
	d.log.Error("event dead-lettered", "event-id", eventID, "error", cause)
	if _, err := d.spool.SetState(eventID, spool.StateDead); err != nil {
		return fmt.Errorf("deliver %s: dead-letter: %w", eventID, err)
	}
	return fmt.Errorf("deliver %s: %w", eventID, cause)
}

// recordFailure persists one failed delivery attempt (R8-Q8): it increments
// the attempt count and dead-letters the event when the retry budget is
// exhausted (R11-Q4). It always returns a non-nil error so the socket reply /
// sync push fails; the cause is ErrDead when the event dead-lettered.
func (d *Deliverer) recordFailure(eventID string, cause error) error {
	rec, err := d.spool.RecordFailure(eventID)
	if err != nil {
		if errors.Is(err, spool.ErrDead) {
			d.log.Error("event dead-lettered", "event-id", eventID, "error", cause)
			return err
		}
		return fmt.Errorf("deliver %s: record failure: %w", eventID, err)
	}
	return fmt.Errorf("deliver %s: %w (attempt %d, retry scheduled)", eventID, cause, rec.Attempts)
}
