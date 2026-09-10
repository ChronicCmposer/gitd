# In-Place Update Flow — update.sh (Phase 8.1 runbook, R3-Q10/R6-Q3)

git.cmposer.cc is updated **in place** — build new artifacts, ship the new
image, restart the container units — with the EBS store and spool preserved.
CloudFormation is **not** the update path: it is for *initial infra* and
*emergency rebuilds only* (R3-Q10/R6-Q3), so you should not re-run `deploy.sh`
to push a routine upgrade to a live server.

> **`update.sh`:** the script described by R3-Q10/R6-Q3 is implemented at
> `cloudformation/update.sh` (Phase 8, `make update`), and this runbook is the
> faithful narrative of its flow. Run it from the client box as
> `make update ARGS="--sha256 <hex>"` (or
> `GITD_IMAGE_SHA256=<hex> cloudformation/update.sh`). It builds/reuses
> `gitd-container.tar`, verifies it against your out-of-band pin, **GPG-signs
> it**, publishes tar + `.asc` (GitHub primary + S3 fallback), then drives the
> host over SSM to fetch both, **verify the GPG signature against the pinned
> public key**, re-verify the same sha256 pin, `ctr images import`, and restart
> the three units.

## 1. What an update moves

- **In:** a new `gitd-container.tar` OCI image (carries `sshd`, `git`, `fish`,
  `sudo`, `gitd`, `ca-certificates` — a full build per `tools/dist`). Optionally
  the host-binaries (`containerd`/`runc`) and renewed host cert, if you're
  doing a coordinated bump.
- **Unchanged (preserved, never restored):** the EBS store `/srv/git`, the
  spool `/var/spool/gitd`, and the live `gitd.yaml` / `webhooks.yaml`.
- **Not touched:** the S3 bundle mirror — an update does **not** do a bundle
  restore. Mirrors are a backup, not a deploy input (R3-Q10).

## 2. The flow (R3-Q10)

1. **Build the new artifacts.** New image, and if the pin changed, new host
   binaries — each through `make <product>-dist` with the determinism gate,
   then `make image-container` to produce
   `tools/dist/out/gitd-container.tar` (`docs/openssh-upgrade.md`,
   `tools/dist/README.md`). For a normal versioned release use the coordinated
   path instead: `make bump-version LEVEL=<major|minor|patch>` then
   `make release`, which builds the stamped `gitd` binary from the exact tag,
   produces the same `gitd-container.tar`, **GPG-signs it**
   (`gitd-container.tar.asc`), and prints the sha256. Note the resulting
   sha256.
2. **Upload the artifacts** to the artifact channel — GitHub Releases (primary,
   `gitd-container` tag) and/or `s3://git.cmposer.cc/image/` — so the host can
   fetch them. The image's detached `.asc` is uploaded **alongside** (the
   update script signs and uploads both). `update.sh` verifies the local
   tarball against the pinned sha256 **and** signs it before publishing;
   `publish-dist.sh` does the same for every product tarball.
3. **Publish the sha256 pin** *locally/out-of-band* (R6-Q3). This is the
   critical security property: the expected sha256 is passed to the update as
   an **argument or environment value on the host**, typed in by the operator
   from `make image-container`'s output (or the release notes). It is **never
   fetched from the artifact channel** — the integrity claim rests on you
   providing a trusted hash out of band, exactly like `deploy.sh`'s pins but
   local to the update instead of baked into CFN parameters (R3-Q2).
4. **Fetch the new `gitd-container.tar` to the host** (SSM host-plane shell,
   `docs/admin-split.md`) and verify it **twice**: first the GPG provenance,
   then the pinned hash:
   ```sh
   # host plane
   expected="<paste the sha256 printed by make image-container>"   # out-of-band
   curl -fsSL -o /opt/gitd-container.tar \
     https://github.com/ChronicCmposer/gitd/releases/download/gitd-container/gitd-container.tar
   curl -fsSL -o /opt/gitd-container.tar.asc \
     https://github.com/ChronicCmposer/gitd/releases/download/gitd-container/gitd-container.tar.asc
   # pinned public key (committed at tools/release/gitd-signing-key.asc)
   gpg --homedir "$(mktemp -d)" --import /opt/gitd-signing-key.asc
   gpg --verify /opt/gitd-container.tar.asc /opt/gitd-container.tar   # must print "Good signature"
   echo "$expected  /opt/gitd-container.tar" | sha256sum -c -   # must print: OK
   ```
   **GPG verification is REQUIRED** (provenance on top of the pinned sha256):
   a missing `.asc`, a bad signature, or a hash mismatch all abort here — never
   import an unverified image. (Fall back to
   `s3://git.cmposer.cc/image/gitd-container.tar` + `.asc` the same way if
   GitHub is down.)
5. **Import + swap.** With the verified image:
   ```sh
   ctr -n default images import --base-name git.cmposer.cc/gitd /opt/gitd-container.tar
   ctr -n default images ls | grep git.cmposer.cc/gitd:latest   # confirm registered
   systemctl restart gitd-sshd gitd-serve gitd-ddns.timer       # pick up the new image
   ```
   The three `gitd-*` units run `ctr run --rm --net-host` with
   `Restart=always`, so a restart re-launches the containers from the now
   imported `gitd-container.tar` (R2-Q15). Ordering `gitd-sshd After=gitd-serve`
   is handled by the units (R12-Q5), so restart serve first is fine.
6. **Verify** — `docs/verification.md`: ssh greeting, push/pull roundtrip,
   bundle still landing in S3, browse over mTLS. `Restart=always` + the
   automatic reboot guard (`gitd-reboot.timer`, R5-Q11) keep the whole thing
   self-healing, but confirm no unit flap.

## 3. Host cert renewal rides every annual update (R12-Q6)

Non-annual updates can skip this, but the **annual** update is where the SSH
host cert (1y lifetime) gets re-signed, so a fresh host cert ships with the
fresh build. From the client box alongside the update:

```sh
tools/ssh-ca/ssh-ca renew host/ssh_host_ed25519_key-cert.pub
cloudformation/upload-certs.sh       # push renewed host material to SSM /gitd/host/*
```

New/rebooted instances and the current one both read `/gitd/host/*` from SSM
at their next opportunity; the client's `@cert-authority` pinning means no
TOFU, so the swap is transparent (`docs/cert-renewal.md`).

## 4. Emergencies: when CloudFormation IS the path

CloudFormation stays for two cases only (R3-Q10):

- **Initial infra.** First `deploy.sh` run — `cloudformation/deploy.md`.
- **Emergency rebuild.** Instance loss / unrecoverable host corruption: re-run
  `deploy.sh`, which pushes the *current* bundle, pins, and SSM certs, then
  `create-stack` rebuilds the instance (EBS is then re-provisioned; the data
  live-store returns via a `gitd mirror fetch` restore from S3,
  `docs/restore-from-s3.md` — S3 is the canonical copy). Certs come from SSM
  `/gitd/*`, so the rebuild inherits current host/TLS identity.

For routine upgrades, do *not* rebuild — that path falls back to restoring from
S3 which is heavier and unnecessary when the durable EBS store is fine.

## 5. Partial-update hazards to avoid

- **Never import an unverified image.** The trust boundary is the **out-of-band
  sha256 pin** (R6-Q3) *plus* the **GPG signature** (provenance): if the `.asc`
  is missing, the signature is bad, or your out-of-band hash doesn't
  `sha256sum -c` cleanly, stop. `update.sh`'s host-side step enforces this
  order — GPG first, then the hash — and aborts on either failure.
- **Don't skip the determinism gate** on the build side — a non-reproducible
  image is a supply-chain smell (R3-Q2).
- **The same image is shared by all three units**, so a bad image breaks all of
  them together. Import + verify + spot-check one unit (start with a canary
  `ctr run --rm ... gitd version`) before restarting the fleet.
- **Configs are NOT updated by update.sh.** Runtime config edits are a
  separate, host-plane step (edit `/etc/gitd/*` + `ctr task kill --signal
  SIGHUP gitd-serve`, R13-Q4). updates do not clobber the authoritative host
  copies.
