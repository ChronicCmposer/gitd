package webhook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
)

// RefUpdate is one pre-receive stdin line, parsed at the boundary (R9-Q7):
// "<old-sha> <new-sha> <ref>".
type RefUpdate struct {
	Old string
	New string
	Ref string
}

// Policy evaluates one ref-update line for a push. A nil return accepts the
// line; a non-nil error rejects the push, its message surfaced on the git
// client's stderr (R5-Q1). Evaluation is fail-closed: any plugin error rejects.
type Policy interface {
	Evaluate(ctx context.Context, line RefUpdate) error
}

// PolicyDeps carries the per-process dependencies policies need at build time.
type PolicyDeps struct {
	Git     *gitenv.Runner
	RepoDir string
}

// PolicyConstructor builds a Policy from its free-form per-policy config map,
// parsed and validated by the plugin itself (R9-Q5, R10-Q6).
type PolicyConstructor func(cfg map[string]any, deps PolicyDeps) (Policy, error)

// PolicyRegistry maps policy names to their constructors (self-registration,
// like the plugin Registry). The process-wide default registry is populated
// by the policy packages' init functions.
type PolicyRegistry struct {
	constructors map[string]PolicyConstructor
}

// NewPolicyRegistry returns an empty policy registry.
func NewPolicyRegistry() *PolicyRegistry {
	return &PolicyRegistry{constructors: make(map[string]PolicyConstructor)}
}

// Register records the constructor for name. Registering a name twice is an
// impossible programmer error, so it panics.
func (r *PolicyRegistry) Register(name string, c PolicyConstructor) {
	if _, dup := r.constructors[name]; dup {
		panic(fmt.Sprintf("webhook: duplicate policy constructor %q", name))
	}
	r.constructors[name] = c
}

// DefaultPolicies is the process-wide policy registry.
var DefaultPolicies = NewPolicyRegistry()

// policyTimeout is the per-plugin evaluation bound (R5-Q1): a hung policy
// cannot wedge a push.
const policyTimeout = 10 * time.Second

// PolicyEngine builds the enabled policies from gitd.yaml (R9-Q5) and
// evaluates every stdin line against them, aggregating rejections via
// errors.Join (R11-Q10).
type PolicyEngine struct {
	registry *PolicyRegistry
	deps     PolicyDeps
	policies []Policy
}

// NewPolicyEngine wires a PolicyEngine over the given registry and deps.
func NewPolicyEngine(registry *PolicyRegistry, deps PolicyDeps) *PolicyEngine {
	return &PolicyEngine{registry: registry, deps: deps}
}

// Build constructs the enabled policies from pc. An unknown policy name or a
// config the policy rejects fails fast (fail-closed startup).
func (e *PolicyEngine) Build(pc config.PoliciesConfig) error {
	var policies []Policy
	for _, name := range pc.Enabled {
		c, ok := e.registry.constructors[name]
		if !ok {
			return fmt.Errorf("webhook: unknown policy %q", name)
		}
		raw, ok := pc.Config[name].(map[string]any)
		if !ok && pc.Config[name] != nil {
			return fmt.Errorf("policy %s: config must be a mapping, got %T", name, pc.Config[name])
		}
		policy, err := c(raw, e.deps)
		if err != nil {
			return fmt.Errorf("policy %s: %w", name, err)
		}
		policies = append(policies, policy)
	}
	e.policies = policies
	return nil
}

// Evaluate checks every line against every enabled policy, aggregating
// rejections via errors.Join (R11-Q10). Any rejection rejects the push; zero
// lines or zero policies accept. Each (policy, line) evaluation has its own
// 10s timeout (R5-Q1).
func (e *PolicyEngine) Evaluate(lines []RefUpdate) error {
	if len(e.policies) == 0 {
		return nil
	}
	var errs []error
	for _, ln := range lines {
		for _, p := range e.policies {
			ctx, cancel := context.WithTimeout(context.Background(), policyTimeout)
			err := p.Evaluate(ctx, ln)
			cancel()
			if err != nil {
				errs = append(errs, fmt.Errorf("ref %s: %w", ln.Ref, err))
			}
		}
	}
	return errors.Join(errs...)
}
