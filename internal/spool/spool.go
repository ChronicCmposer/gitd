// Package spool is the durable webhook event store (R11-Q4): one JSON file per
// event at /var/spool/gitd, unique name (the event-id UUID), temp-file +
// atomic rename + fsync (file -> rename -> parent dir, before notify returns,
// R5-Q2). Each file holds the event envelope plus an internal state block
// {state, attempts, next-retry-at}; decode is strict (R8-Q10) and refuses
// unknown NEWER schema versions without touching the file (R9-Q10).
package spool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ChronicCmposer/gitd/internal/event"
)

// State is the internal delivery state (R11-Q4).
type State string

// State values for the internal block (R11-Q4).
const (
	StatePending   State = "pending"
	StateDelivered State = "delivered"
	StateDead      State = "dead"
)

// fileExt is the spool file suffix.
const fileExt = ".json"

// Record is one spool file: the event envelope fields plus the internal state
// block (R11-Q4).
type Record struct {
	event.Event
	State       State  `json:"state"`
	Attempts    int    `json:"attempts"`
	NextRetryAt string `json:"next-retry-at"` // RFC3339; empty = due now
}

// Store persists event records under a directory with fsync discipline.
type Store struct {
	dir       string
	now       func() time.Time
	retention time.Duration
	log       *slog.Logger
}

// NewStore returns a Store rooted at dir. now is the injectable clock
// (R2-Q12); retention is the delivered-event TTL (SIGHUP-reloadable, R6-Q1).
func NewStore(dir string, now func() time.Time, retention time.Duration, log *slog.Logger) *Store {
	return &Store{dir: dir, now: now, retention: retention, log: log}
}

// SetRetention updates the delivered-event TTL (SIGHUP reload, R6-Q1).
func (s *Store) SetRetention(d time.Duration) { s.retention = d }

// Dir returns the spool directory.
func (s *Store) Dir() string { return s.dir }

// Write durably stores ev and returns its id (the event-id UUID, R5-Q7). A
// missing EventID is generated; a missing schema-version defaults to the
// current one (the envelope schema is pinned). The fsync sequence (file ->
// rename -> parent dir) completes before Write returns (R5-Q2).
func (s *Store) Write(ev *event.Event) (string, error) {
	if ev.EventID == "" {
		id, err := NewID()
		if err != nil {
			return "", fmt.Errorf("spool write: generate id: %w", err)
		}
		ev.EventID = id
	}
	if ev.SchemaVersion == 0 {
		ev.SchemaVersion = event.SchemaVersion
	}
	rec := Record{Event: *ev, State: StatePending}
	if err := s.writeAtomic(s.path(ev.EventID), &rec); err != nil {
		return "", fmt.Errorf("spool write %s: %w", ev.EventID, err)
	}
	return ev.EventID, nil
}

// Read returns the record for id.
func (s *Store) Read(id string) (*Record, error) {
	rec, err := s.readFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("spool read %s: %w", id, err)
	}
	return rec, nil
}

// List returns all records sorted by id.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("spool list: %w", err)
	}
	var recs []Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != fileExt {
			continue
		}
		rec, err := s.readFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("spool list %s: %w", e.Name(), err)
		}
		recs = append(recs, *rec)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].EventID < recs[j].EventID })
	return recs, nil
}

// Delete removes the record for id. A missing record is not an error.
func (s *Store) Delete(id string) error {
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("spool delete %s: %w", id, err)
	}
	return nil
}

// Purge removes delivered records past the retention TTL (R6-Q1, R11-Q4).
// Dead-lettered records are never removed (R11-Q4). It returns the number of
// records removed and is used by both `gitd spool purge` and the serve sweep
// goroutine.
func (s *Store) Purge() (int, error) {
	now := s.now()
	recs, err := s.List()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, rec := range recs {
		if !expired(&rec, s.retention, now) {
			continue
		}
		if err := s.Delete(rec.EventID); err != nil {
			return removed, err
		}
		removed++
		s.log.Info("spool purged", "event-id", rec.EventID, "state", rec.State)
	}
	return removed, nil
}

// expired reports whether a delivered record is past its retention TTL.
func expired(rec *Record, retention time.Duration, now time.Time) bool {
	if rec.State != StateDelivered || retention <= 0 {
		return false
	}
	created, err := time.Parse(time.RFC3339, rec.CreatedAt)
	if err != nil {
		return false
	}
	return now.Sub(created) >= retention
}

// path returns the spool file path for an id.
func (s *Store) path(id string) string { return filepath.Join(s.dir, id+fileExt) }

// readFile decodes one spool file strictly.
func (s *Store) readFile(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rec, err := decodeRecord(data)
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// decodeRecord parses a record with DisallowUnknownFields (R8-Q10) and
// refuses a missing or newer schema-version without modifying the file
// (R9-Q10).
func decodeRecord(data []byte) (*Record, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("decode spool record: %w", err)
	}
	if rec.SchemaVersion == 0 {
		return nil, fmt.Errorf("decode spool record: missing schema-version")
	}
	if rec.SchemaVersion > event.SchemaVersion {
		return nil, fmt.Errorf("event schema-version %d unsupported (running %d)", rec.SchemaVersion, event.SchemaVersion)
	}
	return &rec, nil
}

// writeAtomic writes rec to path with the fsync discipline: temp file in the
// same directory, fsync file, rename over the target, fsync parent dir
// (R5-Q2, R8-Q8).
func (s *Store) writeAtomic(path string, rec *Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode spool record: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, ".spool-*"+fileExt)
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	if err := fsyncDir(s.dir); err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}

// fsyncDir fsyncs the directory so the rename is durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
