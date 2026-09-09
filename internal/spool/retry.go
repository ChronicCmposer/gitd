package spool

import (
	"errors"
	"fmt"
	"time"
)

// backoffSchedule is the durable retry backoff (R11-Q4): 30s -> 5m -> 30m,
// no jitter, deterministic under the injectable clock.
var backoffSchedule = []time.Duration{
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
}

// NextRetryAt returns the retry time for a record that has failed attempts
// times, or ok=false when the retry budget is exhausted (dead-letter, R11-Q4).
// The first failure schedules 30s, the second 5m, the third 30m.
func NextRetryAt(attempts int, now time.Time) (time.Time, bool) {
	if attempts <= 0 || attempts > len(backoffSchedule) {
		return time.Time{}, false
	}
	return now.Add(backoffSchedule[attempts-1]), true
}

// due reports whether a record is pending and its retry time has arrived
// (empty next-retry-at = due now). Dead and delivered records are never
// re-queued by CatchUp (R11-Q4).
func due(rec *Record, now time.Time) bool {
	if rec.State != StatePending {
		return false
	}
	if rec.NextRetryAt == "" {
		return true
	}
	next, err := time.Parse(time.RFC3339, rec.NextRetryAt)
	if err != nil {
		return false
	}
	return !next.After(now)
}

// update atomically rewrites the internal state block of a record (R8-Q8,
// R11-Q4): read, mutate, write back with the same fsync discipline. The
// envelope fields are preserved untouched.
func (s *Store) update(id string, mutate func(*Record)) (*Record, error) {
	path := s.path(id)
	rec, err := s.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("spool update %s: %w", id, err)
	}
	mutate(rec)
	if err := s.writeAtomic(path, rec); err != nil {
		return nil, fmt.Errorf("spool update %s: %w", id, err)
	}
	return rec, nil
}

// SetState marks a record delivered or dead (atomic rewrite of the internal
// block only).
func (s *Store) SetState(id string, state State) (*Record, error) {
	return s.update(id, func(rec *Record) { rec.State = state })
}

// RecordFailure persists one failed delivery attempt: it increments attempts
// and schedules the next retry per the backoff schedule (R8-Q8, R11-Q4). When
// the budget is exhausted the record is dead-lettered. It returns the updated
// record and a non-nil ErrDead when the record transitioned to dead.
func (s *Store) RecordFailure(id string) (*Record, error) {
	rec, err := s.update(id, func(rec *Record) {
		rec.Attempts++
		next, ok := NextRetryAt(rec.Attempts, s.now())
		if !ok {
			rec.State = StateDead
			rec.NextRetryAt = ""
			return
		}
		rec.NextRetryAt = next.Format(time.RFC3339)
	})
	if err != nil {
		return nil, err
	}
	if rec.State == StateDead {
		return rec, ErrDead
	}
	return rec, nil
}

// ErrDead reports a record that exhausted its retry budget and was
// dead-lettered (R11-Q4: dead events are never auto-purged).
var ErrDead = errors.New("spool: retry budget exhausted, dead-lettered")

// CatchUp returns the ids of pending events whose next-retry-at has arrived
// (R10-Q9). The serve actions channel re-queues them sequentially at startup,
// audit-logged, before socket work is accepted.
func (s *Store) CatchUp() ([]string, error) {
	now := s.now()
	recs, err := s.List()
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range recs {
		if due(&recs[i], now) {
			ids = append(ids, recs[i].EventID)
		}
	}
	return ids, nil
}
