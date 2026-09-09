# Authoring a Webhook Plugin (Phase 8.1 runbook)

gitd webhooks are **compiled-in Go plugins** (Phase 4), not external scripts:
a plugin is a small Go package that implements one interface and registers a
constructor into the process-wide registry by name. This runbook describes the
`Plugin` interface, the registry/self-registration pattern, the two reference
plugins (`http`, `logger`), and the config surface a `webhooks.yaml` entry
exposes. It uses `internal/webhook` and its `plugins/` subpackages as the
ground truth.

> **Read the code first.** The canonical examples are
> `internal/webhook/plugins/http/http.go` and
> `internal/webhook/plugins/logger/logger.go`. Everything in this runbook is
> accurate to those files as committed at `17c97c2`.

## 1. The `Plugin` interface

```go
// internal/webhook/webhook.go
type Plugin interface {
    Deliver(ctx context.Context, ev *event.Event) error
}

type Deps struct {
    Log *slog.Logger
}
```

- `Deliver` delivers **one event envelope** to one destination.
- A `nil` return means the event was delivered (or intentionally filtered
  out — e.g. the `http` plugin's `repos` glob returns `nil` for a non-match).
- Any non-`nil` error is a delivery failure and flows into the retry /
  dead-letter machinery.
- `Deps` carries per-process dependencies a constructor needs at build time
  (today: the logger).

Construction is the `Constructor` shape:

```go
// internal/webhook/registry.go
type Constructor func(cfg config.PluginConfig, deps Deps) (Plugin, error)
```

Plugins are **rebuilt from the live, SIGHUP-reloadable config on every
delivery** (`internal/webhook/deliverer.go`), so a reloaded `webhooks.yaml`
and a rotated secret take effect immediately without restart (R8-Q6, R12-Q3).
Your constructor should therefore be cheap and hold no long-lived state built
from the config.

## 2. The registry and self-registration

`webhook.Default` is the process-wide registry:

```go
var Default = NewRegistry()   // in internal/webhook/registry.go
func (r *Registry) Register(name string, c Constructor)
func (r *Registry) Build(cfg config.PluginConfig, deps Deps) (Plugin, error)
```

- Each plugin package registers its constructor **in `init()`** under its
  `Name` (the `type:` value in `webhooks.yaml`) — the database/sql driver
  pattern.
- No wiring at the call site: `internal/cli/serve.go` blank-imports the
  plugin packages (`_ ".../plugins/http"`, `_ ".../plugins/logger"`) so their
  `init()` registrations are live in the `gitd` binary.
- `Register` **panics** on a duplicate name (two plugins claiming one type is
  an impossible programmer error).
- `Build` returns a hard error for an **unknown `type:`** — a mistyped
  `webhooks.yaml` type fails loudly, never silently no-ops.

Authoring a plugin therefore means: implement `Plugin`, implement a
`New(cfg config.PluginConfig, deps webhook.Deps) (webhook.Plugin, error)`
constructor, register it in `init()`, and make sure the binary imports it
(blank import in `internal/cli/serve.go`).

## 3. The reference plugins

### `logger` (`type: logger`)

The simplest plugin and the best starting template
(`internal/webhook/plugins/logger/logger.go`): it logs event **metadata**
(event-id, repo, ref, type, commit count) to slog and always succeeds. It never
logs commit **subjects** (R3-Q1: subjects live in spool payloads, the audit
trail). Use it as a no-network reference and for tests.

### `http` (`type: http`)

The only delivery plugin in v1 (`internal/webhook/plugins/http/http.go`). It
POSTs the event envelope to a URL template with an HMAC signature and a
hardened client. Its behavior is the model for any new delivery plugin:

- **URL template placeholders** `{repo}`, `{ref}`, `{event-id}` are substituted,
  then **RFC 3986 path-escaped** on substitution via `url.PathEscape` (R13-Q5):
  slashes in `{ref}` become `%2F`. The template is the **only** literal-slash
  source; the plugin does not construct path segments from event data.
- **HMAC-SHA256 auth** (R3-Q5): the payload is signed and sent as
  `X-Gitd-Signature: sha256=<hex>`. The secret is read from `secret_file` on
  **every delivery attempt** (R12-Q3) so rotation = replace the file, no
  SIGHUP. Constant-time compare is the receiver's job (`hmac.Equal`); the
  secret never leaves gitd and is never logged (R2-Q8).
- **Hardened HTTP client** (R7-Q1): redirects are never followed
  (`CheckRedirect → http.ErrUseLastResponse`, a 3xx is a delivery failure),
  response bodies are capped at 1MiB and closed immediately, there is a ~10s
  dial timeout, and TLS verification is **on by default** (`insecure_skip_verify`
  is opt-in, R2-Q8).
- **`repos` glob filter**: if non-empty, the repo name must `path.Match` one
  of the globs; a non-match returns `nil` (filtered, not an error).

### Non-fast-forward policy plugin (`policies/`)

Polices are a sibling pattern (`PolicyPlugin`, fail-closed pre-receive
evaluation) with the same registry/blank-import mechanics. See
`internal/webhook/policies/nonfastforward/` for the example guard, configured
via the `policies:` block in `gitd.yaml` (R9-Q5) — not `webhooks.yaml`.

## 4. The `webhooks.yaml` config surface (R10-Q6)

A plugin entry uses the schema pinned in the plan's Config Schemas appendix
(and `configs/webhooks.yaml`):

```yaml
plugins:
  - id: my-receiver          # unique plugin id (must match a configured plugin)
    type: http               # http | logger (the registry `Name`)
    url_template: https://example.com/hook/{repo}   # {repo}/{ref}/{event-id}; RFC 3986 path-escaped (R13-Q5)
    secret_file: /etc/gitd/webhook-secret           # file-path reference (R1-Q4), 0600; read per-attempt (R12-Q3)
    sync: false              # opt-in: delivers in the push path (blocks the push) via POST /v1/deliver (R10-Q2)
    repos: ["*"]             # glob filter on repo name
    timeout: 30s             # per-attempt (R2-Q8)
    retries: 3               # durable retry budget (R8-Q8)
    insecure_skip_verify: false   # TLS verify on by default (R2-Q8)
```

- The config protocol is **strict YAML**: kebab-case keys, `DisallowUnknownFields`
  (send a typo'd key and load fails), duration strings, fail-fast `validate()`
  with the file path in errors (R1-Q3).
- `webhooks.yaml` is `git:git 0600` on the host because the post-receive hook
  runs `gitd notify` as the `git` user (R6-Q5) and must read webhook secrets.

## 5. Delivery semantics you should design against

- **Serial FIFO per plugin.** Delivery runs inside the gitd-serve **actions
  channel** (single worker, global FIFO, R9-Q3/Q11), so each plugin sees events
  in push order; throughput is serialized by design (R8-Q7).
- **Durable retries + dead-letter.** On a failed attempt the spool increments
  `attempts` and schedules `next-retry-at` with the pinned backoff
  `30s → 5m → 30m, no jitter` (R11-Q4). Exhausting the budget dead-letters the
  event (state `dead`). Dead events are **never auto-purged** in v1; recovery
  is `gitd spool replay <id>` after fixing the cause (R6-Q1, R11-Q4).
- **Sync vs async.** A `sync: true` plugin is delivered from within `notify`,
  on the push path — it blocks the push up to `N × timeout` for multi-ref
  pushes (R11-Q1) and fails fast on the first failure (R12-Q10). Async plugins
  go through the normal spool/sweep path.
- **Unknown plugin-id is a FINAL failure (R13-Q8).** If a `/v1/deliver`
  references a plugin-id absent from the live `webhooks.yaml` (e.g. the plugin
  was removed by a reload), gitd-serve replies **404** and **dead-letters the
  event** with the audit log `plugin-id not configured` — it is never retried
  and never auto-purged. Recovery = add the plugin back → `SIGHUP` → `gitd
  spool replay <id>`. Design your receivers around the fact that an event may
  be replayed and dedupe on `event-id`.

## 6. The payload envelope (R5-Q7, R10-Q7, R6-Q6)

The delivered body is the event envelope, kebab-case:

```json
{
  "schema-version": 1,
  "event-id": "<spool filename UUID>",
  "created-at": "<RFC3339>",
  "repo": "<name>",
  "ref": "refs/heads/main",
  "type": "push",
  "commits": [{ "sha": "<hex>", "subject": "<first line>", "author": "<name> <email>" }]
}
```

- `type` ∈ `push` | `ref-created` | `ref-deleted`.
- `commits` is capped at ~50 (`--max-count=51`, R13-Q10).
- `event-id` is the spool filename UUID — **receivers can dedupe replays** on
  it (R5-Q7).
- Decode is **strict**: the server refuses unknown fields and an unknown newer
  `schema-version` rather than silently tolerating them (R8-Q10, R9-Q10).
  Schema evolution goes through `schema-version`, not silent tolerance.

## 7. Testing a plugin

Follow the existing table-driven / hand-rolled-fake style (stdlib testing only,
no testify; R1-Q9 / `internal/webhook` tests). The `http` plugin's tests point
the client at a local loopback receiver over plain HTTP (`insecure_skip_verify`)
to assert the exact payload, signature header, and redirect handling. The
`MemoryStore` in `internal/objectstore` and the socket fakes let you exercise a
plugin end to end without AWS. There is no e2e test (deliberately out of scope,
R1-Q9).
