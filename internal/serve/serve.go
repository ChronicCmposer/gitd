// Package serve owns the serve-side actions channel (R9-Q11): a buffered chan
// with a single worker running all serve work (bundle uploads, deliveries,
// sweeps, verifies) plus the unix-socket HTTP server (R11-Q6) that notify and
// gitd spool replay submit into (R9-Q11, R10-Q2, R12-Q2).
//
// Delivery executes through the injected Deliver seam (a func built from the
// webhook plugin registry); unknown plugin-ids reply 404-style (R13-Q8). The
// control mux (/v1/bundle, /v1/deliver, /v1/restore, /v1/delete) is served on
// the unix socket; the browse :443 mux mounts only /v1/bundle + /v1/deliver
// (Phase 5), so /v1/restore and /v1/delete stay socket-only. Restore and
// delete are dispatched to the git-context mirror-agent via the
// /var/spool/gitd/restore job spool — serve never writes /srv/git (its
// container mounts it ro and lacks CAP_CHOWN); see mirror.Agent. All channel
// submissions wait up to 10s for a slot then reply 503 busy (R12-Q1); notify's
// 60s socket client timeout (R11-Q3) is the outer bound.
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
	"path/filepath"
	"sort"
	"strings"
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

// TestSetSubmitWait shrinks the busy-wait for tests (R12-Q1) and returns a
// restore func. Test-only seam; production uses the 10s default.
func TestSetSubmitWait(d time.Duration) func() {
	old := submitWait
	submitWait = d
	return func() { submitWait = old }
}

// Config wires the serve process. All fields are required.
type Config struct {
	Mirror            *mirror.Mirror
	Spool             *spool.Store
	Webhooks          func() *config.WebhooksConfig // live (SIGHUP-reloadable) plugins
	Deliver           func(ctx context.Context, pluginID, eventID string) error
	ReposRoot         string
	RestoreDir        string // restore job spool for the mirror-agent (/var/spool/gitd/restore)
	SocketPath        string
	Now               func() time.Time
	Log               *slog.Logger
	SweepInterval     time.Duration
	VerifyInterval    time.Duration
	RestoreOnStart    bool // mirror.restore_on_start: stage restore jobs for S3-mirrored repos missing on disk at startup
	ActionsBufferSize int  // serve.actions_buffer_size (R9-Q11); 0 = default 64
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
	restoreDir     string
	socketPath     string
	now            func() time.Time
	log            *slog.Logger
	sweepInterval  time.Duration
	verifyInterval time.Duration
	restoreOnStart bool
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
		restoreDir:     cfg.RestoreDir,
		socketPath:     cfg.SocketPath,
		now:            cfg.Now,
		log:            cfg.Log,
		sweepInterval:  cfg.SweepInterval,
		verifyInterval: cfg.VerifyInterval,
		restoreOnStart: cfg.RestoreOnStart,
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
	// (R10-Q9, R6-Q1): sequential through the channel, audit-logged. The
	// restore-spool sweep runs here too (startup only — the periodic sweep
	// must never touch in-flight restore jobs).
	if err := s.catchUp(); err != nil {
		return err
	}
	startupDone := make(chan struct{})
	s.actions <- func(sv *Serve) {
		sv.sweepOnce()
		sv.sweepRestoreSpool()
		sv.restoreMissingOnStart()
		close(startupDone)
	}
	<-startupDone

	s.startSweepLoop(ctx)
	s.startVerifyLoop(ctx)

	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("serve: listen %s: %w", s.socketPath, err)
	}
	// Allow the git group (admin data-plane) to connect to the control socket
	// (R9-Q11); the setgid /var/spool/gitd makes the group git, so 0770 grants
	// git+admin read/write without opening it to everyone.
	if err := os.Chmod(s.socketPath, 0o770); err != nil {
		_ = ln.Close()
		return fmt.Errorf("serve: chmod socket %s: %w", s.socketPath, err)
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

// Submit queues act for the worker, waiting up to submitWait (R12-Q1). It is
// the exported seam browse render actions use (R11-Q2) so all serve-side work
// — renders, bundle uploads, deliveries — flows through the same channel.
func (s *Serve) Submit(act func(*Serve)) error { return s.submit(act) }

// SocketHandler returns the mux for the socket control endpoints (/v1/bundle,
// /v1/deliver, /v1/restore, /v1/delete). It is mounted on the unix-socket
// server (Run) and, at only the /v1/bundle + /v1/deliver paths, behind the
// browse :443 mux (Phase 5) — /v1/restore and /v1/delete are deliberately
// socket-only: the staging paths (serve dispatching to the git-context agent)
// are never reachable from :443. Both paths keep the actions-channel
// discipline and the R13-Q9 read-header/body caps.
func (s *Serve) SocketHandler() http.Handler { return s.handler() }

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

// restoreResultDeadline bounds serve's wait for the mirror-agent's
// <id>.result: 4m30s, inside the admin client's 5-minute budget
// (cli.restoreTimeout). It is a var so tests can shrink it.
var restoreResultDeadline = 4*time.Minute + 30*time.Second

// restoreStageResult is the worker's reply for a staged restore job: the job
// id (empty on failure) plus any staging error.
type restoreStageResult struct {
	id  string
	err error
}

// stageRestoreJob downloads + verifies the latest bundle for repo and stages
// a restore job for the mirror-agent, returning the job id. It runs inside
// the worker (serialized with all other serve work) and never writes the
// repo store: the gitd-restore agent performs the /srv/git write.
func (s *Serve) stageRestoreJob(repoName string) (string, error) {
	id, err := spool.NewID()
	if err != nil {
		return "", fmt.Errorf("serve: restore %s: job id: %w", repoName, err)
	}
	if err := os.MkdirAll(s.restoreDir, 0o770); err != nil {
		return "", fmt.Errorf("serve: restore %s: mkdir %s: %w", repoName, s.restoreDir, err)
	}
	data, err := s.mirror.LatestBundle(context.Background(), repoName)
	if err != nil {
		return "", fmt.Errorf("serve: restore %s: %w", repoName, err)
	}
	bundlePath := filepath.Join(s.restoreDir, id+mirror.JobBundleExt)
	if err := os.WriteFile(bundlePath, data, 0o644); err != nil {
		return "", fmt.Errorf("serve: restore %s: stage bundle: %w", repoName, err)
	}
	// Fail fast: verify the staged bundle before handing it to the agent
	// (defense in depth — the agent re-verifies too, so a forged or corrupt
	// bundle is caught here and again at the write path).
	if err := s.mirror.VerifyBundleFile(context.Background(), bundlePath); err != nil {
		_ = os.Remove(bundlePath)
		return "", fmt.Errorf("serve: restore %s: bundle verify: %w", repoName, err)
	}
	// Hand the job to the agent. The request is written atomically (temp +
	// rename) so the agent's watcher never sees a half-written job, and the
	// bundle is staged before the request so a request event implies its
	// bundle is complete.
	reqData, err := json.Marshal(mirror.JobRequest{Repo: repoName, Bundle: id + mirror.JobBundleExt})
	if err != nil {
		_ = os.Remove(bundlePath)
		return "", fmt.Errorf("serve: restore %s: encode request: %w", repoName, err)
	}
	if err := mirror.WriteJobFile(s.restoreDir, id+mirror.JobRequestExt, reqData, 0o644); err != nil {
		_ = os.Remove(bundlePath)
		return "", fmt.Errorf("serve: restore %s: stage request: %w", repoName, err)
	}
	s.log.Info("restore job staged for mirror-agent", "repo", repoName, "id", id, "bundle", id+mirror.JobBundleExt)
	return id, nil
}

// waitRestoreResult polls the spool for <id>.result up to
// restoreResultDeadline. It returns the agent's outcome; a timeout reports
// that the job is still in progress — the agent completes it regardless and
// serve's startup sweep handles the leftover files. Shared by restore and
// delete (both stage <id>.request and read <id>.result).
func (s *Serve) waitRestoreResult(id string) (*mirror.JobResult, error) {
	resultPath := filepath.Join(s.restoreDir, id+mirror.JobResultExt)
	deadline := time.After(restoreResultDeadline)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			return nil, fmt.Errorf("job still in progress (mirror-agent slow); the agent will complete it and serve's startup sweep handles leftovers")
		case <-ticker.C:
			data, err := os.ReadFile(resultPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read job result: %w", err)
			}
			res, err := mirror.DecodeJobResult(data)
			if err != nil {
				return nil, fmt.Errorf("decode job result: %w", err)
			}
			return res, nil
		}
	}
}

// cleanupRestoreJob removes the staged bundle, request, and result for id
// (serve-owned cleanup after the outcome is read; os.Remove is idempotent,
// so a job the agent already consumed is a no-op).
func (s *Serve) cleanupRestoreJob(id string) {
	for _, name := range []string{id + mirror.JobBundleExt, id + mirror.JobRequestExt, id + mirror.JobResultExt} {
		if err := os.Remove(filepath.Join(s.restoreDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("restore job cleanup", "id", id, "file", name, "error", err)
		}
	}
}

// sweepRestoreSpool removes leftover restore job files from a crash or a
// timed-out wait. Serve owns the jobs it orchestrated: after a crash serve
// no longer tracks them, so they must never linger or re-trigger. It runs at
// serve startup only — the periodic sweep must not touch in-flight jobs.
// The mirror-agent still processes leftover <id>.request files it finds on
// its own startup (legitimate crash leftovers); the race between the two is
// benign (see the mirror.Agent package comment on stale-job ownership).
func (s *Serve) sweepRestoreSpool() {
	entries, err := os.ReadDir(s.restoreDir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("restore spool sweep: read dir", "error", err)
		}
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, mirror.JobBundleExt) &&
			!strings.HasSuffix(name, mirror.JobRequestExt) &&
			!strings.HasSuffix(name, mirror.JobResultExt) {
			continue
		}
		if err := os.Remove(filepath.Join(s.restoreDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("restore spool sweep: remove", "file", name, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		s.log.Info("restore spool sweep removed stale jobs", "count", removed)
	}
}

// restoreMissingOnStart stages a restore job for every repo that has S3
// bundle mirrors but is missing on disk. It runs once at serve startup,
// inside the worker (after the restore-spool sweep, before the socket accepts
// work), and is deliberately best-effort:
//
//   - An unreachable objectstore (ListAllRepos fails) or an unreadable repos
//     root (ListBare fails) logs a warning and skips the pass — serve must
//     start even when S3 is down.
//   - The diff is additive/non-destructive by construction: only repos
//     present in S3 AND absent on disk are staged; local-only repos are never
//     touched, and mirror.Restore itself fails fast on an existing dest.
//   - A per-repo staging failure is logged and skipped, with no retry — a
//     corrupt bundle would otherwise hot-loop at every startup.
//
// The mirror-agent picks staged jobs up asynchronously (it scan-then-watches
// the restore spool), so serve never writes /srv/git itself.
func (s *Serve) restoreMissingOnStart() {
	if !s.restoreOnStart {
		return
	}
	mirrored, err := s.mirror.ListAllRepos(context.Background())
	if err != nil {
		s.log.Warn("restore-on-start skipped: listing mirrors failed", "error", err)
		return
	}
	live, err := repo.ListBare(s.reposRoot)
	if err != nil {
		s.log.Warn("restore-on-start skipped: listing live repos failed", "error", err)
		return
	}
	liveSet := make(map[string]struct{}, len(live))
	for _, name := range live {
		liveSet[name] = struct{}{}
	}
	var missing []string
	for name := range mirrored {
		if _, ok := liveSet[name]; !ok {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) == 0 {
		return
	}
	s.log.Info("restoring missing repo(s)", "count", len(missing))
	for _, name := range missing {
		if _, err := s.stageRestoreJob(name); err != nil {
			s.log.Error("restore-on-start staging failed", "repo", name, "error", err)
		}
	}
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
	mux.HandleFunc("/v1/restore", s.handleRestore)
	mux.HandleFunc("/v1/delete", s.handleDelete)
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

// handleRestore serves POST /v1/restore: serve downloads + verifies the
// latest S3 bundle, stages a restore job for the git-context mirror-agent,
// waits for the agent's <id>.result under restoreResultDeadline, and cleans
// up the staged files. Serve never writes /srv/git — the gitd-restore agent
// does. 200 on success; 400 invalid repo / bad body; 503 busy when the
// channel is full (R12-Q1); staging/agent failures and the timeout surface
// as non-2xx with a clear message.
func (s *Serve) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req socket.RestoreRequest
	if err := decodeStrict(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !repo.ValidName(req.Repo) {
		http.Error(w, "invalid repo name", http.StatusBadRequest)
		return
	}

	// Staging runs inside the worker (serialized with all other serve work);
	// the result wait runs in the request goroutine so the worker stays free
	// for bundles/deliveries while the agent works.
	reply := make(chan restoreStageResult, 1)
	act := func(sv *Serve) {
		jobID, err := sv.stageRestoreJob(req.Repo)
		reply <- restoreStageResult{id: jobID, err: err}
	}
	if err := s.submit(act); err != nil {
		writeServeError(w, err)
		return
	}
	var staged restoreStageResult
	select {
	case staged = <-reply:
	case <-r.Context().Done():
		// Client disconnected; the staging action still completes and the
		// buffered reply is discarded.
		return
	}
	if staged.err != nil {
		writeServeError(w, staged.err)
		return
	}

	res, err := s.waitRestoreResult(staged.id)
	if err != nil {
		// Timeout / unreadable result: leave the staged files in place — the
		// agent still completes the restore and serve's startup sweep handles
		// leftovers.
		writeServeError(w, err)
		return
	}
	s.cleanupRestoreJob(staged.id)
	if !res.OK {
		writeServeError(w, fmt.Errorf("restore failed: %s", res.Message))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// stageDeleteJob stages a delete job for the mirror-agent and returns the job
// id. A delete needs no bundle (nothing to download or verify), so the job is
// just the <id>.request envelope. It runs inside the worker (serialized with
// all other serve work) and never writes the repo store: the gitd-restore
// agent performs the /srv/git removal.
func (s *Serve) stageDeleteJob(repoName string) (string, error) {
	id, err := spool.NewID()
	if err != nil {
		return "", fmt.Errorf("serve: delete %s: job id: %w", repoName, err)
	}
	if err := os.MkdirAll(s.restoreDir, 0o770); err != nil {
		return "", fmt.Errorf("serve: delete %s: mkdir %s: %w", repoName, s.restoreDir, err)
	}
	reqData, err := json.Marshal(mirror.JobRequest{Type: "delete", Repo: repoName})
	if err != nil {
		return "", fmt.Errorf("serve: delete %s: encode request: %w", repoName, err)
	}
	// The request is written atomically (temp + rename) so the agent's
	// watcher never sees a half-written job.
	if err := mirror.WriteJobFile(s.restoreDir, id+mirror.JobRequestExt, reqData, 0o644); err != nil {
		return "", fmt.Errorf("serve: delete %s: stage request: %w", repoName, err)
	}
	s.log.Info("delete job staged for mirror-agent", "repo", repoName, "id", id)
	return id, nil
}

// handleDelete serves POST /v1/delete: serve stages a delete job for the
// git-context mirror-agent (no bundle — a delete has nothing to download or
// verify), waits for the agent's <id>.result under restoreResultDeadline, and
// cleans up the staged files. The agent removes /srv/git/<repo>.git ONLY; S3
// bundle mirrors are never touched, so the repo stays restorable via gitd
// mirror restore. Socket-only (never mounted behind the browse :443 mux),
// matching /v1/restore. 200 on success; 400 invalid repo / bad body; 503 busy
// when the channel is full (R12-Q1); staging/agent failures and the timeout
// surface as non-2xx with a clear message.
func (s *Serve) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req socket.DeleteRequest
	if err := decodeStrict(w, r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !repo.ValidName(req.Repo) {
		http.Error(w, "invalid repo name", http.StatusBadRequest)
		return
	}

	// Staging runs inside the worker (serialized with all other serve work);
	// the result wait runs in the request goroutine so the worker stays free
	// for bundles/deliveries while the agent works.
	reply := make(chan restoreStageResult, 1)
	act := func(sv *Serve) {
		jobID, err := sv.stageDeleteJob(req.Repo)
		reply <- restoreStageResult{id: jobID, err: err}
	}
	if err := s.submit(act); err != nil {
		writeServeError(w, err)
		return
	}
	var staged restoreStageResult
	select {
	case staged = <-reply:
	case <-r.Context().Done():
		// Client disconnected; the staging action still completes and the
		// buffered reply is discarded.
		return
	}
	if staged.err != nil {
		writeServeError(w, staged.err)
		return
	}

	res, err := s.waitRestoreResult(staged.id)
	if err != nil {
		// Timeout / unreadable result: leave the staged files in place — the
		// agent still completes the delete and serve's startup sweep handles
		// leftovers.
		writeServeError(w, err)
		return
	}
	s.cleanupRestoreJob(staged.id)
	if !res.OK {
		writeServeError(w, fmt.Errorf("delete failed: %s", res.Message))
		return
	}
	w.WriteHeader(http.StatusOK)
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
