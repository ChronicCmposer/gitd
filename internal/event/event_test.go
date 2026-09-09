package event

import (
	"strings"
	"testing"
)

func TestDecodeStrict(t *testing.T) {
	valid := `{"schema-version":1,"event-id":"x","created-at":"2026-01-01T00:00:00Z","repo":"r","ref":"refs/heads/main","type":"push","commits":[{"sha":"abc","subject":"s","author":"a <b>"}]}`
	ev, err := Decode([]byte(valid))
	if err != nil {
		t.Fatalf("Decode(valid) = %v", err)
	}
	if ev.SchemaVersion != 1 || ev.Repo != "r" || ev.Type != TypePush || len(ev.Commits) != 1 {
		t.Errorf("Decode(valid) = %+v", ev)
	}

	tests := []struct {
		name string
		in   string
	}{
		{name: "unknown field", in: `{"schema-version":1,"bogus":1}`},
		{name: "missing schema-version", in: `{"event-id":"x"}`},
		{name: "newer schema-version", in: `{"schema-version":2}`},
		{name: "malformed json", in: `{`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode([]byte(tc.in)); err == nil {
				t.Errorf("Decode(%q) = nil error, want error", tc.in)
			}
		})
	}
}

func TestTypeConstants(t *testing.T) {
	want := []string{TypePush, TypeRefCreated, TypeRefDeleted}
	for _, w := range want {
		if w == "" {
			t.Fatal("type constant empty")
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	ev := &Event{
		SchemaVersion: SchemaVersion,
		EventID:       "uuid",
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          "r",
		Ref:           "refs/heads/main",
		Type:          TypeRefCreated,
		Commits:       []Commit{{SHA: "abc", Subject: "s", Author: "a"}},
	}
	data, err := ev.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema-version"`) {
		t.Errorf("Encode missing kebab-case key: %s", data)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventID != ev.EventID || got.Ref != ev.Ref || len(got.Commits) != 1 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}
