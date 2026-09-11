# Restore and Delete a Repository (Phase 8.1 runbooks)

Two operational procedures on the repo layer, both **container-only** — the
admin shell image has no `aws` CLI (R6-Q9), so every operation below runs via
`gitd` subcommands, never raw S3.

- Restoring a repo from its S3 bundle mirror (`gitd mirror restore <repo>`,
  R8-Q2/R11-Q5)
- Deleting a repo outright (manual admin/host procedure, R5-Q10)

## 1. Restore a repo from S3

S3 is the canonical durable copy; the EBS `/srv/git` is disposable. When the
local repo is lost (instance rebuild, accidental deletion), restore the latest
bundle into a fresh bare repo under `/srv/git`.

### The restore sequence (R11-Q5)

`gitd mirror restore <repo>` implements exactly this order. The destination is
**always** `/srv/git/<repo>.git` — there is no `<dest>` argument:

1. **Serve downloads + verifies the latest bundle** (serve has the instance
   role + S3), stages it as `<id>.bundle` in `/var/spool/gitd/restore/`, and
   writes the JSON job `<id>.request` for the restore agent.
2. **The `gitd-restore` agent re-verifies the staged bundle itself** — `git
   bundle verify` against a fresh bare-repo context (defense in depth; it
   never trusts serve's prior verify).
3. **Destination must not exist.** The agent's `mirror.Restore` fails fast if
   `/srv/git/<repo>.git` already exists (`mirror fetch: destination ...
   already exists`). You cannot restore over an existing repo; no `--force`
   exists.
4. **Explicit sha256 init.** A fresh bare repo is created with
   `git init --bare --object-format=sha256 <dest>` — never relies on defaults
   (R10-Q4).
5. **Bundle verify.** The **latest** bundle is downloaded and `git bundle
   verify` runs against the fresh repo before anything is unbundled — the
   bundle's true "applies cleanly" check.
6. **Unbundle.** `git bundle unbundle` unpacks objects and the restore
   applies the listed refs. If HEAD is dangling after unbundle (init's default
   branch differs from the bundle's), it is repointed at the first restored
   branch so the restored repo always has a resolvable HEAD. The result is
   audit-logged (`repo restored from bundle`).

**No `git fsck` in v1** — `bundle verify` is the `fsck --full` equivalent on
the bundle, the restore target is empty so there are no prerequisites to check,
and the next push re-validates everything via `receive.fsckObjects` (R11-Q5).

### On the container shell

Get into the gitd container shell over SSH and run the mirror verbs **directly
as admin** — there is no `sudo` in the image. `gitd mirror restore` is
**serve-orchestrated**: the CLI submits it over the serve socket
(`POST /v1/restore` on `/var/spool/gitd/gitd.sock`); serve downloads +
verifies the bundle and stages a restore job; the **`gitd-restore` agent**
(the `gitd mirror-agent` daemon, a background role running in its own
container as the `git` user with no elevated caps) performs the `/srv/git`
write:

```sh
# Reach the admin fish shell (cert principals git,admin; R2-Q14).
ssh git@git.cmposer.cc
# list/delete run directly as admin (S3 through the instance role):
gitd mirror list my-repo
# list with no <repo> enumerates every repo that has mirrors (NDJSON):
gitd mirror list
# Restore is serve-orchestrated + agent-executed: submit to the serve socket;
# serve stages the job and the gitd-restore agent writes /srv/git as git.
# Requires gitd-serve AND gitd-restore to be running; takes no --sha256 and
# no dest.
gitd mirror restore <repo>
```

> Restore runs entirely in-container via the instance role — there is no
> `aws` CLI in the image (R6-Q9). The `gitd-restore` container performs the
> restore as the `git` user (`--user 1001:1001`, `/srv/git` mounted
> `rbind:rw`, no elevated caps), so the restored repo lands git-owned and the
> admin never needs elevation. The socket is 0770 `git:git` and admin is a
> `git` group member, so the CLI can connect. If `gitd-serve` is down, restore
> fails loudly (`socket ... /v1/restore` error); if `gitd-restore` is down,
> serve replies "restore still in progress (mirror-agent slow)" after its
> 4m30s internal deadline and the agent completes the restore when it comes
> back (leftovers are swept at serve's next startup).

Notes:

- `restore` takes exactly `<repo>` (the name, allowlist-validated) and
  restores into `/srv/git/<repo>.git` — the repo is live and recognizable,
  matching the `/srv/git/<name>.git` layout (R10-Q4).
- Serve never writes `/srv/git` (its container mounts it `rbind:ro` and lacks
  `CAP_CHOWN`); it stages the verified bundle + job request in
  `/var/spool/gitd/restore/` (setgid `git`, 2770), waits up to 4m30s for the
  agent's `<id>.result`, then removes the staged files. The agent re-verifies
  the staged bundle and writes `/srv/git/<repo>.git` via `mirror.Restore` —
  the destination is derived from the validated repo name, so an arbitrary
  dest is impossible.
- `<repo>` is validated against the repo-name allowlist
  `[A-Za-z0-9][A-Za-z0-9._-]{0,99}` (R2-Q1) — by serve at the socket AND again
  by the agent on the job (defense in depth against forged jobs).

### Verify

```sh
# Lists the bundle(s) — NDJSON {repo, bundles:[...]} (R13-Q6).
gitd mirror list my-repo
# `mirror list` with no <repo> lists every repo that has mirrors.
gitd mirror list
# The restored repo should push/pull like a normally-created one.
ssh git@git.cmposer.cc        # greeting
git clone git@git.cmposer.cc:my-repo.git /tmp/my-repo-clone
```

### Which bundle is "latest"?

The bundle key embeds a nanosecond RFC3339 timestamp
(`repos/<repo>/<RFC3339 with '-' for ':'>.bundle`, R11-Q3) that is generated
**inside the serialized actions-channel action at execution time** (R10-Q3), so
the last-executed bundle is always the newest repo state. Restore sorts keys
ascending and takes the last one. S3 object versioning keeps older writes, but
`mirror list`/`restore` operate on the current keys under the `repos/<prefix>/`
namespace.

### Eventual-mirror caveat (R13-Q2)

A push that times out at the 60s socket bound may still produce a bundle
afterwards — serve completes the upload action even after `notify`
disconnects. That means a restore immediately following such a push may or may
not include the very last refs; the mirror is eventually consistent. Re-push
is safe and idempotent and will produce a fresh authoritative bundle.

## 2. Weekly bundle verification (R8-Q3)

The serving daemon runs a periodic verification goroutine (default weekly,
`mirror.verify_interval`, 168h in `configs/gitd.yaml`). For each repo it Gets
the latest bundle, runs `git bundle verify`, and audit-logs the result;
failure is logged and may emit a webhook event. This is your backup-integrity
backstop — you do not need to run it by hand. To confirm it is healthy,
`journalctl -u gitd-serve` (host plane) should show `bundle verified` lines
for each repo with bundles on the configured cadence.

If you want an on-demand spot check, restore is itself a verify+restore, so a
restore into a scratch dir is a manual verification path — but the weekly loop
is the intended mechanism.

## 3. Deleting a repository (manual, split-plane)

Repo deletion is deliberately **not** a `gitd` subcommand in v1 (R5-Q10). It is
a manual two-part procedure. The purpose is to make deletion a conscious,
audited action rather than an API footgun. The live repo lives on host storage
(`/srv/git` is `0755 git:git`, so not even a `git` group member can write it
from the container) while the bundles live in S3 — so the two halves split
across planes:

Steps, in order:

1. Remove the local repo from the EBS store — **host plane** (SSM root shell,
   where `/srv/git` is writable):
   ```sh
   # SSM Session Manager root shell (docs/admin-split.md)
   rm -rf /srv/git/<repo>.git                     # remove the live repo
   ```

2. Remove the current bundle(s) from the S3 mirror — **data plane**, directly
   as admin (S3 through the instance role; no sudo exists in the image):
   ```sh
   ssh git@git.cmposer.cc                        # admin shell
   gitd mirror delete <repo>                      # deletes every current bundle
   ```
   `gitd mirror delete <repo>` lists the keys then deletes each via the
   objectstore `Store.Delete` seam (R6-Q9). Deleting a repo with **zero refs** /
   no bundles is a no-op that lists nothing.

3. Confirm:
   ```sh
   gitd mirror list <repo>                        # → {repo, bundles:[]}
   ```

**Noncurrent versions expire naturally.** The bucket has a 30-day
`NoncurrentVersionExpiration` lifecycle rule, so old bundle versions left
behind by prior pushes are garbage-collected automatically after 30d (R12-Q9)
— you only need to remove the current object(s) for the repo to be gone from
the `repos/` namespace. `gitd mirror delete` is scoped to the repo's current
keys.

### What this does / does not do

- It does remove the live repo and its reachable mirror bundles, so the repo
  is gone from both browse and push/pull after this procedure.
- It does **not** interact with `gitd spool` — spool events for past pushes to
  the repo remain until they deliver/die and the TTL/purge rules apply
  (R11-Q4). That is the audit trail, by design (R3-Q1: spool + S3 bundles are
  the audit trail).

### IAM in play

The container talks to S3 only through the instance role, which grants
`Put/List/Get/Delete` on `s3://<bucket>/repos/*` (R6-Q9). `mirror delete` and
`mirror restore` both run entirely in-container via that role — no `aws` CLI,
no extra creds (R7-Q3).
