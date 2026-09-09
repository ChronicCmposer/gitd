// Package event defines the webhook event envelope (R10-Q7): the kebab-case
// JSON schema shared by spool files and webhook payloads. Decode is strict —
// unknown fields are an error (parse-don't-validate, mirrors the R1-Q3 config
// protocol) and an unknown NEWER schema-version refuses to load (R8-Q10,
// R9-Q10: never silently partially decode a future event).
package event

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the current envelope schema version. Events carrying a
// newer version are refused by Decode (R9-Q10).
const SchemaVersion = 1

// Type values for the envelope (R10-Q7).
const (
	TypePush       = "push"
	TypeRefCreated = "ref-created"
	TypeRefDeleted = "ref-deleted"
)

// Commit summarizes one commit in the pushed range (R10-Q7).
type Commit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Author  string `json:"author"`
}

// Event is the webhook/spool envelope. Field names are pinned kebab-case
// (R10-Q7); the struct is the single source of truth for both spool files and
// webhook payloads.
type Event struct {
	SchemaVersion int      `json:"schema-version"`
	EventID       string   `json:"event-id"`
	CreatedAt     string   `json:"created-at"`
	Repo          string   `json:"repo"`
	Ref           string   `json:"ref"`
	Type          string   `json:"type"`
	Commits       []Commit `json:"commits"`
}

// ErrNewerSchema reports an event whose schema-version is newer than the
// running gitd supports (R9-Q10). The file must be left untouched.
var ErrNewerSchema = fmt.Errorf("event schema-version newer than supported %d", SchemaVersion)

// Decode parses a strict event from data. Unknown fields, a missing schema
// version, an unsupported newer schema version, an unknown type, and a
// malformed created-at all fail loudly: err == nil implies a valid event
// (the FuzzDecodeEvent invariant, R5-Q9).
func Decode(data []byte) (*Event, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var ev Event
	if err := dec.Decode(&ev); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	if ev.SchemaVersion == 0 {
		return nil, fmt.Errorf("decode event: missing schema-version")
	}
	if ev.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("%w: got %d", ErrNewerSchema, ev.SchemaVersion)
	}
	switch ev.Type {
	case TypePush, TypeRefCreated, TypeRefDeleted:
	default:
		return nil, fmt.Errorf("decode event: unknown type %q", ev.Type)
	}
	if _, err := time.Parse(time.RFC3339, ev.CreatedAt); err != nil {
		return nil, fmt.Errorf("decode event: created-at: %w", err)
	}
	return &ev, nil
}

// Encode serializes the event as compact JSON.
func (e *Event) Encode() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}
	return data, nil
}
