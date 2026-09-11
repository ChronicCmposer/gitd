package spool

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/event"
)

func testStore(t *testing.T, now func() time.Time) *Store {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return NewStore(t.TempDir(), now, 90*24*time.Hour, log)
}

func testEvent() *event.Event {
	return &event.Event{
		SchemaVersion: event.SchemaVersion,
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          "r",
		Ref:           "refs/heads/main",
		Type:          event.TypePush,
	}
}

func TestWriteReadList(t *testing.T) {
	s := testStore(t, time.Now)
	id, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("Write returned empty id")
	}
	if !strings.Contains(id, "-") {
		t.Errorf("id %q does not look like a UUID", id)
	}
	rec, err := s.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != StatePending || rec.SchemaVersion != event.SchemaVersion {
		t.Errorf("Read = %+v", rec)
	}
	recs, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].EventID != id {
		t.Errorf("List = %+v", recs)
	}
}

func TestWriteGeneratesIDAndSchema(t *testing.T) {
	s := testStore(t, time.Now)
	ev := testEvent()
	ev.EventID = "fixed-id"
	ev.SchemaVersion = 0
	if _, err := s.Write(ev); err != nil {
		t.Fatal(err)
	}
	rec, err := s.Read("fixed-id")
	if err != nil {
		t.Fatal(err)
	}
	if rec.SchemaVersion != event.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", rec.SchemaVersion, event.SchemaVersion)
	}
}

func TestDecodeRejectsUnknownAndNewerSchema(t *testing.T) {
	s := testStore(t, time.Now)
	dir := s.Dir()
	bad := []string{
		`{"schema-version":1,"event-id":"a","state":"pending","bogus":1}`,
		`{"schema-version":0,"event-id":"a","state":"pending"}`,
		`{"schema-version":2,"event-id":"a","state":"pending"}`,
	}
	for i, data := range bad {
		path := filepath.Join(dir, "bad"+string(rune('0'+i))+".json")
		if err := os.WriteFile(path, []byte(data), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Read("bad" + string(rune('0'+i))); err == nil {
			t.Errorf("Read(bad%d) = nil error, want decode error", i)
		}
	}
}

func TestBackoffSchedule(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want := []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute}
	for i, d := range want {
		next, ok := NextRetryAt(i+1, now)
		if !ok || !next.Equal(now.Add(d)) {
			t.Errorf("NextRetryAt(%d) = %v, %v; want +%v", i+1, next, ok, d)
		}
	}
	if _, ok := NextRetryAt(0, now); ok {
		t.Error("NextRetryAt(0) ok = true, want false")
	}
	if _, ok := NextRetryAt(4, now); ok {
		t.Error("NextRetryAt(4) ok = true, want false (budget exhausted)")
	}
}

func TestRecordFailureDeadLetters(t *testing.T) {
	var now = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, func() time.Time { return now })
	id, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}

	// The pinned schedule (R11-Q4) budgets 3 retries after the initial
	// attempt: failures 1-3 schedule 30s -> 5m -> 30m, failure 4 dead-letters.
	want := []time.Duration{30 * time.Second, 5 * time.Minute, 30 * time.Minute}
	for i, d := range want {
		now = now.Add(time.Second)
		rec, err := s.RecordFailure(id)
		if err != nil {
			t.Fatalf("RecordFailure(%d) err = %v, want pending +%v", i+1, err, d)
		}
		if rec.Attempts != i+1 || rec.State != StatePending || rec.NextRetryAt != now.Add(d).Format(time.RFC3339) {
			t.Errorf("after failure %d: %+v", i+1, rec)
		}
	}

	rec, err := s.RecordFailure(id)
	if err != ErrDead {
		t.Fatalf("RecordFailure(4) err = %v, want ErrDead", err)
	}
	if rec.State != StateDead || rec.Attempts != 4 {
		t.Errorf("dead-letter: %+v", rec)
	}

	// Dead records survive purge (R11-Q4).
	n, err := s.Purge()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("Purge removed %d dead records, want 0", n)
	}
}

func TestPurgeDeliveredExpiredOnly(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, func() time.Time { return base })

	idDelivered, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState(idDelivered, StateDelivered); err != nil {
		t.Fatal(err)
	}
	idPending, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}

	// Delivered but within TTL: not purged.
	n, err := s.Purge()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Purge within TTL removed %d, want 0", n)
	}

	// After TTL: only the delivered record is purged.
	s.SetRetention(24 * time.Hour)
	base = base.Add(48 * time.Hour)
	n, err = s.Purge()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("Purge = %d, want 1", n)
	}
	if _, err := s.Read(idDelivered); err == nil {
		t.Error("delivered record still readable after purge")
	}
	if _, err := s.Read(idPending); err != nil {
		t.Error("pending record was purged")
	}
}

func TestCatchUp(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := testStore(t, func() time.Time { return base })

	dueID, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	waitID, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.update(waitID, func(rec *Record) {
		rec.NextRetryAt = base.Add(24 * time.Hour).Format(time.RFC3339)
	}); err != nil {
		t.Fatal(err)
	}
	deadID, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetState(deadID, StateDead); err != nil {
		t.Fatal(err)
	}

	ids, err := s.CatchUp()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != dueID {
		t.Errorf("CatchUp = %v, want [%s]", ids, dueID)
	}
}

func TestWriteFSyncDiscipline(t *testing.T) {
	s := testStore(t, time.Now)
	// The fsync sequence (file -> rename -> parent dir) is exercised by Write;
	// this test asserts the file exists with 0640 perms (R4-Q11).
	id, err := s.Write(testEvent())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(s.Dir(), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("spool file mode = %v, want 0640", st.Mode().Perm())
	}
}
