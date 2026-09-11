# gitd — git.cmposer.cc

A minimal, single-user **personal git server on AWS** (`t4g.micro`, AL2023 arm64,
`us-east-2`), focused on post-quantum transport, durable S3 mirroring, an
mTLS-gated browse UI, and a small webhook plugin architecture — deployed via
CloudFormation at **~$7/month** ([cost model](docs/cost.md)).

It is the implementation of `plans/git.cmposer.cc.md` (that plan is gitignored /
internal). Module path `github.com/ChronicCmposer/gitd`, Go 1.26.x, Bazel
(`rules_go` 0.63.0 + gazelle 0.54.0) with a Makefile facade.

## What it is

| Surface | What | Gate |
|---------|------|------|
| `:22` SSH gateway | custom static **OpenSSH** with **hybrid-PQC kex** (`mlkem768x25519`/`sntrup761x25519`), SSH-CA mutual auth, GitHub-style greeting, push-to-create | SSH host + user certs (local CA) |
| S3 mirror | every push bundles `--all` → versioned `s3://git.cmposer.cc/repos/<repo>/<ts>.bundle`; weekly verify; restore via `gitd mirror restore <repo>` | instance role |
| `:443` browse | mTLS read-only UI (server/client/none render toggle, security headers, empty-repo pages) | client cert from the TLS CA |
| webhooks | compiled-in Go plugins (`http`, `logger`) + durable JSON spool, HMAC-SHA256, dead-letter + replay | plugin `type:` + HMAC secret |

The admin path is split (R2-Q16): **SSH lands in the container shell** for
data-plane ops (`gitd spool`/`mirror`, repo management); **SSM Session Manager**
(`ssm:StartSession`-only IAM) is the host plane (containerd, dnf, systemctl,
journalctl). See [admin-split](docs/admin-split.md).

## Architecture at a glance

- One self-contained VPC/stack: `cloudformation/stack.yaml` creates the VPC, SG
  (22 + 443 in, 443-only egress), versioned SSE-S3 bucket, IAM instance role,
  a `t4g.micro` with an auto-assigned public IP (DDNS keeps `git.cmposer.cc`
  pointed at it); `cloudformation/userdata.sh` boots it from a
  sha256-pinned deployment bundle.
- A **from-scratch OCI image** (rules_oci) holds `sshd` + `git` + `fish` +
  `sudo` + `ca-certificates` + `gitd`; three `ctr run --rm --net-host` systemd
  units (`gitd-sshd`, `gitd-serve`, `gitd-ddns`) share host bind mounts
  (`/srv/git`, `/var/spool/gitd`, `/etc/gitd`, `/home/admin`), hard-capped
  memory, `--rootfs-ro`.
- **gitd-serve** owns a strimserver-style actions channel (buffered chan +
  single worker) and an HTTP-over-unix-socket control plane (`/v1/bundle`,
  `/v1/deliver`); hook shims exec `gitd notify`/`gitd pre-receive`, which submit
  uploads/deliveries into that channel.
- Certs/CA live **client-side only** (`~/.ssh/gitd-ca/`, gitignored); SSM
  `/gitd/*` carries the issued material to the host (upload-certs.sh), with
  per-handshake reads for zero-downtime TLS rotation and passwordless renewal.

Post-quantum posture is deliberately split — **hybrid-PQC key exchange
everywhere** (SSH kex + TLS 1.3 `X25519MLKEM768`), but **classical Ed25519 /
ECDSA P-256 authentication** (no PQC signatures in OpenSSH yet, RSA-4096 is
strictly worse). See the [quantum threat model](docs/quantum-threat-model.md).

## Layout

```
cmd/gitd/                entry point (dispatch in internal/cli)
internal/
  cli/                   gitd subcommand dispatch: serve, notify, pre-receive, spool, ddns, mirror, version
  sshcmd/                SSH_ORIGINAL_COMMAND tokenizer + git gateway + greeting
  config/                strict YAML (gitd.yaml, webhooks.yaml), SIGHUP reload
  event/ spool/          webhook event envelope + durable JSON spool (fsync, retries, dead-letter)
  webhook/               Plugin + PolicyPlugin interfaces, registry; plugins/{http,logger}, policies/nonfastforward
  objectstore/ s3/       consumer-side ObjectStore seam (Put/Get/List/Delete) + s3 impl
  mirror/                git bundle create/verify/restore (S3 mirror)
  serve/                 actions-channel daemon + unix-socket control server + sweep/verify loops
  browse/ render/        :443 mTLS browse UI + rendering
  socket/ disk/ gitenv/ repo/ version/   cross-cutting seams (unix-socket client, disk guard, fixed git env, repo rules, versioning)
cloudformation/          stack.yaml, userdata.sh, deploy.sh, upload-certs.sh, ddns-setup.md
configs/                 gitd.yaml + webhooks.yaml examples (R13-Q4, shipped verbatim at boot)
tools/dist/              deterministic dist pipeline (openssh/git/fish/sudo/ca-certs/containerd/runc/Go)
tools/release/           release orchestration: bump-version.sh, release.sh, workspace-status.sh, sign-artifact.sh, gitd-signing-key.asc
tools/ssh-ca/            SSH CA tooling + client setup
pki/                     TLS mTLS PKI tooling + renewal timers
image/                   from-scratch OCI image assembly
docs/                    runbooks (below)
```

## Build / test / run

The Makefile is a thin facade over Bazel + the dist pipeline
(`tools/dist/README.md` for the whole pipeline).

```sh
make build        # bazel build //...
make test         # bazel test //...
make fuzz         # fuzz the SSH_ORIGINAL_COMMAND tokenizer (bazel run rules_go test -fuzz=...)
make mutate       # mutation testing (Phase 9; full baseline-aware run)
make mutate-ci    # CI mutation gate (git-diff mode, zero new survivors)
make coverage     # statement coverage via the pinned rules_go SDK
```

Dist / image targets (need root or working unprivileged user namespaces +
network for chroot builds, authenticated `gh` CLI (`gh auth login`) + aws to
publish):

```sh
make check-openssh-dist check-git-dist ...   # build-twice determinism gates
make gen-dist-pins                           # regenerate //:dist_pins.bzl
make publish-openssh-dist publish-git-dist ... # GitHub-first, S3-fallback publish
make image-container                         # gitd-container.tar for ctr images import
make deploy ARGS="--key-name KP"   # CloudFormation deploy
```

### Versioning & releases (R1-Q11/Q14)

Git **tags are the source of truth** for versioning. The version is injected at
**link time** via Bazel stamping — `workspace-status.sh` emits `STABLE_VERSION`
(the exact release tag at `HEAD`, else `v0.0.0-devel`), `.bazelrc` sets
`build --stamp`, and the `gitd` binaries carry an `x_defs` that substitutes the
value into `internal/version.Version` at link time. There is
**no generated version code**; `go run ./cmd/gitd version` reports the
`v0.0.0-devel` dev fallback, while a stamped build embeds the real tag.

```sh
make version                          # print the current version (tag at HEAD or v0.0.0-devel)
make bump-version LEVEL=patch         # tag the next vX.Y.Z (major|minor|patch) + push to origin
make release                          # build the stamped binary + gitd-container.tar, print the update pin sha256
```

`make release` **requires `HEAD` to be exactly a release tag** (`git describe
--tags --exact-match` must succeed) so the binary embeds the exact tag — run
`make bump-version` first. It builds the stamped binary and image, **GPG-signs
`gitd-container.tar`** (`gitd-container.tar.asc`, operator key via
`GPG_KEY_ID`), prints the sha256 you use as the **out-of-band update pin** for
`make update` (R6-Q3), and optionally publishes the tar **and** `.asc` to the
`gitd-container` family release on `ChronicCmposer/gitd` when the `gh` CLI is
authenticated (a skipped/failed publish never fails the build).

### Artifact signing (GPG)

Every published artifact — the `gitd-container.tar` image **and** each dist
product tarball — carries a detached ASCII-armored GPG signature (`.asc`)
uploaded alongside it to the same GitHub release + S3 mirror. The consumers
verify provenance **before** trusting the pinned sha256:

- **Sign (build side):** `release.sh`, `deploy.sh`, `update.sh`, and
  `publish-dist.sh` call `sign_artifact` (shared helper
  `tools/release/sign-artifact.sh`), which uses the operator's key — `GPG_KEY_ID`
  if set, else gpg's default key. Signing fails loudly, so an unsigned artifact
  is never published.
- **Verify (consumer side):** `userdata.sh` at boot and `update.sh`'s host
  step verify the `.asc` against the **committed public key**
  (`tools/release/gitd-signing-key.asc`) in a throwaway GPG home, then check the
  pinned sha256. A missing `.asc` or a bad signature aborts (fail-fast) — the
  out-of-band sha256 pin remains the primary trust anchor, GPG adds provenance
  (supersedes the plan's R3-Q2 "no signature" note).
- **Key custody:** the **private** signing key lives only on the operator's
  client box (never in the repo — `.gitignore` guards `*.gpg`/`secring`/etc.
  under `tools/release/`). The committed `gitd-signing-key.asc` is public
  export-only material. To use your own key, export its public half over the
  committed file and keep its private half in your keyring, then sign with
  `GPG_KEY_ID=<fingerprint>`.

```sh
# verify an artifact's provenance manually (throwaway keyring, pinned key)
gpg --homedir "$(mktemp -d)" --import tools/release/gitd-signing-key.asc
gpg --verify <artifact>.asc <artifact>     # must print "Good signature"
```

### Running the CLI locally

All dispatch lives in `internal/cli`; entry point `cmd/gitd/main.go`.

```sh
go run ./cmd/gitd version                              # print the version
go run ./cmd/gitd serve  --config ...                  # gateway (SSH_CONNECTION) or daemon
go run ./cmd/gitd notify --config /etc/gitd/gitd.yaml  # post-receive hook
go run ./cmd/gitd pre-receive --config ...             # pre-receive hook
go run ./cmd/gitd spool list                           # list spool events (NDJSON)
go run ./cmd/gitd spool replay <id>                    # re-deliver one event
go run ./cmd/gitd spool purge                          # remove delivered+expired events
go run ./cmd/gitd mirror list <repo>                   # list S3 bundles
go run ./cmd/gitd mirror delete <repo>                 # delete a repo's bundles
go run ./cmd/gitd mirror restore <repo>           # restore from the latest bundle into /srv/git/<repo>.git (serve socket)
go run ./cmd/gitd ddns --config ...                    # Namecheap dynamic DNS refresh
```

Exit codes: `0` ok, `1` runtime, `2` usage; errors to stderr as
`gitd: <err>` (R1-Q5).

## Runbooks (`docs/`)

| Runbook | Covers |
|---------|--------|
| [deploy](docs/deploy.md) | prerequisites, `deploy.sh`, artifact sha256 pin + GPG provenance flow, boot/rollback, SSM access, post-boot verification |
| [restore-from-s3](docs/restore-from-s3.md) | `gitd mirror restore <repo>` (serve-orchestrated + gitd-restore agent, `/srv/git/<repo>.git`), weekly bundle verify, repo deletion |
| [cert-renewal](docs/cert-renewal.md) | TLS + SSH renewal (client timer → SSM → cert-sync; host-cert via update.sh) |
| [plugin-authoring](docs/plugin-authoring.md) | webhook `Plugin` interface, registry, http/logger, HMAC, config schema, delivery semantics |
| [openssh-upgrade](docs/openssh-upgrade.md) | bump pin, auth-identity patch, rebuild, determinism, republish (GPG-signed), in-place update |
| [admin-split](docs/admin-split.md) | container-shell data plane vs SSM host plane, `gitd` verb reference |
| [update](docs/update.md) | in-place `update.sh` flow (GPG verify + out-of-band sha256 pin, ctr import, restart) |
| [ca-loss-recovery](docs/ca-loss-recovery.md) | new CA + reissue + SSM + `@cert-authority` cutover |
| [quantum-threat-model](docs/quantum-threat-model.md) | hybrid-PQC kex vs classical signatures, revisit triggers |
| [verification](docs/verification.md) | end-to-end probes (ssh greeting + mTLS curl only) |
| [cost](docs/cost.md) | ~$7/mo breakdown |

## Configuration provenance (R13-Q4)

`configs/gitd.yaml` + `configs/webhooks.yaml` are the *example/authoritative
target* configs (matching the plan's Config Schemas appendix). `deploy.sh`
packages them into the deployment bundle and `userdata.sh` writes them
verbatim to `/etc/gitd` at first boot (the `host_allowlist` placeholder is
substituted with the instance's auto-assigned public IP). **After first boot
the host copies are authoritative** —
runtime edits are host-plane file edits + `ctr task kill --signal SIGHUP
gitd-serve` (fail-safe reload).

## Phase 9 — Mutation Testing Gate (MSI trend)

Phase 9 adds mutation testing and coverage gates as CI hardening (shipped in
`ff67e3c`):

- `jonbaldie/go-mutesting/v2` v2.7.9 pinned in `go_deps` (hermetic,
  rules_go-built binary), `make mutate` (full baseline-aware run) + `make
  mutate-ci` (selected `git-diff` mode, zero new survivors on changed lines).
- `make coverage` enforces >= 80% statement coverage per internal package with
  an explicit exceptions list.
- A `mutate.yml` GitHub Actions workflow: PR job (git-diff mode) + nightly full
  run. The nightly full-run **MSI (Mutation Score Indicator)** is reported as a
  `gitd-mutation-reports` CI artifact; the MSI trend table is populated from
  those artifacts.

> **MSI trend.** The nightly full-run MSI is published as the
> `gitd-mutation-reports` artifact in the nightly mutation workflow; the trend
> table below is populated from those runs once they accumulate. PR gates are
> enforced separately (contents: read token, no secrets in workflows, per
> R3-Q8).

## Related pointers

- Dist pipeline internals: `tools/dist/README.md`
- SSH CA + client setup: `tools/ssh-ca/README.md`
- TLS mTLS PKI + renewal: `pki/README.md`
- Namecheap DDNS one-time setup: `cloudformation/ddns-setup.md`
