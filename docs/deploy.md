# Deploying the gitd Stack (Phase 8.1 runbook)

This runbook walks the full deploy path for `git.cmposer.cc` — from a clean
checkout to a booted, verified stack. The orchestration lives in
`cloudformation/deploy.sh`; the instance boot is driven by
`cloudformation/userdata.sh` inside a deployment bundle, and the one-time
DDNS setup is documented in `cloudformation/ddns-setup.md`.

Order matters and is guaranteed by `deploy.sh` (R3-Q2/R3-Q3): the artifact
pins, the deployment bundle, the published image, and the SSM certs must all
exist **before** `create-stack`, because the instance pulls them at first
boot. This script enforces that ordering with early `set -euo pipefail` exits.

## 1. Prerequisites

These must all be true before the first deploy:

- **GitHub token.** `GH_TOKEN` set and exported in the environment, with
  `repo` scope over `ChronicCmposer/gitd-dist` (used to publish
  `gitd-container.tar` to the `gitd-container` release).
- **AWS credentials.** `aws` CLI configured with credentials for the deploy
  account/region (`us-east-2` by default). The instance role grants only
  read/`repos/` access to S3 and `ssm:GetParameter` on `/gitd/*`; your own
  credential must hold the broader `s3:PutObject` (bundle + image),
  `cloudformation:*`, and `ssm:PutParameter` on `/gitd/*` actions that
  `deploy.sh` and `upload-certs.sh` need.
- **Built + published dist/image artifacts.** The dist pipeline
  (`tools/dist/`, see `tools/dist/README.md`) must have produced and published
  the host binaries (`containerd-*.linux-<arch>.tar.gz`,
  `runc-*.linux-<arch>.tar.gz` under `tools/dist/out/`) and the OCI image
  (`tools/dist/out/gitd-container.tar`, via `make image-container`, or via the
  versioned `make bump-version LEVEL=<major|minor|patch>` + `make release`
  flow — see the README "Versioning & releases" section). `deploy.sh`
  fails fast if these are missing (see step 2).
- **EC2 keypair name** that already exists in the region (passed with
  `--key-name`). It trails `cloudformation/stack.yaml`'s
  `AWS::EC2::KeyPair::KeyName` as the SSM-host-plane emergency key. It is not
  the gitd sshd path (that is the SSH CA), but CloudFormation requires a
  keypair on the launch template.
- **A pre-allocated Elastic IP** in the region, and its allocation ID (passed
  with `--eip-allocation-id`). The EIP is attached by the stack and is the
  public address DDNS points `git.cmposer.cc` at.
- **Namecheap Dynamic DNS set up** (Phase 7.4, `cloudformation/ddns-setup.md`):
  host `git`, domain `cmposer.cc`, and the DDNS password stored in SSM at
  `/gitd/ddns/password` (SecureString). Run this before the first deploy.
- **Client-side PKI generated** (R3-Q3): the SSH CA + issued host cert
  (`tools/ssh-ca/ssh-ca init` + `issue-host`) and the TLS mTLS PKI
  (`pki/pki-new.sh`). `upload-certs.sh` refuses to run against missing or
  invalid material, so these must exist. The CA keys live in
  `~/.ssh/gitd-ca/` (0600, gitignored) — see `docs/ca-loss-recovery.md`.

## 2. Deploy

```sh
# From the repo root. Required flags are --key-name and --eip-allocation-id.
cloudformation/deploy.sh \
  --key-name sd-experiment-key \
  --eip-allocation-id eipalloc-XXXXXXXXXXXXXXXXX
```

`make deploy` is a thin facade:
`make deploy ARGS="--key-name <kp> --eip-allocation-id <id>"`.

### Flags

| Flag | Default | Required | Purpose |
|------|---------|----------|---------|
| `--stack-name` | `gitd` | no | CloudFormation stack name |
| `--region` | `us-east-2` | no | Region |
| `--key-name` | — | **yes** | EC2 keypair name |
| `--eip-allocation-id` | — | **yes** | Pre-allocated EIP allocation ID |
| `--instance-type` | `t4g.nano` | no | Instance type (arm64) |
| `--bucket` | `git.cmposer.cc` | no | S3 bucket (artifacts + repos + bundles) |
| `--image-tar` | `tools/dist/out/gitd-container.tar` | no | OCI image tarball |
| `--gitd-release-tag` | `gitd-container` | no | gitd-dist release tag |
| `--bundle-dir` | `cloudformation/out` | no | Bundle staging dir |

`--key-name` and `--eip-allocation-id` are enforced with a fail-fast exit
before anything else runs.

### The deploy pipeline (order matters)

`deploy.sh` runs these steps in order (R3-Q2/R3-Q3):

1. **Validate inputs.** `aws`, `gh`, `sha256sum`, `tar` must be on PATH;
   `GH_TOKEN` must be set; the image tarball, both host-binary tarballs, the
   configs, `userdata.sh`, `gitd-cert-sync.sh`, the containerd unit, and
   `config.toml` must all exist.
2. **Compute artifact sha256 pins** (R6-Q3 discipline): the image, containerd,
   and runc tarballs are hashed, plus the git HEAD commit
   (`RepoRefSha` — the RepoRef-style pin). These become the CloudFormation
   parameters the instance verifies at boot.
3. **Build the deployment bundle.** The bundle is staged from `userdata.sh`,
   `gitd.yaml`, `webhooks.yaml`, the host-binary tarballs,
   `containerd.service`, `config.toml`, `gitd-cert-sync.sh`, the image
   tarball **plus its `.asc`**, the **pinned GPG public key**
   (`tools/release/gitd-signing-key.asc`), and the shared `sign-artifact.sh`
   verify helper, then tarballed deterministically
   (`tar --sort=name --owner=0 --group=0 --numeric-owner`, gzip). A sha256 is
   taken over the tarball and it is stored in the bundle dir as
   `gitd-bundle-<sha256>.tar.gz` and uploaded to
   `s3://<bucket>/bundles/gitd-bundle-<sha256>.tar.gz`.
4. **Publish the image** to the `gitd-container` release on
   `ChronicCmposer/gitd-dist` (create if absent, `--clobber` upload if present)
   and to `s3://<bucket>/image/gitd-container.tar`. The image is GPG-signed
   first (`gitd-container.tar.asc`, operator key) and the `.asc` is uploaded
   alongside to both channels. The pinned `IMAGE_SHA256` is recorded in the
   release notes.
5. **Push certs to SSM** via `cloudformation/upload-certs.sh` (R3-Q3) — the
   TLS server material, the probe client cert, the SSH host key/cert, and the
   SSH CA public key, all SecureString under `/gitd/*`, encrypted with the
   default `aws/ssm` managed key (R5-Q8). This runs **before** create-stack so
   the instance can read its identity material at boot.
6. **Create (or update) the stack.** A `describe-stacks` check chooses update
   vs create. `create-stack` blocks until the instance's boot signal fires
   (`aws cloudformation wait stack-create-complete`) — that is the
   `CreationPolicy` gating `CREATE_COMPLETE`. Update runs
   `wait stack-update-complete`.

The stack write is otherwise standard CloudFormation (self-contained VPC, SG
22+443 open / egress 443-only, one t4g.nano AL2023 arm64 instance behind the
EIP, versioned SSE-S3 bucket with 30d noncurrent + 7d multipart lifecycle).

## 3. Artifact integrity + provenance flow

Integrity is a pinned-sha256 contract end to end (R3-Q2), with **GPG artifact
provenance layered ON TOP** (supersedes the plan's R3-Q2 "no signature"
decision — the pinned hash remains the primary trust anchor; the detached GPG
signature adds who-signed-it):

- `deploy.sh` hashes every payload it carries and passes the pins as
  CloudFormation **parameters** (`BundleSha256`, `ImageSha256`,
  `ContainerdSha256`, `RuncSha256`, plus `RepoRefSha`). It then **GPG-signs**
  `gitd-container.tar` (detached ASCII-armored `gitd-container.tar.asc`, using
  the operator's key — `GPG_KEY_ID`, else the default) and uploads the tar
  **and** the `.asc` to the `gitd-container` release and `s3://<bucket>/image/`.
- The **bundle** pin is checked in the stack's UserData bootstrap:
  `echo "<BundleSha256>  bundle.tar.gz | sha256sum -c -` aborts boot on
  mismatch.
- Inside `userdata.sh`, after fetching the image (GitHub primary / S3 fallback /
  bundle copy) **and** its `.asc` from the **same** source, it first verifies
  the GPG signature against the bundle's pinned public key
  (`gitd-signing-key.asc`, committed in-repo at `tools/release/` and packaged
  into the bundle by `deploy.sh`), then `verify_sha256 <expected> <file>` does a
  strict 64-hex fixed-length compare, then imports. Any failure — missing
  `.asc`, bad signature, malformed hash, hash mismatch — `die`s
  ("signature verification FAILED ... aborting boot") on the spot. **Signature
  verification is REQUIRED**: a missing `.asc` aborts boot, never falls through.
- The signing **private key never enters the repo or the bundle** — it stays on
  the operator's client box. The committed `gitd-signing-key.asc` is public
  (export-only) material; verification pins against it.

The out-of-band sha256 pin is unchanged: the hash is still supplied by the
operator (CFN parameters for deploy, `--sha256` for update) and never fetched
from the artifact channel.

## 4. Boot and rollback diagnostics

All first-boot output is teed to **`/var/log/gitd-userdata.log`** by the
bootstrap (`exec >/var/log/gitd-userdata.log 2>&1`). The bootstrap then runs
`userdata.sh` and cfn-signals the process exit code.

- **20-minute timeout.** `cloudformation/stack.yaml` sets
  `CreationPolicy: { ResourceSignal: { Count: 1, Timeout: PT20M } }` on the
  instance. `create-stack` will not reach `CREATE_COMPLETE` until exactly one
  signal arrives within 20 minutes. No signal ⇒ the stack rolls back
  (`DELETE_COMPLETE`), so a hang or a `die()` in userdata manifests as
  `stack-create-complete` waiting then failing.
- **Fail-fast semantics.** `userdata.sh` runs `set -euo pipefail` and every
  guard (`require_env`, `require_cmd`, `verify_sha256`, `ssm_get`) aborts
  boot with a `gitd: userdata: ...` line. The bootstrap always cfn-signals the
  exit code (`-e "$status"`), so a boot failure surfaces as a failed signal
  rather than a silent timeout. `CREATE_FAILED` rollback is the expected
  recovery: fix the cause, `make deploy` again.

### Reading the failure

The CloudFormation **stack events** name the failing resource and the signal
status. For the actual root cause, open a Session Manager shell to the
(canary) instance before rollback tears it down, or note that userdata never
completes and re-create after reading events. If the instance no longer
exists, the fastest path is usually: read the failing CFN event, fix the
underlying cause locally, and re-run `deploy.sh`.

Watch for the most common boot failures:

- GPG signature verification FAILED / `.asc` missing → the image was not
  signed by the key matching the bundle's pinned `gitd-signing-key.asc` (or the
  `.asc` was not published next to the tar). Rebuild + re-sign with the
  operator's key, republish tar **and** `.asc`, re-run deploy so the bundle
  carries a consistent public key.
- sha256 mismatch / empty image tarball → artifact keyed to the wrong build;
  rebuild + republish, re-run deploy so the pins update.
- `ctr images import failed` or
  `image import did not register git.cmposer.cc/gitd:latest` → the image
  tarball is not a valid OCI archive for this containerd.
- `browse mTLS liveness probe failed` → TLS material in `/gitd/server/*` or
  `/gitd/probe/*` is stale/broken; run `upload-certs.sh` and redeploy.
- `gitd-serve did not start` / `gitd-sshd did not start` → `journalctl -u
  gitd-serve` (host-plane) for the container command line; the ephemeral
  container logs go to the host journal.

## 5. Host-plane access (SSM Session Manager)

Admin access is split (R2-Q16, `docs/admin-split.md`): SSH lands in the
container shell for data-plane ops; the **host plane** (containerd, dnf,
systemctl, journalctl) is reached only through AWS Systems Manager Session
Manager, because the instance role grants **`ssm:StartSession` only** — no
`SendCommand` (R2-Q9).

```sh
INSTANCE_ID=$(aws cloudformation describe-stacks --stack-name gitd \
  --region us-east-2 --query \
  'Stacks[0].Outputs[?OutputKey==`GitdInstanceId`].OutputValue' --output text)

aws ssm start-session --target "$INSTANCE_ID" --region us-east-2
```

That drops you into a root shell on the host (a `t4g.nano` still needs its
512MiB budget kept free, R8-Q4). From here you run `systemctl`, `journalctl`,
`ctr`, `dnf` — the host-plane operations. The container console
(`ctr task attach` etc.) is generally **not** how you operate this stack; that
is the container shell's job.

## 6. Post-boot verification

`userdata.sh` already runs a boot liveness probe (R9-Q9) before cfn-signaling:
it checks `gitd-serve.service` and `gitd-sshd.service` are active and curls
the browse UI over mTLS with the probe client cert. After the stack reaches
`CREATE_COMPLETE`, run the full end-to-end verification in
`docs/verification.md`:

- `ssh git@git.cmposer.cc` → two-line greeting (identity + no-shell hint).
- push → pull roundtrip on a scratch repo.
- bundle present in S3 (`aws s3 ls s3://git.cmposer.cc/repos/<repo>/`).
- browse over mTLS with the device cert.
- webhook delivery (if a plugin target is configured).
- DDNS: `host git.cmposer.cc` resolves to the EIP.

The public IP is discoverable from the stack outputs (`GitdPublicIp`) and is
the value the EIP association pins; DDNS keeps the hostname current.

## 7. Notes and caveats

- **First run only for stage-4 items.** `setup-client.sh` and `pki/*.sh` are
  one-time bootstrap steps; everything they generate is gitignored and
  client-side.
- **Idempotent-ish.** Re-running `deploy.sh` with the same stack updates it
  in place (new bundle/pins → instance is NOT automatically rebooted by CFN;
  userdata only runs at first boot). To apply a new image/settings to a live
  stack use the `update.sh` flow (`docs/update.md`); CloudFormation stays for
  initial infra + emergency rebuild (R3-Q10).
- **Config provenance.** The deployed `gitd.yaml`/`webhooks.yaml` are the
  bundle copies written verbatim to `/etc/gitd/` at first boot (R13-Q4). After
  first boot the **host copies are authoritative** for runtime edits; do not
  expect `deploy.sh` to re-push config edits you've made live.
