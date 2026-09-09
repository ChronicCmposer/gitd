# tools/ssh-ca — gitd SSH Certificate Authority

Issues OpenSSH certificates for git.cmposer.cc over a local CA whose keypair
lives in `~/.ssh/gitd-ca/` (R3-Q4 / R12-Q7: passphrase-less, 0600, gitignored,
never leaves this box). The CA signs both the client **user** certs and the
server **host** cert.

Cert lifetimes (plan decision table): **SSH user 90d, host 1y**. Key types are
**Ed25519 only** (R13-Q3, R5-Q5) — the tool refuses any other key type.

## Commands

```
ssh-ca init                      # create the CA keypair + cert in ~/.ssh/gitd-ca/
ssh-ca issue-user [--admin] KEY  # 90d user cert; --admin -> principals git,admin
ssh-ca issue-host [OUT_DIR]      # Ed25519 host key + 1y host cert (principal git.cmposer.cc only)
ssh-ca renew CERT                # re-issue with a fresh window (same CA)
ssh-ca revoke KEY                # append key to /etc/ssh/revoked_keys (needs root/sudo)
```

### init
Creates the CA keypair. The printed `@cert-authority` line and
`TrustedUserCAKeys` line are the two ends the client and server consume.

### issue-user
Signs an existing Ed25519 public key into a 90d user cert.
- default: principal `git` (the gateway user the git protocol runs as).
- `--admin`: principals `git,admin` (R2-Q14 — the admin shell over the gateway).

The cert is written next to the key as `<key>-cert.pub`.

### issue-host
Generates an Ed25519 host key + a 1y host cert whose **only** principal is
`git.cmposer.cc` (R10-Q10 — the EIP is deliberately rejected; connect via the
hostname only, no TOFU). Output (default `host/`, gitignored):
`ssh_host_ed25519_key` (0600), `.pub`, `-cert.pub`.

### renew
Re-signs an existing cert with a fresh validity window, preserving its
identity and principals. Used for SSH user-cert renewal and the annual
host-cert renewal that rides `update.sh` (R12-Q6).

### revoke
Appends an Ed25519 public key to `/etc/ssh/revoked_keys` (R2-Q4). Needs write
access to `/etc/ssh` (root, or the admin user's passwordless sudo).

## Client setup (6.3)

`setup-client.sh` configures this box to connect to git.cmposer.cc:

1. Dedicated client keypair `~/.ssh/gitd_ed25519` (separate from the github key
   so the gitd cert is never offered to github.com, R2-Q14).
2. `~/.ssh/config` host block (User git, that key, `IdentitiesOnly yes`).
3. `@cert-authority git.cmposer.cc <CA>` line in `~/.ssh/known_hosts` (no TOFU).
4. An **admin** cert (principals `git,admin`) over the dedicated key.

It is idempotent and backs up `~/.ssh/config` and `known_hosts` before editing.
Committed reference blocks: `sample-ssh-config` and `sample-known-hosts`.

```
tools/ssh-ca/ssh-ca init
tools/ssh-ca/setup-client.sh
ssh git@git.cmposer.cc
```

## Environment

- Run from this box (the CA lives here; there is no cloud backup — see the
  CA-loss runbook, 8.1).
- `HOME` must be set; the CA lives under `$HOME/.ssh/gitd-ca/`.
