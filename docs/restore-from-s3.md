# Restore and Delete a Repository (Phase 8.1 runbooks)

Two operational procedures on the repo layer, both **container-only** — the
admin shell image has no `aws` CLI (R6-Q9), so every operation below runs via
`gitd` subcommands, never raw S3.

- Restoring a repo from its S3 bundle mirror (`gitd mirror fetch`, R8-Q2/R11-Q5)
- Deleting a repo outright (manual admin-shell procedure, R5-Q10)

## 1. Restore a repo from S3

S3 is the canonical durable copy; the EBS `/srv/git` is disposable. When the
local repo is lost (instance rebuild, accidental deletion), restore the latest
bundle into a fresh bare repo under `/srv/git`.

### The restore sequence (R11-Q5)

`gitd mirror fetch <repo> [dest]` implements exactly this order — `<dest>` is
optional and defaults to `/srv/git/<repo>.git`:

1. **Destination must not exist.** `Fetch` fails fast if the destination
   already exists (`mirror fetch: destination ... already exists`). You cannot
   restore over an existing repo; use a fresh path.
2. **Explicit sha256 init.** A fresh bare repo is created with
   `git init --bare --object-format=sha256 <dest>` — never relies on defaults
   (R10-Q4).
3. **Bundle verify.** The **latest** bundle is downloaded and `git bundle
   verify` runs against the fresh repo before anything is unbundled — the
   bundle's true "applies cleanly" check.
4. **Unbundle.** `git bundle unbundle` unpacks objects and the restore
   applies the listed refs. If HEAD is dangling after unbundle (init's default
   branch differs from the bundle's), it is repointed at the first restored
   branch so the restored repo always has a resolvable HEAD. The result is
   audit-logged (`repo restored from bundle`).

**No `git fsck` in v1** — `bundle verify` is the `fsck --full` equivalent on
the bundle, the restore target is empty so there are no prerequisites to check,
and the next push re-validates everything via `receive.fsckObjects` (R11-Q5).

### On the container shell

Get into the gitd container shell over SSH. `gitd mirror list` / `delete` are
in the **scoped sudoers** (`image/fs/etc/sudoers`, R8-Q1) and run through the
admin user's `sudo -u git` elevation:

```sh
# Reach the admin fish shell (cert principals git,admin; R2-Q14).
ssh git@git.cmposer.cc
# list/delete are within the scoped NOPASSWD sudoers (R8-Q1/R6-Q9):
sudo -u git gitd mirror list my-repo
```

> **`gitd mirror fetch` is a restore-path extension (R8-Q2/R11-Q5) and is
> deliberately NOT in the R8-Q1 scoped sudoers at HEAD `17c97c2`.** Confirm
> whether your build grants `sudo -u git gitd mirror fetch <repo>`; if
> not, run it as the git user directly from an admin-shell context your setup
> permits, or escalate the sudoers scope in the image build (then rebuild +
> update in place) before relying on the container-only restore. This is the
> one verb in the restore story you should verify against the actual build's
> sudoers.
>
> (Restore runs entirely in-container via the instance role — there is no
> `aws` CLI in the image, R6-Q9.)

Notes:

- `fetch` takes `<repo>` (the name, allowlist-validated) and an optional
  `<dest>` (the path to create). With no `<dest>`, it restores into
  `/srv/git/<repo>.git` by default so the repo is live and recognizable,
  matching the `/srv/git/<name>.git` layout (R10-Q4).
- Restoring to a path under a writable mount is required (`/srv/git` is `rw`
  in the sshd container). The bundle temp download lives under
  `/var/spool/gitd` (existing rw mount, R5-Q4/R8-Q3).
- `<repo>` is validated against the repo-name allowlist
  `[A-Za-z0-9][A-Za-z0-9._-]{0,99}` (R2-Q1).

### Verify

```sh
# Lists the bundle(s) — NDJSON {repo, bundles:[...]} (R13-Q6).
sudo -u git gitd mirror list my-repo
# The restored repo should push/pull like a normally-created one.
ssh git@git.cmposer.cc        # greeting
git clone git@git.cmposer.cc:my-repo.git /tmp/my-repo-clone
```

### Which bundle is "latest"?

The bundle key embeds a nanosecond RFC3339 timestamp
(`repos/<repo>/<RFC3339 with '-' for ':'>.bundle`, R11-Q3) that is generated
**inside the serialized actions-channel action at execution time** (R10-Q3), so
the last-executed bundle is always the newest repo state. `Fetch` sorts keys
ascending and takes the last one. S3 object versioning keeps older writes, but
`mirror list`/`fetch` operate on the current keys under the `repos/<prefix>/`
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

If you want an on-demand spot check, fetch is itself a verify+restore, so a
restore into a scratch dir is a manual verification path — but the weekly loop
is the intended mechanism.

## 3. Deleting a repository (manual, container-only)

Repo deletion is deliberately **not** a `gitd` subcommand in v1 (R5-Q10). It is
a manual two-part admin-shell procedure. The purpose is to make deletion a
conscious, audited action rather than an API footgun.

Steps, in order:

1. Remove the local repo from the EBS store:
   ```sh
   ssh git@git.cmposer.cc                        # admin shell
   rm -rf /srv/git/<repo>.git                     # remove the live repo
   ```
   (`/srv/git` is writable in the sshd container — the `rw` bind mount, R5-Q4.)

2. Remove the current bundle(s) from the S3 mirror:
   ```sh
   sudo -u git gitd mirror delete <repo>          # deletes every current bundle
   ```
   `gitd mirror delete <repo>` lists the keys then deletes each via the
   objectstore `Store.Delete` seam (R6-Q9). Deleting a repo with **zero refs** /
   no bundles is a no-op that lists nothing.

3. Confirm:
   ```sh
   sudo -u git gitd mirror list <repo>            # → {repo, bundles:[]}
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
`mirror fetch` both run entirely in-container via that role — no `aws` CLI,
no extra creds (R7-Q3).
