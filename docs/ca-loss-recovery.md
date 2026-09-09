# CA Loss Recovery (Phase 8.1 runbook, R3-Q4)

The gitd CAs — the **SSH CA** (`~/.ssh/gitd-ca/ssh-ca`) and the **TLS CA**
(`~/.ssh/gitd-ca/tls-ca`) — live **only on the client box**. They are
passphrase-less, `0600`, gitignored, and deliberately have **no cloud backup**
(R3-Q4). This is an accepted-risk design: the client box is the single trust
boundary, and on its loss the whole trust domain must be rebuilt. This runbook
is the recovery procedure.

## 1. What CA custody means

- `~/.ssh/gitd-ca/` holds both CA keypairs:
  - `ssh-ca` (+ `.pub`) — signs SSH user + host certs (R2-Q4, R12-Q7)
  - `tls-ca.key` / `tls-ca.crt` + `tls-db/` — signs the browse TLS
    server/client certs and the CRL (R3-Q4/R12-Q7)
- Both are `0600`, owned by you, ignored by git (`.gitignore` entries cover the
  generated material; the scripts stay committed). The only things that ever
  leave the box are **public/issued** material pushed to SSM `/gitd/*` by
  `cloudformation/upload-certs.sh` (R5-Q8).
- **Consequence of loss:** nothing about the old CAs can be recovered, so
  every cert they ever signed (SSH user certs, the SSH host cert, the TLS CA
  cert and everything under it) must be replaced wholesale. Think of this as a
  **total rekey**, not a repair.

## 2. Recover: new CA + reissue + SSM + client trust (R3-Q4)

Perform the recovery in this order. Do the **client-side crypto first**, then
re-push to the server, then re-point the client trust.

### Step 1 — Create a new CA (client box)

```sh
# SSH CA (Ed25519):
tools/ssh-ca/ssh-ca init

# TLS mTLS PKI (ECDSA P-256 CA + server + client + CRL):
pki/pki-new.sh
```

Both refuse to clobber an existing CA, so a truly lost CA means the old files
are already gone and these run clean. Keep the newly generated keys in
`~/.ssh/gitd-ca/` `0600`, gitignored, as before.

> **Key change:** this is a new CA identity. Every host/device you ever
> connected with will *stop trusting* the old CA — that is the point; you are
> cutting over the trust domain.

### Step 2 — Reissue every SSH cert

Since no cert survives the CA, re-sign the user certs and the host cert with
the **new** CA:

```sh
# Admin + gateway user certs over the dedicated key(s):
tools/ssh-ca/ssh-ca issue-user --admin ~/.ssh/gitd_ed25519.pub     # principals git,admin
# (repeat for any other user keys; default principals git)

# Host key + 1y host cert (principal git.cmposer.cc only, R10-Q10):
tools/ssh-ca/ssh-ca issue-host host/
```

Install the upgraded user certs into the client `~/.ssh/` (0600) and reach the
server only via `git.cmposer.cc` (direct-EIP is unsupported, R10-Q10).

### Step 3 — Push the new material to SSM

The server reads its identity material from SSM, so the new CA's public/issued
material must replace the old **before** the server tries to authenticate with
it:

```sh
cloudformation/upload-certs.sh
```

This pushes `/gitd/server/*` (TLS server material + CRL + probe cert),
`/gitd/host/*` (SSH host key/cert), and `/gitd/ca/ssh-user-ca.pub` (the new SSH
CA public key → the server's `TrustedUserCAKeys`), all SecureString under the
default `aws/ssm` key (R5-Q8, R12-Q6). A missing/invalid source file fails
fast, so you are never left with a partial push.

### Step 4 — Get the new material onto the running host

For the changes to take effect on a live instance without a rebuild, refresh
the host material:

- **TLS:** the hourly `gitd-cert-sync` timer pulls `/gitd/server/*` → `/etc/gitd/tls/`
  and browse picks it up per-handshake (zero downtime, R12-Q6). You can trigger
  it manually: `systemctl start gitd-cert-sync.service` (host plane).
- **SSH CA + host cert:** the host reads `/gitd/host/*` and
  `/gitd/ca/ssh-user-ca.pub` at **boot** (userdata). On a live instance, either
  trigger the in-place update flow (`docs/update.md`) to refresh the running
  sshd's trusted-CA file and host cert, or — cleanest for a CA cutover —
  **rebuild** the stack (`cloudformation/deploy.md`), which re-runs userdata
  against the new SSM material. A full CA loss is one of the few legitimate
  reasons to reach for the emergency-rebuild (R3-Q10). If you must avoid a
  rebuild, refresh `/etc/gitd/trusted_user_ca_keys.pem` + the sshd host cert
  files on the host and restart `gitd-sshd` (host plane).

### Step 5 — Replace `@cert-authority` on the client

The client `known_hosts` pins the SSH CA via:

```
@cert-authority git.cmposer.cc <NEW CA PUBLIC KEY>
```

Replace the old key with the new one:
`tools/ssh-ca/ssh-ca init` prints the exact line to install. Without this, the
client offers the new (CA-signed) certs but does not recognize a server host
cert signed by the new CA. `setup-client.sh` is idempotent and backs up
`~/.ssh/config` + `known_hosts` before editing, so you can re-run it:
`tools/ssh-ca/setup-client.sh`.

Update the **TLS client trust** too: the new TLS CA means the browse `:443`
client cert (and the `~/.ssh`/`pki` copy the browser/client uses) must chain to
the new CA. Re-export/install the new `client-ca.crt` and a fresh device cert
to whatever client presents mTLS to the browse UI.

### Step 6 — Verify

Run `docs/verification.md` end to end:

- `ssh git@git.cmposer.cc` — greeting, then a push/pull roundtrip (both sides
  now trust the new CA).
- Browse over mTLS with the new device cert.
- `aws ssm get-parameter --name /gitd/ca/ssh-user-ca.pub` reflects the new key.
- Confirm the old CA's fingerprint nowhere in `known_hosts`/configs.

## 3. Prevent this from being worse than it is

- There is **no legal use of the old CA** after loss — a "maybe I backed it up
  somewhere" hesitation prolongs exposure. Execute the rekey promptly and
  completely.
- The cost structure (reissue every cert + update SSM + replace
  `@cert-authority` on every client + reinstall TLS trust) is exactly why the
  plan chose to ship this as a **documented runbook** rather than pretend the
  recovery is automatic. Budget ~30-45 minutes for a careful cutover on a
  single-user server.
- **Label/rotate deliberately.** The passphrase-less 0600 custody (R12-Q7) is
  what enables unattended renewals — which is why the *client box* itself is
  the security boundary. Treat loss of that box as requiring this runbook, and
  consider a periodic (annual) CA rotation a good practice so a real loss is
  never the first time you run this.
