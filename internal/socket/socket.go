// Package socket is the HTTP-over-unix-socket submission seam between gitd
// subprocesses (notify, spool replay, mirror restore) and the gitd-serve
// actions channel (R9-Q11, R10-Q2, R12-Q2). The server side (POST /v1/bundle,
// POST /v1/deliver, POST /v1/restore) lives in internal/serve (Phase 4); this
// client is the single submission path Phase 3 callers use.
package socket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// DefaultPath is the unix socket path shared by all gitd processes.
const DefaultPath = "/var/spool/gitd/gitd.sock"

// Client submits requests to the serve socket. The dial timeout bounds a
// wedged serve: notify fails the push with a clear error instead of hanging
// it (R11-Q3).
type Client struct {
	path  string
	httpc *http.Client
}

// NewClient returns a Client for the socket at path with the given total
// timeout per request (60s for notify, R11-Q3).
func NewClient(path string, timeout time.Duration) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{path: path, httpc: &http.Client{Transport: transport, Timeout: timeout}}
}

// BundleRequest is the POST /v1/bundle body (R9-Q11).
type BundleRequest struct {
	Repo string `json:"repo"`
}

// BundleResult is the 200 reply (R11-Q3): uploaded=false with reason "no refs"
// when the repo has zero refs (R9-Q1).
type BundleResult struct {
	Uploaded bool   `json:"uploaded"`
	Reason   string `json:"reason,omitempty"`
}

// Bundle submits a synchronous bundle create+upload for repo and blocks for
// the reply. Any non-2xx reply is an error: notify exits non-zero so the push
// reports the mirror failure (R5-Q2, R9-Q2).
func (c *Client) Bundle(ctx context.Context, repo string) (BundleResult, error) {
	body, err := json.Marshal(BundleRequest{Repo: repo})
	if err != nil {
		return BundleResult{}, fmt.Errorf("socket bundle: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gitd/v1/bundle", bytes.NewReader(body))
	if err != nil {
		return BundleResult{}, fmt.Errorf("socket bundle: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return BundleResult{}, fmt.Errorf("socket %s /v1/bundle: %w", c.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return BundleResult{}, fmt.Errorf("socket /v1/bundle: read reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return BundleResult{}, fmt.Errorf("socket /v1/bundle: %s: %s", resp.Status, trim(string(reply)))
	}
	var result BundleResult
	if err := json.Unmarshal(reply, &result); err != nil {
		return BundleResult{}, fmt.Errorf("socket /v1/bundle: decode reply: %w", err)
	}
	return result, nil
}

// DeliverRequest is the POST /v1/deliver body (R10-Q2, R12-Q2).
type DeliverRequest struct {
	PluginID string `json:"plugin-id"`
	EventID  string `json:"event-id"`
}

// Deliver submits a synchronous webhook delivery for the given plugin and
// event. 200 = delivered; any non-2xx (including the 404-style unknown
// plugin-id reply, R13-Q8) is a final failure: the event dead-letters and is
// never auto-purged (R11-Q4).
func (c *Client) Deliver(ctx context.Context, pluginID, eventID string) error {
	body, err := json.Marshal(DeliverRequest{PluginID: pluginID, EventID: eventID})
	if err != nil {
		return fmt.Errorf("socket deliver: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gitd/v1/deliver", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("socket deliver: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("socket %s /v1/deliver: %w", c.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("socket /v1/deliver: %s: %s", resp.Status, trim(string(reply)))
	}
	return nil
}

// trim bounds error-message bodies.
func trim(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// RestoreRequest is the POST /v1/restore body (serve-orchestrated mirror
// restore).
type RestoreRequest struct {
	Repo string `json:"repo"`
}

// Restore submits a synchronous mirror restore of repo to serve over the
// socket. Serve downloads + verifies the bundle and dispatches the restore to
// the gitd-restore agent, which writes /srv/git/<repo>.git as the git user —
// no admin elevation needed. Any non-2xx reply is an error: the CLI surfaces
// it so the operator knows the restore failed.
func (c *Client) Restore(ctx context.Context, repo string) error {
	body, err := json.Marshal(RestoreRequest{Repo: repo})
	if err != nil {
		return fmt.Errorf("socket restore: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://gitd/v1/restore", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("socket restore: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("socket %s /v1/restore: %w", c.path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("socket /v1/restore: %s: %s", resp.Status, trim(string(reply)))
	}
	return nil
}
