// Package serve owns the serve-side actions channel (R9-Q11): a buffered chan
// with a single worker running all serve work (bundle uploads, deliveries,
// sweeps, verifies) plus the unix-socket HTTP server (R11-Q6) that notify and
// gitd spool replay submit into (R9-Q11, R10-Q2, R12-Q2).
//
// The webhook delivery engine itself lands in Phase 4; this package exposes
// the Deliver seam (a func injected at construction) and the 404 unknown
// plugin-id reply (R13-Q8). All channel submissions wait up to 10s for a slot
// then reply 503 busy (R12-Q1); notify's 60s socket client timeout (R11-Q3)
// is the outer bound.
package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/repo"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

// gitUID is the git user id in the image (R7-Q3); the stale-socket unlink
// guard refuses to remove sockets not owned by it (R12-Q4).
const gitUID = 1001

// Submission constants (R12-Q1, R13-Q9). submitWait is a var so tests can
// exercise the busy path without a 10s delay.
var submitWait = 10 * time.Second

const (
	readHeaderTO  = 10 * time.Second
	maxBodyBytes  = 64 << 10 // 64KiB (R13-Q9)
	drainTimeout  = 10 * time.Second
	verifyTimeout = 60 * time.Second
)

// ErrBusy reports that the actions channel had no slot within submitWait.
var ErrBusy = errors.New("serve: actions channel busy")

// ErrShuttingDown reports a submission made after shutdown began.
var ErrShuttingDown = errors.New("serve: shutting down")

// Config wires the serve process. All fields are required.
type Config struct {
	Mirror            *mirror.Mirror
	Spool             *spool.Store
	Webhooks          func() *config.WebhooksConfig // live (SIGHUP-reloadable) plugins
	Deliver           func(ctx context.Context, pluginID, eventID string) error
	ReposRoot         string
	SocketPath        string
	Now               func() time.Time
	Log               *slog.Logger
	SweepInterval     time.Duration
	VerifyInterval    time.Duration
	ActionsBufferSize int // serve.actions_buffer_size (R9-Q11); 0 = default 64
}

// Serve is the actions-channel server. It is safe to submit from any
// goroutine; all work executes on the single worker, so serve-side effects
// are globally FIFO (R8-Q7 ordering falls out of the channel, R9-Q3/Q11).
type Serve struct {
	actions        chan func(*Serve)
	workerDone     chan struct{}
	done           chan struct{} // closed when Run begins shutdown
	mirror         *mirror.Mirror
	spool          *spool.Store
	webhooks       func() *config.WebhooksConfig
	deliver        func(ctx context.Context, pluginID, eventID string) error
	reposRoot      string
	socketPath     string
	now            func() time.Time
	log            *slog.Logger
	sweepInterval  time.Duration
	verifyInterval time.Duration
	httpSrv        *http.Server
}

// New returns a Serve ready to Run. A zero ActionsBufferSize falls back to
// the plan default of 64 (R9-Q11) so hand-rolled Config values behave like
// the config-file default.
func New(cfg Config) *Serve {
	buffer := cfg.ActionsBufferSize
	if buffer <= 0 {
		buffer = 64
	}
	return &Serve{
		actions:        make(chan func(*Serve), buffer),
		workerDone:     make(chan struct{}),
		done:           make(chan struct{}),
		mirror:         cfg.Mirror,
		spool:          cfg.Spool,
		webhooks:       cfg.Webhooks,
		deliver:        cfg.Deliver,
		reposRoot:      cfg.ReposRoot,
		socketPath:     cfg.SocketPath,
		now:            cfg.Now,
		log:            cfg.Log,
		sweepInterval:  cfg.SweepInterval,
		verifyInterval: cfg.VerifyInterval,
	}
}

// Run serves the unix-socket endpoints and background loops until ctx is
// canceled, then drains in-flight actions up to drainTimeout (R2-Q12).
func (s *Serve) Run(ctx context.Context) error {
	if err := s.unlinkStaleSocket(); err != nil {
		return err
	}
	s.startWorker()

	// Startup catch-up + one sweep run before socket work is accepted
	// (R10-Q9, R6-Q1): sequential through the channel, audit-logged.
	if err := s.catchUp(); err != nil {
		return err
	}
	startupDone := make(chan struct{})
	s.actions <- func(sv *Serve) {
		sv.sweepOnce()
		close(startupDone)
	}
	<-startupDone

	s.startSweepLoop(ctx)
	s.startVerifyLoop(ctx)

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("serve: listen %s: %w", s.socketPath, err)
	}
	s.httpSrv = &http.Server{Handler: s.handler(), ReadHeaderTimeout: readHeaderTO}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.httpSrv.Serve(ln) }()
	s.log.Info("socket server listening", "socket", s.socketPath)

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: socket server: %w", err)
	case <-ctx.Done():
	}

	s.log.Info("shutting down; draining in-flight actions")
	close(s.done)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
		s.log.Warn("socket server shutdown", "error", err)
	}
	select {
	case <-s.workerDone:
	case <-shutdownCtx.Done():
		s.log.Warn("drain timed out; abandoning in-flight actions")
	}
	return nil
}

// submit queues act for the worker, waiting up to submitWait (R12-Q1). It
// never blocks past shutdown: done closed makes the select bail out.
func (s *Serve) submit(act func(*Serve)) error {
	select {
	case s.actions <- act:
		return nil
	case <-time.After(submitWait):
		return ErrBusy
	case <-s.done:
		return ErrShuttingDown
	}
}

// startWorker runs the single global-FIFO worker (R9-Q3). On shutdown it
// drains the remaining buffer, so in-flight submissions complete (R2-Q12).
func (s *Serve) startWorker() {
	go func() {
		defer close(s.workerDone)
		for {
			select {
			case act := <-s.actions:
				act(s)
			case <-s.done:
				for {
					select {
					case act := <-s.actions:
						act(s)
					default:
						return
					}
				}
			}
		}
	}()
}

// catchUp re-queues events whose next-retry-at has arrived, sequential through
// the channel, before socket work is accepted (R10-Q9).
func (s *Serve) catchUp() error {
	ids, err := s.spool.CatchUp()
	if err != nil {
		return fmt.Errorf("serve: startup catch-up: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	s.log.Info("startup catch-up re-queueing", "count", len(ids))
	for _, id := range ids {
		s.actions <- s.deliverAllAction(id)
	}
	return nil
}

// sweepOnce purges delivered+expired events (R6-Q1) and re-queues due pending
// ones. It runs inside the worker; re-queue submissions are non-blocking so a
// full channel never deadlocks the worker (the next sweep retries).
func (s *Serve) sweepOnce() {
	if n, err := s.spool.Purge(); err != nil {
		s.log.Error("spool purge failed", "error", err)
	} else if n > 0 {
		s.log.Info("spool sweep purged", "count", n)
	}
	ids, err := s.spool.CatchUp()
	if err != nil {
		s.log.Error("spool catch-up failed", "error", err)
		return
	}
	for _, id := range ids {
		select {
		case s.actions <- s.deliverAllAction(id):
		default:
			s.log.Warn("actions channel full; deferring re-queue", "event-id", id)
		}
	}
}

// verifyOnce checks the latest bundle of every repo (R8-Q3). The whole pass is
// capped so a wedged git never blocks the channel forever.
func (s *Serve) verifyOnce() {
	repos, err := repo.ListBare(s.reposRoot)
	if err != nil {
		s.log.Error("mirror verify: list repos", "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	for _, r := range repos {
		if err := s.mirror.Verify(ctx, r); err != nil {
			s.log.Error("mirror verify failed", "repo", r, "error", err)
		}
	}
}

func (s *Serve) startSweepLoop(ctx context.Context) {
	if s.sweepInterval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(s.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.submit(func(sv *Serve) { sv.sweepOnce() }); err != nil {
					return
				}
			}
		}
	}()
}

func (s *Serve) startVerifyLoop(ctx context.Context) {
	if s.verifyInterval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(s.verifyInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.submit(func(sv *Serve) { sv.verifyOnce() }); err != nil {
					return
				}
			}
		}
	}()
}

// deliverAllAction delivers id to every configured plugin, sequentially
// (per-plugin FIFO, R8-Q7). Used by catch-up and the sweep; Phase 4 wires the
// real delivery + durable retry bookkeeping here.
func (s *Serve) deliverAllAction(id string) func(*Serve) {
	return func(sv *Serve) {
		wh := sv.webhooks()
		if len(wh.Plugins) == 0 {
			sv.log.Info("no plugins configured; event remains pending", "event-id", id)
			return
		}
		for _, p := range wh.Plugins {
			if err := sv.deliver(context.Background(), p.ID, id); err != nil {
				sv.log.Error("delivery failed", "event-id", id, "plugin", p.ID, "error", err)
			}
		}
	}
}

// bundleBackoff is the sleep between bundle upload attempts (R9-Q2). A var so
// tests can shrink it; production keeps 1s/2s/4s, well inside the 60s socket
// bound.
var bundleBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// bundleAction creates + uploads the bundle for repo with the retry budget:
// one initial attempt + 3 retries with backoff (R9-Q2). The timestamp is
// generated inside the action at execution time, so the last-executed bundle
// is the newest repo state (R10-Q3).
func (s *Serve) bundleAction(repoName string) (mirror.BundleResult, error) {
	var result mirror.BundleResult
	var err error
	for i := 0; i <= len(bundleBackoff); i++ {
		result, err = s.mirror.CreateBundle(context.Background(), repoName)
		if err == nil {
			return result, nil
		}
		if i < len(bundleBackoff) {
			time.Sleep(bundleBackoff[i])
		}
	}
	return result, fmt.Errorf("serve: bundle %s: %w", repoName, err)
}

// unlinkStaleSocket removes a leftover socket from an unclean shutdown, only
// when it is a socket owned by the git user (R12-Q4).
func (s *Serve) unlinkStaleSocket() error {
	st, err := os.Lstat(s.socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("serve: stat %s: %w", s.socketPath, err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("serve: refusing to remove %s: not a socket", s.socketPath)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != gitUID {
		return fmt.Errorf("serve: refusing to remove %s: not owned by git (uid %d)", s.socketPath, gitUID)
	}
	if err := os.Remove(s.socketPath); err != nil {
		return fmt.Errorf("serve: remove stale socket: %w", err)
	}
	s.log.Info("removed stale socket", "path", s.socketPath)
	return nil
}

// handler routes the socket endpoints (R9-Q11, R10-Q2).
func (s *Serve) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/bundle", s.handleBundle)
	mux.HandleFunc("/v1/deliver", s.handleDeliver)
	return mux
}

// handleBundle serves POST /v1/bundle: synchronous bundle create+upload in the
// channel; 200 + {uploaded, reason} on success (zero-ref skip explicit,
// R11-Q3); 503 busy when the channel is full (R12-Q1).
func (s *Serve) handleBundle(w http.ResponseWriter, r *http.Request) {
	var req socket.BundleRequest
	if err := decodeStrict(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !repo.ValidName(req.Repo) {
		http.Error(w, "invalid repo name", http.StatusBadRequest)
		return
	}

	reply := make(chan struct {
		result mirror.BundleResult
		err    error
	}, 1)
	act := func(sv *Serve) {
		result, err := sv.bundleAction(req.Repo)
		reply <- struct {
			result mirror.BundleResult
			err    error
		}{result, err}
	}
	if err := s.submit(act); err != nil {
		writeServeError(w, err)
		return
	}
	select {
	case res := <-reply:
		if res.err != nil {
			writeServeError(w, res.err)
			return
		}
		writeJSON(w, socket.BundleResult{Uploaded: res.result.Uploaded, Reason: res.result.Reason})
	case <-r.Context().Done():
		// Client disconnected; the action still completes (eventual mirror,
		// R13-Q2) and the buffered reply is discarded.
	}
}

// handleDeliver serves POST /v1/deliver: synchronous delivery through the
// channel (R10-Q2, R12-Q2). An unknown plugin-id is a 404-style final failure
// (R13-Q8); any delivery error is non-2xx.
func (s *Serve) handleDeliver(w http.ResponseWriter, r *http.Request) {
	var req socket.DeliverRequest
	if err := decodeStrict(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.PluginID == "" || req.EventID == "" {
		http.Error(w, "plugin-id and event-id are required", http.StatusBadRequest)
		return
	}
	if !s.pluginConfigured(req.PluginID) {
		s.log.Error("plugin-id not configured", "plugin-id", req.PluginID, "event-id", req.EventID)
		// Unknown plugin-id is a FINAL failure (R13-Q8): dead-letter the
		// event so it is never retried or auto-purged. Recovery = re-add the
		// plugin + SIGHUP + gitd spool replay.
		if _, err := s.spool.SetState(req.EventID, spool.StateDead); err != nil {
			s.log.Warn("unknown plugin-id: could not dead-letter event", "event-id", req.EventID, "error", err)
		}
		http.Error(w, "plugin-id not configured", http.StatusNotFound)
		return
	}

	reply := make(chan error, 1)
	act := func(sv *Serve) { reply <- sv.deliver(context.Background(), req.PluginID, req.EventID) }
	if err := s.submit(act); err != nil {
		writeServeError(w, err)
		return
	}
	select {
	case err := <-reply:
		if err != nil {
			writeServeError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case <-r.Context().Done():
		// Client disconnected; the delivery action still completes.
	}
}

// pluginConfigured reports whether id is present in the live webhooks config.
func (s *Serve) pluginConfigured(id string) bool {
	for _, p := range s.webhooks().Plugins {
		if p.ID == id {
			return true
		}
	}
	return false
}

// decodeStrict decodes a JSON body with DisallowUnknownFields (parse-don't-
// validate, R8-Q10 discipline) and the 64KiB cap (R13-Q9).
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) error {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

// writeServeError maps busy/shutdown to 503 and everything else to 500, with
// the error message as the body (notify surfaces it on the git client's
// stderr, R5-Q2).
func writeServeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBusy) || errors.Is(err, ErrShuttingDown) {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// writeJSON writes a JSON reply.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Default().Error("serve: encode reply", "error", err)
	}
}
