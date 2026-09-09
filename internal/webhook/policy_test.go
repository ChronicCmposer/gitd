package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/config"
)

// recordingPolicy appends each evaluated line and returns the configured error.
type recordingPolicy struct {
	name  string
	err   error
	lines *[]RefUpdate
}

func (r *recordingPolicy) Evaluate(_ context.Context, line RefUpdate) error {
	*r.lines = append(*r.lines, line)
	return r.err
}

// testEngine wires an engine with two named recording policies.
func testEngine(t *testing.T, aErr, bErr error) (*PolicyEngine, *[]RefUpdate) {
	t.Helper()
	var lines []RefUpdate
	reg := NewPolicyRegistry()
	reg.Register("a", func(cfg map[string]any, deps PolicyDeps) (Policy, error) {
		return &recordingPolicy{name: "a", err: aErr, lines: &lines}, nil
	})
	reg.Register("b", func(cfg map[string]any, deps PolicyDeps) (Policy, error) {
		return &recordingPolicy{name: "b", err: bErr, lines: &lines}, nil
	})
	return NewPolicyEngine(reg, PolicyDeps{}), &lines
}

func TestEvaluateEveryLineEveryPolicy(t *testing.T) {
	e, lines := testEngine(t, nil, nil)
	if err := e.Build(config.PoliciesConfig{Enabled: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	input := []RefUpdate{
		{Old: "0", New: "1", Ref: "refs/heads/main"},
		{Old: "0", New: "2", Ref: "refs/heads/dev"},
	}
	if err := e.Evaluate(input); err != nil {
		t.Fatal(err)
	}
	// 2 lines x 2 policies = 4 evaluations, in policy order per line.
	if len(*lines) != 4 {
		t.Errorf("evaluations = %d, want 4", len(*lines))
	}
	if (*lines)[0].Ref != "refs/heads/main" || (*lines)[2].Ref != "refs/heads/dev" {
		t.Errorf("lines evaluated out of order: %+v", *lines)
	}
}

func TestEvaluateAggregatesRejections(t *testing.T) {
	e, _ := testEngine(t, errors.New("policy a rejected"), errors.New("policy b rejected"))
	if err := e.Build(config.PoliciesConfig{Enabled: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	input := []RefUpdate{
		{Old: "0", New: "1", Ref: "refs/heads/main"},
		{Old: "0", New: "2", Ref: "refs/heads/dev"},
	}
	err := e.Evaluate(input)
	if err == nil {
		t.Fatal("Evaluate = nil error, want aggregated rejections")
	}
	// 2 lines x 2 rejecting policies = 4 joined errors (R11-Q10: pusher sees
	// all violations in one dump).
	if got := strings.Count(err.Error(), "rejected"); got != 4 {
		t.Errorf("joined rejections = %d, want 4", got)
	}
	if !strings.Contains(err.Error(), "refs/heads/main") || !strings.Contains(err.Error(), "refs/heads/dev") {
		t.Errorf("error missing ref context: %v", err)
	}
}

func TestEvaluateZeroLinesAccepts(t *testing.T) {
	e, _ := testEngine(t, errors.New("would reject"), nil)
	if err := e.Build(config.PoliciesConfig{Enabled: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	// Zero stdin lines = accept even with rejecting policies (R11-Q10).
	if err := e.Evaluate(nil); err != nil {
		t.Errorf("zero lines = %v, want accept", err)
	}
}

func TestEvaluateNoPoliciesAccepts(t *testing.T) {
	e, _ := testEngine(t, nil, nil)
	if err := e.Build(config.PoliciesConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Evaluate([]RefUpdate{{Old: "0", New: "1", Ref: "refs/heads/main"}}); err != nil {
		t.Errorf("no policies = %v, want accept (default config)", err)
	}
}

func TestBuildUnknownPolicyFailsFast(t *testing.T) {
	e, _ := testEngine(t, nil, nil)
	err := e.Build(config.PoliciesConfig{Enabled: []string{"ghost"}})
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Errorf("err = %v, want unknown-policy failure", err)
	}
}

func TestBuildRejectsWrongConfigShape(t *testing.T) {
	reg := NewPolicyRegistry()
	reg.Register("a", func(cfg map[string]any, deps PolicyDeps) (Policy, error) {
		return &recordingPolicy{name: "a"}, nil
	})
	e := NewPolicyEngine(reg, PolicyDeps{})
	err := e.Build(config.PoliciesConfig{
		Enabled: []string{"a"},
		Config:  map[string]any{"a": "not-a-mapping"},
	})
	if err == nil || !strings.Contains(err.Error(), "must be a mapping") {
		t.Errorf("err = %v, want wrong-config-shape failure", err)
	}
}
