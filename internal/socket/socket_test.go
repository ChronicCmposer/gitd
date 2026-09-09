package socket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// serveFake starts a minimal unix-socket HTTP server for client tests and
// returns its path + a shutdown func.
func serveFake(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitd.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return path, func() { srv.Close() }
}

func TestClientBundle(t *testing.T) {
	path, stop := serveFake(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/bundle" {
			http.Error(w, "wrong endpoint", http.StatusBadRequest)
			return
		}
		var req BundleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Repo != "r" {
			http.Error(w, "unexpected repo", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"uploaded":false,"reason":"no refs"}`)
	}))
	defer stop()

	c := NewClient(path, 60*time.Second)
	res, err := c.Bundle(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded || res.Reason != "no refs" {
		t.Errorf("Bundle = %+v", res)
	}
}

func TestClientBundleFailureIsError(t *testing.T) {
	path, stop := serveFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "storage down", http.StatusInternalServerError)
	}))
	defer stop()

	c := NewClient(path, 60*time.Second)
	if _, err := c.Bundle(context.Background(), "r"); err == nil {
		t.Error("Bundle on 500 = nil error, want error")
	}
}

func TestClientDeliver(t *testing.T) {
	var got DeliverRequest
	path, stop := serveFake(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deliver" {
			http.Error(w, "wrong endpoint", http.StatusBadRequest)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer stop()

	c := NewClient(path, 60*time.Second)
	if err := c.Deliver(context.Background(), "p1", "ev1"); err != nil {
		t.Fatal(err)
	}
	if got.PluginID != "p1" || got.EventID != "ev1" {
		t.Errorf("Deliver sent %+v", got)
	}
}

func TestClientServeDownFailsFast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	c := NewClient(path, 2*time.Second)
	if _, err := c.Bundle(context.Background(), "r"); err == nil {
		t.Error("Bundle with no socket = nil error, want error (R12-Q2 serve down)")
	}
}

func TestClientTimeout(t *testing.T) {
	// A server that never replies: the client must fail within its timeout
	// instead of hanging the push (R11-Q3).
	path, stop := serveFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer stop()

	c := NewClient(path, 300*time.Millisecond)
	start := time.Now()
	_, err := c.Bundle(context.Background(), "r")
	if err == nil {
		t.Fatal("Bundle = nil error, want timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("Bundle took %v, want ~300ms", time.Since(start))
	}
}

func TestClientDeliverFailureIsError(t *testing.T) {
	// 404-style unknown plugin-id reply (R13-Q8): the client must surface it
	// as a final failure so the event dead-letters.
	path, stop := serveFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "plugin-id not configured", http.StatusNotFound)
	}))
	defer stop()

	c := NewClient(path, 60*time.Second)
	err := c.Deliver(context.Background(), "ghost", "ev1")
	if err == nil {
		t.Fatal("Deliver on 404 = nil error, want error")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("plugin-id not configured")) {
		t.Errorf("Deliver err = %v", err)
	}
}

func TestClientBundleDecodeFailure(t *testing.T) {
	// A 200 reply that is not valid BundleResult JSON must error, not return
	// a zero value.
	path, stop := serveFake(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"uploaded":`) // truncated JSON
	}))
	defer stop()

	c := NewClient(path, 60*time.Second)
	if _, err := c.Bundle(context.Background(), "r"); err == nil {
		t.Fatal("Bundle with bad JSON = nil error, want error")
	}
}

func TestTrim(t *testing.T) {
	if got := trim("x"); got != "x" {
		t.Errorf("trim(short) = %q", got)
	}
	long := string(make([]byte, 300))
	if got := trim(long); len(got) != 203 {
		t.Errorf("trim(long) length = %d, want 203", len(got))
	}
}
