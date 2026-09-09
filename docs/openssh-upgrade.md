# Upgrading the Custom OpenSSH Build (Phase 8.1 runbook)

git.cmposer.cc runs a **custom-built static OpenSSH** inside the OCI image —
AL2023's stock OpenSSH is 8.7p1 with no post-quantum kex, and the PQC-only
requirement (`mlkem768x25519-sha256` + `sntrup761x25519-sha512`) forces a
modern build (R2-Q17 decision). This runbook is the upgrade path for that
build: bump the pin, apply the auth-identity patch, rebuild, prove determinism,
republish, and update in place. It is accurate to `tools/dist/` as committed;
see `tools/dist/README.md` for the full pipeline layout.

## 1. Preconditions

- Root on the build host (the openssh build runs in an Alpine **musl** chroot;
  `make check-openssh-dist-deps` enforces `id -u` == 0 and the presence of
  `curl` + `tar`).
- A clean git checkout of this repo, a `GH_TOKEN` and `aws` credentials for
  the publish step, network access to the chroot bases (Alpine mirror) and
  upstream sources.
- **Do not** touch the running server until the artifacts are built, verified,
  and `update.sh` is ready — OpenSSH rides in the image and ships with the
  container update, not a hot host swap.

## 2. Bump the pin in `tools/dist/versions.sh`

`tools/dist/versions.sh` is the single source of truth for every pinned
artifact (version, URL, checksum). For OpenSSH:

```sh
OPENSSH_VERSION="10.5p1"                      # <- bump to the new release
OPENSSH_URL="https://cdn.openbsd.org/pub/OpenBSD/OpenSSH/portable/openssh-${OPENSSH_VERSION}.tar.gz"
OPENSSH_SHA256=""                             # filled from the tarball by check-dist
```

`OPENSSH_SHA256` is intentionally empty here — `check-dist.sh` derives it from
the built tarball rather than trusting a pre-written value. The
`SOURCE_DATE_EPOCH` at the top of the file is fixed at the current release's
timestamp; per the file's own comment, **bump it only when a product pin
changes** so the build stays reproducible. If the new OpenSSH release wraps a
new build toolchain interaction, revisit it.

Note the chroot **base** pin too: `ALPINE_BRANCH` / `ALPINE_MIRROR` control
the musl toolchain the build links against. Only change these if the new
OpenSSH needs a newer toolchain — otherwise leave them so determinism
continues to compare like-for-like.

## 3. Apply the auth-identity patch (R2-Q6)

The patch `tools/dist/patches/openssh-10.5p1-auth-identity.patch` teaches
`session.c:do_setup_env` to export **`SSH_AUTH_KEY_FP`**, **`SSH_AUTH_KEY_TYPE`**,
and **`SSH_AUTH_CERT_ID`** from the authenticated key into the forced-command
child environment. gitd reads these in every git session, and **fails closed
if they're missing** (R10-Q5) — so the patch is not optional: an upgrade that
drops it becomes a visible regression the moment anyone pushes.

`build-openssh.sh` applies the patch via `patch -p1` against the pristine
<version> tree. When bumping to a **new** upstream version, the patch context
may drift; fix the hunk against the new `session.c` **before** proceeding. The
determinism check (`make check-openssh-dist`) will not pass if the patch does
not apply cleanly and reproducibly, so fixing the patch is part of the pin
bump, not an afterthought.

If you bump to a version where the patch file name no longer matches, rename
the patch to the new version and keep it under `tools/dist/patches/`.

## 4. Rebuild via `tools/dist/build-openssh.sh`

```sh
make check-openssh-dist        # build-openssh.sh twice, byte-compare the tarballs
```

`make check-openssh-dist` runs `tools/dist/check-dist.sh openssh`, which builds
the openssh product **twice** into separate directories and compares the sha256
of every output. A mismatch fails loudly and blocks publish. This is the
determinism gate (R3-Q2, plan 2.3): `SOURCE_DATE_EPOCH` is pinned, the tarball
is produced with `--sort=name --mtime=@$SOURCE_DATE_EPOCH --owner=0 --group=0`
and `gzip -n` so no timestamp leaks in.

## 5. Regenerate the Bazel pins

The Bazel repo rule for the openssh dist archive (`@openssh_dist_<arch>` in
`//:dist.bzl`) consumes the sha256 pins in `//:dist_pins.bzl`. After a
successful build-twice determinism check, regenerate them from the verified
tarballs and commit:

```sh
make gen-dist-pins       # rewrites //:dist_pins.bzl from tools/dist/out tarballs
git add tools/dist/versions.sh tools/dist/patches/ dist_pins.bzl
git commit
```

The commit is the `RepoRefSha` the next deploy/update will reference (R3-Q2).

## 6. Republish the dist artifact

```sh
make publish-openssh-dist   # tools/dist/publish-dist.sh openssh
```

This uploads the determinism-checked artifact to the GitHub mirror tag
(`openssh-<version>` on `ChronicCmposer/gitd-dist`, primary, R3-Q2) and to
`s3://git.cmposer.cc/openssh/` (fallback). Before uploading, the artifact is
**GPG-signed** (detached ASCII-armored `.asc`, operator key via `GPG_KEY_ID`)
and the `.asc` is uploaded alongside on both channels — consumers (boot,
update, and the Bazel fetch review flow) verify provenance against the
committed public key `tools/release/gitd-signing-key.asc`. Requires `GH_TOKEN`
+ `aws` CLI. Publishing is gated on having a checked artifact; an unpublished
or mis-pinned artifact would fail the Bazel fetch later.

## 7. Rebuild the image and update the sha256 pin

OpenSSH lives inside the OCI image only — there is no separate host-side
OpenSSH. So, after republishing the dist tarball:

```sh
make image-container      # rebuilds //image:image (rules_oci) from the new
                          # @openssh_dist_<arch> archive + package-image.sh
```

`make image-container` produces `tools/dist/out/gitd-container.tar`, whose
sha256 is what the server pins. The image tarball is published to the
`gitd-container` release + S3 by `deploy.sh` **or** by the `update.sh` flow.

**Pin flow (R3-Q2/R6-Q3):**

- `deploy.sh`/`update.sh` compute `IMAGE_SHA256` from the tarball and pass it
  as the CloudFormation `ImageSha256` parameter (deploy) or as the update
  sha256 pin (update). Both sign the image (`gitd-container.tar.asc`) before
  publishing and upload the `.asc` alongside.
- `userdata.sh` fetches the image **and its `.asc`**, verifies the GPG
  signature against the bundle's pinned public key first, then verifies the
  pinned hash (`verify_sha256`), and aborts boot on any failure — so a
  corrupted, tampered, or unauthenticated image can never silently land.
  `update.sh`'s host-side step applies the same GPG + sha256 checks.

## 8. Update in place

Do **not** tear down and re-create the instance for an upgrade. Use the
in-place `update.sh` flow (R3-Q10/R6-Q3, `docs/update.md`): fetch the new
`gitd-container.tar` **and its `.asc`**, verify the GPG signature against the
pinned public key, then the pinned sha256 (passed locally out-of-band, never
fetched from the artifact channel), `ctr images import`, restart the
container units. EBS `/srv/git` and the spool are preserved; there is no bundle
restore. The host keys stay the same across the whole sshd upgrade (the host
key/cert are on the host and overlaid into the container, not baked into the
image), so users' `known_hosts` `@cert-authority` pinning is unaffected.

The annual host-cert renewal also rides this same update window
(`tools/ssh-ca/ssh-ca renew` then `cloudformation/upload-certs.sh`, R12-Q6) —
see `docs/cert-renewal.md`.

## 9. Verify after upgrade

- `ssh git@git.cmposer.cc` → two-line greeting (identity + no-shell hint).
- `ssh -vv git@git.cmposer.cc` shows the negotiated **PQC kex**
  (`mlkem768x25519-sha256` with the 10.4p1 client).
- A push/pull roundtrip works, meaning the auth-identity env reached gitd
  (R10-Q5: git sessions fail closed if `SSH_AUTH_KEY_FP`/`CERT_ID` are missing).
- `gitd version` on the host matches the new build's link-time version (bazel
  stamping, R1-Q11) — the `RepoRefSha`-consistent build.
- No bundle-verify regressions: the post-receive mirror still uploads bundles
  after a push (bundle-in-S3 check in `docs/verification.md`).

## 10. Everything else that rides a full image build

A version bump only — not just OpenSSH — is the correct moment to re-run the
whole `tools/dist` check/publish set you touched. If you touched nothing else,
an OpenSSH-only rebuild is fine. But note the pipeline is integrated: `git`
(2.53.0, R2-Q13), `fish` (R2-Q18), `sudo` (R8-Q1), and `ca-certificates`
(R7-Q2) all ship in the same image and follow `versions.sh` + the same
`check-*-dist`/`gen-dist-pins`/`publish-*-dist` pattern. Keep the pins moving
in lockstep with the release cadence and publish them through the same
determinism gate before the image rebuild.
