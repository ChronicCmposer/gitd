# TLS + SSH Certificate Renewal (Phase 8.1 runbook)

Three certificate families are in play: the browse **TLS/mTLS** material
(server cert + client-CA pool + revocation list behind the `:443` browse UI),
and the **SSH** user + host certs signed by the local SSH CA. Renewal is split
between the client box (where the CAs live) and the host (which only consumes
SSM), with **zero downtime** throughout because every loaded value is
re-read per handshake.

## Cert lifetimes (plan decision table)

| Cert | Lifetime | Renewed by |
|------|----------|-----------|
| SSH **user** cert | 90d | `tools/ssh-ca` `issue-user` / `renew` (client box) |
| SSH **host** cert | 1y | annual `update.sh` via `ssh-ca renew` (R12-Q6) |
| TLS **server** cert | 90d | monthly client timer → SSM → host cert-sync |
| TLS **client** (device) cert | 30d | `pki/pki-new.sh` / re-issue (client) |
| TLS **CA** | 10y | re-key only on loss (see `docs/ca-loss-recovery.md`) |

All keys are **Ed25519** (SSH, R13-Q3) and **ECDSA P-256** (TLS/device, Phase 5
`internal/browse`); CA keys are passphrase-less, 0600, gitignored, and live
**only** on the client box (R3-Q4/R12-Q7).

## 1. TLS server-cert renewal flow (R12-Q6)

Three components cooperate, in order:

1. **Client-side renewal timer.** `pki/systemd/gitd-tls-renew.{service,timer}`
   runs `pki/renew-server-cert.sh` on the **1st of the month at 04:15** (the
   server cert is valid 90d; monthly renewal keeps it comfortably inside the
   window). The service file carries a `__REPO_ROOT__` placeholder you must
   replace with the absolute paths of this checkout before enabling it.
2. **SSM `/gitd/server/*`.** `renew-server-cert.sh` regenerates a **fresh**
   server key (keys rotate every renewal, never reused), re-issues the server
   cert against the existing local TLS CA, re-issues the CRL, fail-fast
   verifies both, then calls `cloudformation/upload-certs.sh` to push the
   refreshed material to SSM (SecureString, default `aws/ssm` key, R5-Q8).
   It needs `ssm:PutParameter` on `/gitd/server/*`.
3. **Host-side `gitd-cert-sync` timer.** The host runs
   `pki/gitd-cert-sync.sh` hourly (systemd `gitd-cert-sync.timer`, installed
   by userdata at R12-Q6) as root. It pulls `/gitd/server/*` (server cert/key,
   client-CA pool, revocation list) from SSM, stages + verifies them
   (server cert/key must be the same key pair, must chain to the client-CA
   pool, CRL must parse), then **atomically renames** them into `/etc/gitd/tls/`
   root:root. It never restarts anything.

**Zero downtime:** `browse` reads the server cert (via `tls.Config.GetCertificate`,
R5-Q6) and the client-CA pool + revocation list **on every handshake**
(R7-Q5). So the file replacement by `gitd-cert-sync` is transparent — the very
next handshake picks up the new material. No restart, no SIGHUP.

```sh
# Manual one-shot of the same flow (from the client box, where the CA lives):
pki/renew-server-cert.sh
```

Confirm the host picked it up:

```sh
# Host plane (SSM Session Manager, see docs/admin-split.md):
systemctl status gitd-cert-sync.timer        # last trigger
journalctl -u gitd-cert-sync.service | tail   # "refreshed from SSM (zero-downtime)"
```

A failed renewal is safe by construction: `renew-server-cert.sh` push failure
leaves the client certs renewed but **not** pushed (it tells you to rerun
`upload-certs.sh`), and `gitd-cert-sync.sh` refuses to install a broken pair —
it stages + verifies everything before the atomic rename, so a botched SSM
state never lands on the host.

## 2. SSH user-cert renewal

User certs are valid 90d. Re-issue with the local SSH CA on this box and
reinstall the cert into `~/.ssh/`:

```sh
# Re-sign an existing cert with a fresh window, preserving identity + principals.
tools/ssh-ca/ssh-ca renew ~/.ssh/gitd_ed25519-cert.pub

# Or issue a fresh cert over the underlying key (admin variant for the shell):
# tools/ssh-ca/ssh-ca issue-user --admin ~/.ssh/gitd_ed25519.pub
install -m 0600 ~/.ssh/gitd_ed25519-cert.pub ~/.ssh/
```

`renew` derives the underlying public key (`<key>-cert.pub` → `<key>.pub`),
reads the cert's principals + identity, and re-signs exactly those. The
**client** needs the new cert on every device; the server needs nothing (SSH
CA certs authenticate against `TrustedUserCAKeys`, which is already installed).
There is no server-side registration step. To expire existing sessions use
`revoke`, not new issuance.

## 3. SSH host-cert renewal (rides update.sh, R12-Q6)

The host cert is valid **1y** and its renewal is deliberately coupled to the
annual `update.sh` in-place update flow (`docs/update.md`), so a fresh host
cert ships with a fresh build rather than being pushed mid-life. On the client
box, alongside the update:

```sh
# Re-sign the existing host cert in place (principal git.cmposer.cc only, R10-Q10).
tools/ssh-ca/ssh-ca renew host/ssh_host_ed25519_key-cert.pub
# Push the refreshed host key + cert + CA to SSM:
cloudformation/upload-certs.sh
```

A boot-time/first-boot instance always pulls the then-current host material
from SSM (userdata reads `/gitd/host/*`, R3-Q3), so a renewed host cert also
bakes into any future emergency rebuild. No TOFU: the client pins the CA via
`@cert-authority git.cmposer.cc` in `known_hosts`, so the renewed host cert is
accepted automatically as long as it chains to the same CA.

## 4. Revocation (R2-Q4)

To refuse a specific key through the SSH CA, append its public key to the
**host's** `/etc/gitd/revoked_keys` — the sshd container mounts `/etc/gitd`
over `/etc/ssh` read-only (`userdata.sh`) and sshd's `RevokedKeys` directive
consults it on each authentication, so the revocation takes effect without a
restart. The file is created empty at boot (`root:root 0644`) and is
**host-plane** state: it is not reachable from the container (the sshd mount
is `ro`, and there is no passwordless sudo anywhere).

```sh
# Host plane (SSM root shell, docs/admin-split.md): append the offending key.
grep -Fqx '<the public key line>' /etc/gitd/revoked_keys || \
  echo '<the public key line>' >> /etc/gitd/revoked_keys
```

The client-box helper `tools/ssh-ca/ssh-ca revoke <pubkey>` appends to that
box's **own** `/etc/ssh/revoked_keys` — a local record only; it does NOT reach
git.cmposer.cc. There is no sync path for SSH revoked keys (only the TLS CRL
rides `upload-certs.sh`), so the host-plane append above is the actual
mechanism. It is an Ed25519-only guard and fail-fast on a duplicate.

On the **browse side**, revocation is the TLS CRL: revoke by re-issuing the
CRL (the CA index database tracks it), push via
`renew-server-cert.sh`/`upload-certs.sh`, and `gitd-cert-sync` replaces
`revoked.crl` on the next hourly run — enforced per handshake (R7-Q5). Browse
also accepts **any** valid CA-signed client cert (no CN allowlist, R9-Q8), so
the CRL is the mechanism for a lost device cert.

## 5. `tools/ssh-ca` subcommand reference

| Command | Purpose |
|---------|---------|
| `init` | Create the CA keypair + cert in `~/.ssh/gitd-ca/` (Ed25519, 0600). Refuses to clobber. |
| `issue-user [--admin] <pubkey>` | 90d user cert; `--admin` → principals `git,admin` (R2-Q14), else `git`. |
| `issue-host [OUT_DIR]` | Ed25519 host key + 1y host cert, principal `git.cmposer.cc` only (R10-Q10). |
| `renew <cert.pub>` | Re-issue with a fresh window, preserving identity + principals. |
| `revoke <pubkey>` | Append a key to THIS box's `/etc/ssh/revoked_keys` (local record only — server-side revocation is a host-plane append to `/etc/gitd/revoked_keys`, see above). |

Cert lifetimes and key types are constants in the tool (`lib.sh`) — it refuses
any non-Ed25519 key or an out-of-window request rather than accepting a
caller-supplied value.

## 6. What to keep running

- `gitd-tls-renew.timer` (client) — monthly TLS server renewal.
- `gitd-cert-sync.timer` (host) — hourly TLS material sync.
- `gitd-ddns.timer` (host) — 6h, unrelated to certs but part of the same
  maintenance picture.

Check them periodically via `systemctl status` on each side. A monthly glance
at `journalctl -u gitd-cert-sync.service` and an annual `ssh-ca renew` for the
host cert are the standing renewal cadence.
