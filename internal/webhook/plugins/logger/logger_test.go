package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

func testLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestDeliverLogsEvent(t *testing.T) {
	log, buf := testLogger(t)
	p, err := New(config.PluginConfig{ID: "p1", Type: Name}, webhook.Deps{Log: log})
	if err != nil {
		t.Fatal(err)
	}
	ev := &event.Event{
		SchemaVersion: event.SchemaVersion,
		EventID:       "ev1",
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          "r",
		Ref:           "refs/heads/main",
		Type:          event.TypePush,
		Commits:       []event.Commit{{SHA: "abc", Subject: "s", Author: "a"}},
	}
	if err := p.Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"webhook event", "event-id=ev1", "repo=r", "ref=refs/heads/main", "type=push", "commits=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
	// Commit subjects are never logged (R3-Q1): the logger emits no subject
	// key at all.
	if strings.Contains(out, "subject") {
		t.Errorf("log leaked a commit subject key: %q", out)
	}
}

func TestRegistrationInit(t *testing.T) {
	log, _ := testLogger(t)
	plugin, err := webhook.Default.Build(config.PluginConfig{ID: "p", Type: Name}, webhook.Deps{Log: log})
	if err != nil {
		t.Fatalf("default registry Build = %v", err)
	}
	if plugin == nil {
		t.Fatal("default registry Build = nil plugin")
	}
}
