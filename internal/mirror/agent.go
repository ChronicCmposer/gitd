// The git-context restore agent.
//
// gitd-serve can no longer write /srv/git itself: its container mounts
// /srv/git rbind:ro and lacks CAP_CHOWN. Restores therefore move to a
// dedicated gitd-restore container that mounts /srv/git rbind:rw and runs as
// the git user (uid 1001) with no elevated caps. This file implements the
// gitd mirror-agent daemon that performs those restores, plus the mirror
// methods the agent and serve share.
//
// The handoff is a job spool under /var/spool/gitd/restore/:
//
//   - gitd-serve downloads the latest S3 bundle, verifies it, stages it as
//     <id>.bundle, and writes the JSON job <id>.request =
//     {"repo":"<name>","bundle":"<id>.bundle"} (the bundle before the
//     request, so a request event implies its bundle is complete).
//   - The agent scan-then-watches the spool with fsnotify (observer pattern,
//     no polling): on startup it processes every leftover <id>.request
//     (crash recovery), then reacts to new request files immediately.
//   - Each job re-validates the repo name, re-verifies the staged bundle
//     itself (defense in depth — never trust serve's prior verify), writes
//     the restored repo into /srv/git/<repo>.git ONLY via mirror.Restore
//     (the dest is derived from the validated repo name, so an arbitrary
//     destination is impossible by construction), and writes the outcome to
//     <id>.result.
//
// Stale-job ownership (who removes what):
//
//   - The agent CONSUMES its inputs: it removes <id>.request and <id>.bundle
//     before writing <id>.result, so a restart never re-triggers a processed
//     job and a crash between the two leaves nothing to re-run.
//   - Serve owns the final cleanup: after reading <id>.result it removes the
//     three files (idempotent).
//   - On serve startup, serve sweeps leftover restore/ files — jobs it was
//     orchestrating that it no longer tracks (crash leftovers, timed-out
//     waits) — so they never linger or re-trigger.
//   - On agent startup, the agent still processes leftover <id>.request
//     files: those may be legitimate crash leftovers (a job serve handed off
//     but never saw finish), and re-running one is safe because the repo
//     name is re-validated, the staged bundle is re-verified, and
//     mirror.Restore fails fast on an already-existing dest.
//   - The race between serve's startup sweep and the agent's startup scan is
//     benign in both orders: if serve sweeps first the agent finds nothing;
//     if the agent scans first the restore completes and serve's sweep then
//     removes whatever remains.
package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/ChronicCmposer/gitd/internal/repo"
)

// Restore job spool file names (the mirror-agent protocol). Serve stages
// <id>.bundle + <id>.request; the agent writes <id>.result; see the package
// comment for ownership.
const (
	JobBundleExt  = ".bundle"
	JobRequestExt = ".request"
	JobResultExt  = ".result"
)

// JobRequest is the restore job envelope gitd-serve writes to <id>.request:
// the repo to restore and the staged bundle file the agent must re-verify.
type JobRequest struct {
	Repo   string `json:"repo"`
	Bundle string `json:"bundle"` // "<id>.bundle" — must match the job id
}

// JobResult is the mirror-agent's outcome written to <id>.result: ok=true on
// a completed restore, otherwise an error message for serve to surface.
type JobResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// DecodeJobRequest parses a restore job request strictly (unknown fields
// fail loudly, matching the config/event decode discipline).
func DecodeJobRequest(data []byte) (*JobRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var req JobRequest
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("decode restore request: %w", err)
	}
	return &req, nil
}

// DecodeJobResult parses a restore job result strictly.
func DecodeJobResult(data []byte) (*JobResult, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var res JobResult
	if err := dec.Decode(&res); err != nil {
		return nil, fmt.Errorf("decode restore result: %w", err)
	}
	return &res, nil
}

// WriteJobFile durably writes data to dir/name with the job-spool discipline
// (temp file + fsync + rename + dir fsync), so a watcher never observes a
// half-written job file and a crash never leaves a torn file under the final
// name. Both serve (the request) and the agent (the result) use it.
func WriteJobFile(dir, name string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(perm); err != nil {
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
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename: %w", err)
	}
	return fsyncDir(dir)
}

// fsyncDir fsyncs dir so a rename into it is durable (job-spool discipline).
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// AgentConfig wires the mirror-agent daemon. All fields are required.
type AgentConfig struct {
	Mirror    *Mirror
	WorkDir   string // the restore job spool (/var/spool/gitd/restore)
	ReposRoot string // canonical repo store (/srv/git) — audit only
	Log       *slog.Logger
}

// Agent is the git-context restore daemon: it scan-then-watches WorkDir for
// restore jobs staged by gitd-serve and performs them with the git user's
// context (no elevated caps, /srv/git mounted rw).
type Agent struct {
	mirror    *Mirror
	workdir   string
	reposRoot string
	log       *slog.Logger
}

// NewAgent returns an Agent ready to Run.
func NewAgent(cfg AgentConfig) *Agent {
	return &Agent{mirror: cfg.Mirror, workdir: cfg.WorkDir, reposRoot: cfg.ReposRoot, log: cfg.Log}
}

// Run scan-then-watches the restore spool until ctx is canceled:
//
//  1. the fsnotify watcher is added BEFORE the startup scan, so a job
//     created during the scan is either seen by the scan or delivered as an
//     event — never lost (and the scan already removed the request it
//     processed, so the queued event for the same id is a harmless no-op);
//  2. every existing <id>.request is processed (crash recovery);
//  3. the watch loop reacts to new request files immediately.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.workdir, 0o770); err != nil {
		return fmt.Errorf("mirror-agent: mkdir %s: %w", a.workdir, err)
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("mirror-agent: fsnotify: %w", err)
	}
	defer watcher.Close()
	if err := watcher.Add(a.workdir); err != nil {
		return fmt.Errorf("mirror-agent: watch %s: %w", a.workdir, err)
	}
	if err := a.processExistingJobs(ctx); err != nil {
		return err
	}
	a.log.Info("mirror-agent: watching restore spool", "dir", a.workdir)
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			a.onEvent(ctx, ev)
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			a.log.Error("mirror-agent: watch error", "error", err)
		}
	}
}

// processExistingJobs processes every leftover <id>.request in the spool
// (crash recovery) before the watch loop takes over. Jobs found here are
// legitimate crash leftovers (see the package comment on stale-job
// ownership); each is processed exactly like a fresh event.
func (a *Agent) processExistingJobs(ctx context.Context) error {
	entries, err := os.ReadDir(a.workdir)
	if err != nil {
		return fmt.Errorf("mirror-agent: scan %s: %w", a.workdir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), JobRequestExt) {
			continue
		}
		id := strings.TrimSuffix(e.Name(), JobRequestExt)
		a.log.Info("mirror-agent: startup restore job", "id", id)
		a.processJob(ctx, id)
	}
	return nil
}

// onEvent reacts to a new or updated <id>.request in the spool. Only Create
// and Write ops on .request files start work; .bundle/.result/temp files and
// Remove/Rename events are ignored (serve stages the bundle BEFORE the
// request, so a request event implies its bundle is complete).
func (a *Agent) onEvent(ctx context.Context, ev fsnotify.Event) {
	if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}
	id, ok := requestJobID(ev.Name)
	if !ok {
		return
	}
	a.log.Info("mirror-agent: restore job event", "id", id, "op", ev.Op.String())
	a.processJob(ctx, id)
}

// requestJobID extracts the job id from a restore-spool request path
// (<dir>/<id>.request). It reports false for anything that is not a request
// file, so bundle/result/temp files never trigger work, and the id is always
// a plain filename component (no path separators).
func requestJobID(path string) (string, bool) {
	return strings.CutSuffix(filepath.Base(path), JobRequestExt)
}

// processJob runs one restore job: read the request, validate everything at
// the boundary, re-verify the staged bundle, restore via the mirror, and
// record the outcome. A request that is already gone (a queued event for a
// job the startup scan or a previous event consumed) is a no-op.
func (a *Agent) processJob(ctx context.Context, id string) {
	reqPath := filepath.Join(a.workdir, id+JobRequestExt)
	data, err := os.ReadFile(reqPath)
	if errors.Is(err, os.ErrNotExist) {
		return // already consumed
	}
	if err != nil {
		a.finish(id, "", false, fmt.Sprintf("read request: %v", err))
		return
	}
	req, err := DecodeJobRequest(data)
	if err != nil {
		a.finish(id, "", false, err.Error())
		return
	}
	a.restore(ctx, id, req)
}

// restore executes one validated job. Every trust decision is a guard clause
// at the top (early exit); anything untrusted fails the job without touching
// the repo store.
func (a *Agent) restore(ctx context.Context, id string, req *JobRequest) {
	// Only a validated repo name may reach the write path. mirror.Restore
	// derives the canonical dest reposRoot/<repo>.git from it, so an
	// arbitrary destination is impossible by construction.
	if !repo.ValidName(req.Repo) {
		a.finish(id, req.Repo, false, fmt.Sprintf("invalid repo name %q", req.Repo))
		return
	}
	// The job's bundle must be exactly "<id>.bundle": the id bounds the
	// staged-file path to the spool, so a forged job cannot name an
	// arbitrary file.
	if req.Bundle != id+JobBundleExt {
		a.finish(id, req.Repo, false, fmt.Sprintf("job bundle %q does not match job id %q", req.Bundle, id))
		return
	}
	bundlePath := filepath.Join(a.workdir, req.Bundle)
	// Defense in depth: re-verify the staged bundle ourselves — never trust
	// serve's prior verify. A corrupt or forged bundle halts the job before
	// any write.
	if err := a.mirror.VerifyBundleFile(ctx, bundlePath); err != nil {
		a.finish(id, req.Repo, false, fmt.Sprintf("bundle verify: %v", err))
		return
	}
	// Write the restored repo into reposRoot/<repo>.git ONLY. mirror.Restore
	// re-downloads the authoritative latest bundle and does init + verify +
	// unbundle + partial-failure cleanup; the staged bundle above is the
	// tamper-evident canary that gates this write.
	if err := a.mirror.Restore(ctx, req.Repo); err != nil {
		a.finish(id, req.Repo, false, err.Error())
		return
	}
	a.finish(id, req.Repo, true, "restore complete")
}

// finish records the job outcome in <id>.result (slog-audited with repo,
// bundle, ok/error) and consumes the job inputs. The inputs (<id>.request +
// <id>.bundle) are removed BEFORE the result is written: once <id>.result
// exists, serve owns the final cleanup, and a crash between the removal and
// the result write must not leave a request that a restart would re-run
// (re-running a completed restore would fail with "already exists" and
// overwrite the good result). The <id>.result is left for serve.
func (a *Agent) finish(id, repo string, ok bool, message string) {
	for _, name := range []string{id + JobRequestExt, id + JobBundleExt} {
		if err := os.Remove(filepath.Join(a.workdir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Warn("mirror-agent: consume job inputs", "id", id, "file", name, "error", err)
		}
	}
	data, err := json.Marshal(JobResult{OK: ok, Message: message})
	if err != nil {
		a.log.Error("mirror-agent: encode result", "id", id, "error", err)
		return
	}
	if err := WriteJobFile(a.workdir, id+JobResultExt, data, 0o644); err != nil {
		a.log.Error("mirror-agent: write result", "id", id, "error", err)
		return
	}
	a.log.Info("restore job finished", "id", id, "repo", repo, "bundle", id+JobBundleExt, "ok", ok, "message", message)
}

// LatestBundle downloads the latest bundle for repo and returns its bytes.
// It does NOT write the repo store: serve stages these bytes for the
// mirror-agent, which performs the actual restore. A repo with no bundles
// fails loudly, matching mirror.Restore semantics.
func (m *Mirror) LatestBundle(ctx context.Context, repoName string) ([]byte, error) {
	if !repo.ValidName(repoName) {
		return nil, fmt.Errorf("mirror latest-bundle: invalid repo name %q", repoName)
	}
	keys, err := m.List(ctx, repoName)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("mirror latest-bundle %s: no bundles", repoName)
	}
	latest := keys[len(keys)-1]
	data, err := m.store.Get(ctx, latest)
	if err != nil {
		return nil, fmt.Errorf("mirror latest-bundle %s: download %s: %w", repoName, latest, err)
	}
	m.log.Info("latest bundle downloaded for restore staging", "repo", repoName, "key", latest, "bytes", len(data))
	return data, nil
}

// VerifyBundleFile runs git bundle verify on the bundle file at path in a
// fresh bare-repo context — the same "applies cleanly" check Fetch uses. It
// is shared by serve (fail-fast staging verification) and the mirror-agent
// (defense-in-depth re-verification of the staged bundle). The scratch repo
// is initialized in the bundle's own object format (detected from the bundle
// header; fail closed on an unrecognized header), so both SHA-1 and SHA-256
// bundles verify correctly. The scratch repo lives under workDir and is
// removed before returning.
func (m *Mirror) VerifyBundleFile(ctx context.Context, path string) error {
	format, err := bundleObjectFormat(path)
	if err != nil {
		return fmt.Errorf("mirror verify-bundle: %w", err)
	}
	scratch, err := os.MkdirTemp(m.workDir, "verify-*")
	if err != nil {
		return fmt.Errorf("mirror verify-bundle: scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	repoDir := filepath.Join(scratch, "repo.git")
	if _, err := m.git.Run(ctx, "init", "--bare", "--object-format="+format, repoDir); err != nil {
		return fmt.Errorf("mirror verify-bundle: init: %w", err)
	}
	if _, err := m.git.RunIn(ctx, repoDir, "bundle", "verify", path); err != nil {
		return fmt.Errorf("mirror verify-bundle %s: %w", path, err)
	}
	return nil
}
