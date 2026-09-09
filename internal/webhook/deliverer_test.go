package webhook

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

func testDeliverer(t *testing.T) (*Deliverer, *spool.Store, *config.WebhooksConfig) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := spool.NewStore(t.TempDir(), time.Now, 90*24*time.Hour, log)
	wh := &config.WebhooksConfig{}
	reg := NewRegistry()
	reg.Register("fake", func(cfg config.PluginConfig, deps Deps) (Plugin, error) {
		return &fakePlugin{cfg: cfg, log: deps.Log}, nil
	})
	d := NewDeliverer(func() *config.WebhooksConfig { return wh }, store, reg, time.Now, log)
	return d, store, wh
}

// fakePlugin drives the retry / dead-letter and timeout paths without
// network: URLTemplate "fail" errors, "block" hangs until the context is
// canceled, anything else succeeds.
type fakePlugin struct {
	cfg config.PluginConfig
	log *slog.Logger
}

func (f *fakePlugin) Deliver(ctx context.Context, ev *event.Event) error {
	switch f.cfg.URLTemplate {
	case "fail":
		return errors.New("fake plugin failed")
	case "block":
		<-ctx.Done()
		return ctx.Err()
	default:
		return nil
	}
}

func testEvent(t *testing.T, store *spool.Store) string {
	t.Helper()
	ev := &event.Event{
		SchemaVersion: event.SchemaVersion,
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          "r",
		Ref:           "refs/heads/main",
		Type:          event.TypePush,
	}
	id, err := store.Write(ev)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeliverMarksDelivered(t *testing.T) {
	d, store, wh := testDeliverer(t)
	wh.Plugins = []config.PluginConfig{{ID: "p", Type: "fake"}}
	id := testEvent(t, store)

	if err := d.Deliver(context.Background(), "p", id); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != spool.StateDelivered {
		t.Errorf("state = %s, want delivered", rec.State)
	}
}

func TestDeliverUnknownPluginDeadLetters(t *testing.T) {
	d, store, _ := testDeliverer(t)
	id := testEvent(t, store)

	err := d.Deliver(context.Background(), "ghost", id)
	if !errors.Is(err, ErrPluginNotFound) {
		t.Fatalf("err = %v, want ErrPluginNotFound (R13-Q8)", err)
	}
	rec, err := store.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != spool.StateDead {
		t.Errorf("state = %s, want dead", rec.State)
	}
}

func TestDeliverFailureRetriesThenDeadLetters(t *testing.T) {
	d, store, wh := testDeliverer(t)
	wh.Plugins = []config.PluginConfig{{ID: "p", Type: "fake", URLTemplate: "fail"}}
	id := testEvent(t, store)

	// The retry budget is 3 retries then dead-letter (R11-Q4): the first
	// three failures schedule retries (state stays pending, attempts grows),
	// the fourth dead-letters.
	for i := 1; i <= 3; i++ {
		err := d.Deliver(context.Background(), "p", id)
		if err == nil {
			t.Fatalf("attempt %d = nil error, want failure", i)
		}
		if errors.Is(err, spool.ErrDead) {
			t.Fatalf("attempt %d dead-lettered too early", i)
		}
		rec, _ := store.Read(id)
		if rec.State != spool.StatePending || rec.Attempts != i {
			t.Errorf("after attempt %d: %+v", i, rec)
		}
	}
	err := d.Deliver(context.Background(), "p", id)
	if !errors.Is(err, spool.ErrDead) {
		t.Fatalf("final err = %v, want ErrDead", err)
	}
	rec, _ := store.Read(id)
	if rec.State != spool.StateDead || rec.Attempts != 4 {
		t.Errorf("dead-letter state: %+v", rec)
	}
}

func TestDeliverPerPluginTimeout(t *testing.T) {
	d, store, wh := testDeliverer(t)
	// The blocking plugin hangs until ctx done; a tiny timeout bounds it.
	wh.Plugins = []config.PluginConfig{{ID: "p", Type: "fake", URLTemplate: "block", Timeout: config.Duration(20 * time.Millisecond)}}
	id := testEvent(t, store)

	start := time.Now()
	err := d.Deliver(context.Background(), "p", id)
	if err == nil {
		t.Fatal("blocking plugin delivered without error")
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Errorf("per-attempt timeout not applied: took %v", time.Since(start))
	}
	rec, _ := store.Read(id)
	if rec.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 (failure recorded)", rec.Attempts)
	}
}

func TestDeliverMissingEvent(t *testing.T) {
	d, _, wh := testDeliverer(t)
	wh.Plugins = []config.PluginConfig{{ID: "p", Type: "fake"}}
	err := d.Deliver(context.Background(), "p", "no-such-id")
	if err == nil || !strings.Contains(err.Error(), "read event") {
		t.Errorf("err = %v, want read-event failure (no dead-letter on missing file)", err)
	}
}

func TestBuildUnknownTypeFails(t *testing.T) {
	d, store, wh := testDeliverer(t)
	wh.Plugins = []config.PluginConfig{{ID: "p", Type: "bogus"}}
	id := testEvent(t, store)
	err := d.Deliver(context.Background(), "p", id)
	if err == nil {
		t.Error("unknown plugin type = nil error")
	}
	rec, _ := store.Read(id)
	if rec.State != spool.StateDead {
		t.Errorf("state = %s, want dead (build failure is a final failure)", rec.State)
	}
}
