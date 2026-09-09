// Package http implements the webhook HTTP delivery plugin (4.2): it POSTs
// the event envelope to a URL template with HMAC-SHA256 authentication, TLS
// verification on by default, a hardened HTTP client, and a repos glob filter.
package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	nethttp "net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/event"
	"github.com/ChronicCmposer/gitd/internal/webhook"
)

// Name is the plugin type as it appears in webhooks.yaml.
const Name = "http"

// signatureHeader is the HMAC auth header (R3-Q5): X-Gitd-Signature:
// sha256=<hex> over the payload.
const signatureHeader = "X-Gitd-Signature"

// maxResponseBytes caps the response body read (R7-Q1).
const maxResponseBytes = 1 << 20 // 1MiB

// dialTimeout bounds the net-level dial separately from the per-attempt
// timeout (R7-Q1).
const dialTimeout = 10 * time.Second

// Plugin delivers event envelopes over HTTP with the hardened client
// (R7-Q1). It is rebuilt from the live config on every delivery.
type Plugin struct {
	id          string
	urlTemplate string
	secretFile  string
	repos       []string
	client      *nethttp.Client
	log         *slog.Logger
}

// New builds the plugin from its config. The secret is not read here; it is
// read per delivery attempt so rotation = replace the file (R12-Q3).
func New(cfg config.PluginConfig, deps webhook.Deps) (webhook.Plugin, error) {
	if cfg.URLTemplate == "" {
		return nil, errors.New("http: url_template is required")
	}
	timeout := cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := &nethttp.Transport{
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify, // opt-in (R2-Q8)
		},
	}
	// Redirects are never followed: a 3xx counts as a delivery failure
	// (R7-Q1). Response bodies are capped and closed immediately in Deliver.
	client := &nethttp.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: func(*nethttp.Request, []*nethttp.Request) error { return nethttp.ErrUseLastResponse },
	}
	return &Plugin{
		id:          cfg.ID,
		urlTemplate: cfg.URLTemplate,
		secretFile:  cfg.SecretFile,
		repos:       cfg.Repos,
		client:      client,
		log:         deps.Log,
	}, nil
}

// Deliver POSTs the event envelope to the substituted URL template, signing
// the payload with the current secret (read this attempt, R12-Q3).
func (p *Plugin) Deliver(ctx context.Context, ev *event.Event) error {
	if !p.matches(ev.Repo) {
		p.log.Info("event filtered by repos glob", "plugin", p.id, "event-id", ev.EventID, "repo", ev.Repo)
		return nil
	}
	payload, err := ev.Encode()
	if err != nil {
		return fmt.Errorf("http deliver: encode event: %w", err)
	}
	target := buildURL(p.urlTemplate, ev.Repo, ev.Ref, ev.EventID)

	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("http deliver: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.secretFile != "" {
		sig, err := p.signature(payload)
		if err != nil {
			return fmt.Errorf("http deliver: sign payload: %w", err)
		}
		req.Header.Set(signatureHeader, "sha256="+sig)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("http deliver %s: %w", target, err)
	}
	defer resp.Body.Close()
	// Response body capped at 1MiB and closed immediately (R7-Q1); bodies
	// are never logged (R3-Q1).
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http deliver %s: %s: %s", target, resp.Status, trim(string(body)))
	}
	return nil
}

// matches reports whether repo passes the plugin's repos glob filter.
func (p *Plugin) matches(repo string) bool {
	if len(p.repos) == 0 {
		return true
	}
	for _, g := range p.repos {
		if ok, _ := path.Match(g, repo); ok {
			return true
		}
	}
	return false
}

// signature returns the hex HMAC-SHA256 of payload under the secret read from
// secret_file this attempt (R12-Q3). The secret itself is never logged
// (R2-Q8).
func (p *Plugin) signature(payload []byte) (string, error) {
	secret, err := os.ReadFile(p.secretFile)
	if err != nil {
		return "", fmt.Errorf("read secret_file: %w", err)
	}
	mac := hmac.New(sha256.New, bytes.TrimSpace(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// buildURL substitutes the {repo}, {ref}, {event-id} placeholders, RFC 3986
// path-escaping each value on substitution (R13-Q5): slashes in {ref} become
// %2F and the template is the only literal-slash source.
func buildURL(tmpl, repo, ref, eventID string) string {
	r := strings.NewReplacer(
		"{repo}", url.PathEscape(repo),
		"{ref}", url.PathEscape(ref),
		"{event-id}", url.PathEscape(eventID),
	)
	return r.Replace(tmpl)
}

// trim bounds error-message bodies.
func trim(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func init() {
	webhook.Default.Register(Name, New)
}
