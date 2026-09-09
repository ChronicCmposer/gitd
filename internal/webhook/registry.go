package webhook

import (
	"fmt"

	"github.com/ChronicCmposer/gitd/internal/config"
)

// Constructor builds a delivery Plugin from its config. The Deps supply the
// per-process logger. Plugins are rebuilt from the live config on every
// delivery so SIGHUP reloads and secret rotation take effect (R8-Q6, R12-Q3).
type Constructor func(cfg config.PluginConfig, deps Deps) (Plugin, error)

// Registry maps plugin type names to their constructors. Constructors
// self-register by name (http, logger) via Register; the process-wide default
// registry is populated by the plugins' init functions (database/sql driver
// pattern).
type Registry struct {
	constructors map[string]Constructor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{constructors: make(map[string]Constructor)}
}

// Register records the constructor for name. Registering a name twice is an
// impossible programmer error (two plugins claiming one type name), so it
// panics.
func (r *Registry) Register(name string, c Constructor) {
	if _, dup := r.constructors[name]; dup {
		panic(fmt.Sprintf("webhook: duplicate plugin constructor %q", name))
	}
	r.constructors[name] = c
}

// Build constructs the plugin for cfg. An unknown type name is an error (fail
// loud) so a mistyped webhooks.yaml type never silently no-ops.
func (r *Registry) Build(cfg config.PluginConfig, deps Deps) (Plugin, error) {
	c, ok := r.constructors[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("webhook: unknown plugin type %q", cfg.Type)
	}
	return c(cfg, deps)
}

// Default is the process-wide plugin registry. Plugin packages register their
// constructors here in init(); internal/cli/serve.go blank-imports the plugin
// packages so the registrations are live in the gitd binary.
var Default = NewRegistry()
