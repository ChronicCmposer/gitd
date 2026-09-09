package http

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

func testEvent() *event.Event {
	return &event.Event{
		SchemaVersion: event.SchemaVersion,
		EventID:       "uuid-123",
		CreatedAt:     "2026-01-01T00:00:00Z",
		Repo:          "my/repo",
		Ref:           "refs/heads/main",
		Type:          event.TypePush,
		Commits:       []event.Commit{{SHA: "abc", Subject: "s", Author: "a <b>"}},
	}
}

func TestDeliverSendsEnvelopeAndSignature(t *testing.T) {
	var gotPath, gotSig string
	var gotPayload map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath preserves the %2F escaping the client applied (R13-Q5).
		gotPath = r.URL.EscapedPath()
		gotSig = r.Header.Get(signatureHeader)
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: ts.URL + "/hook/{repo}/{ref}/{event-id}", SecretFile: secretPath}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}

	ev := testEvent()
	if err := p.Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/hook/my%2Frepo/refs%2Fheads%2Fmain/uuid-123" {
		t.Errorf("path = %q, want escaped substitution (R13-Q5)", gotPath)
	}
	if gotSig == "" || !strings.HasPrefix(gotSig, "sha256=") {
		t.Errorf("signature header = %q", gotSig)
	}
	if gotPayload["event-id"] != "uuid-123" || gotPayload["schema-version"] != float64(1) || gotPayload["type"] != "push" {
		t.Errorf("payload = %v, want kebab-case envelope (R10-Q7)", gotPayload)
	}
}

func TestDeliverReposFilter(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: ts.URL + "/{repo}", Repos: []string{"allowed"}}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	ev := testEvent()

	if err := p.Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Errorf("non-matching repo delivered, hits = %d", hits)
	}
	ev.Repo = "allowed"
	if err := p.Deliver(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("matching repo not delivered, hits = %d", hits)
	}
}

func TestDeliverNon2xxIsFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A redirect is a 3xx and must count as a delivery failure (R7-Q1).
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer ts.Close()

	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: ts.URL + "/{repo}"}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Deliver(context.Background(), testEvent()); err == nil {
		t.Error("redirect (3xx) delivery = nil error, want failure")
	}
}

func TestDeliverTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: ts.URL + "/{repo}", Timeout: config.Duration(20 * time.Millisecond)}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = p.Deliver(context.Background(), testEvent())
	if err == nil {
		t.Fatal("slow receiver delivered without error")
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Errorf("timeout did not bound the attempt: took %v", time.Since(start))
	}
}

func TestDeliverMissingSecretFileFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := config.PluginConfig{ID: "p", Type: Name, URLTemplate: ts.URL + "/{repo}", SecretFile: filepath.Join(t.TempDir(), "missing")}
	p, err := New(cfg, webhook.Deps{Log: testLog()})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Deliver(context.Background(), testEvent()); err == nil {
		t.Error("missing secret_file delivered without error")
	}
}

func Example_verifySignature() {
	// Receivers verify the X-Gitd-Signature header with hmac.Equal
	// (constant-time, R2-Q8/R3-Q5).
	secret := []byte("s3cret")
	payload := []byte(`{"event-id":"uuid-123"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	got := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	fmt.Println(hmac.Equal([]byte(want), []byte(got)))
	// Output: true
}
