# Admin Split: SSH Container Shell vs SSM Session Manager (Phase 8.1 runbook)

Administration of git.cmposer.cc is deliberately split across two planes
(R2-Q14/Q16). Knowing which plane a given operation belongs to is the single
most useful thing for operating this server.

| Plane | Reach | What it is for | Blocked from |
|-------|-------|----------------|--------------|
| **Data plane** — `ssh git@git.cmposer.cc` (admin cert) | The admin **fish** shell inside the sshd **container** | `gitd spool ...`, `gitd mirror ...`, `rm`/repo ops on `/srv/git` | The host OS, containerd, systemd |
| **Host plane** — SSM Session Manager | A **root shell on the host** (AL2023) | `containerd`/`ctr`, `dnf`, `systemctl`, `journalctl` | Nothing on the host (it is root) |

The git *gateway* user (`git`) is forced into `gitd serve` and never reaches a
shell; the *admin* user cert carries principals `git,admin`, which unlocks the
`ForceCommand` scoped `Match User git` (R10-Q1) and — on the `admin` principal —
the fish shell. This is a PowerShell-style split: SSH is for git data-plane
ops, SSM `StartSession` is for host-plane maintenance.

## 1. Data plane: the container shell (via SSH)

All git **data-lifecycle** operations belong here, and every one of them works
through `gitd` subcommands — the container image has **no** `aws` CLI, so raw
S3 is not an option in-container (R6-Q9). Admin elevation in the container is
a static musl `sudo` with a **scoped sudoers** baked into the image at build
time (R8-Q1): passwordless `sudo -u git` is allowed for exactly the gitd
data-plane verbs and nothing else.

```sh
# Admin cert (principals git,admin) over the dedicated ~/.ssh/gitd_ed25519 key.
ssh git@git.cmposer.cc
# Now inside the fish shell, you can sudo -u git over the scoped-verb list:
sudo -u git gitd spool list
```

### Allowed `sudo -u git` verbs (R8-Q1)

| Verb | Purpose |
|------|---------|
| `gitd spool list` | NDJSON dump of every spooled webhook event (R13-Q6) |
| `gitd spool replay <id>` | Re-deliver one event synchronously via `/v1/deliver` (R7-Q8, R12-Q2) |
| `gitd spool purge` | Remove delivered events past the retention TTL (R6-Q1, R11-Q4) |
| `gitd mirror list <repo>` | List a repo's S3 bundles (`{repo, bundles:[...]}`, NDJSON, R13-Q6) |
| `gitd mirror delete <repo>` | Delete a repo's current bundles (R6-Q9) |

> **`gitd mirror fetch` is not in the scoped sudoers list.** The baked sudoers
> (`image/fs/etc/sudoers`, R8-Q1) permits exactly the five verbs above —
> `mirror fetch` is **not** among them. If you need a restore
> (`docs/restore-from-s3.md`), confirm the intended sudoers scope in your
> build; at HEAD `17c97c2` the v1 scoped list matches R8-Q1 exactly
> (`list|replay|purge` for spool, `list|delete` for mirror) and does not include
> `fetch`.

### Plain operations in the shell (no sudo)

- `rm -rf /srv/git/<repo>.git` — hard repo deletion (the admin user can write
  `/srv/git` because it is mounted `rw` in the sshd container, R5-Q4). Always
  pair with `sudo -u git gitd mirror delete <repo>`; see `docs/restore-from-s3.md`.
- The spool directory `/var/spool/gitd` is 0700 `git:git` and event files are
  0600 `git:git` (R4-Q11), so spool file inspection also goes through the
  `sudo -u git gitd spool ...` verbs rather than naked file reads.

### `gitd` subcommand reference (Phase 3-8, `internal/cli`)

`gitd <verb>` dispatches from `internal/cli`; exit codes are `0 ok / 1 runtime
/ 2 usage`, errors on stderr as `gitd: <err>` (R1-Q5). The full verb set:

| Verb | Role |
|------|------|
| `serve` | The gateway ForceCommand **and** the daemon. With `SSH_CONNECTION` set it runs the sshcmd gateway (greeting, `git-upload-pack`/`git-receive-pack`); as the `gitd-serve` unit it runs the actions-channel server (socket `/v1/bundle` + `/v1/deliver`, spool sweep, weekly verify, startup catch-up) and the `:443` mTLS browse server (R10-Q1, R12-Q5). |
| `notify` | Post-receive hook: writes one spool event per ref line (R11-Q1), submits the bundle upload to serve over the socket, and runs sync-mode deliveries. |
| `pre-receive` | Pre-receive hook: strict stdin parse, statfs disk headroom, fail-closed policy engine (R9-Q7, R7-Q4, R5-Q1). |
| `spool` | `list` / `replay <id>` / `purge` of the webhook spool. |
| `ddns` | Refresh the Namecheap dynamic DNS record (6h timer; reads `ddns.password_file`, root). |
| `mirror` | `list <repo>` / `delete <repo>` / `fetch <repo> [dest]` (dest defaults to `/srv/git/<repo>.git`) of S3 bundle mirrors. |
| `version` | Print the link-time version string. |

## 2. Host plane: SSM Session Manager

Host maintenance is **SSM Session Manager only** — the instance IAM grants
`ssm:StartSession` and **no** `ssm:SendCommand` (R2-Q9), and the stock distro
sshd is masked (the only `:22` listener is our containerized sshd). To get a
host shell:

```sh
INSTANCE_ID=$(aws cloudformation describe-stacks --stack-name gitd \
  --region us-east-2 --query \
  'Stacks[0].Outputs[?OutputKey==`GitdInstanceId`].OutputValue' --output text)
aws ssm start-session --target "$INSTANCE_ID" --region us-east-2
```

You land as root on the AL2023 host. This is where the **host-plane** ops live:

- **containerd**: `systemctl status containerd`, `ctr -n default images ls`,
  `ctr -n default image import ...` (used by the update flow).
- **systemd units**: `systemctl status gitd-serve gitd-sshd gitd-ddns.timer
  gitd-cert-sync.timer gitd-reboot.timer`, restart, enable. The three container
  units (`gitd-serve`, `gitd-sshd`, `gitd-ddns`) are `After=containerd.service`
  and `gitd-sshd` is additionally `After=gitd-serve` (R12-Q5).
- **journalctl**: `journalctl -u gitd-serve`, `-u gitd-sshd`, `-u gitd-ddns`,
  `-u gitd-cert-sync.service`. In-container, the daemons' slog output goes to
  stderr → host journald (R1-Q2), so audit events (push summaries, delivery
  attempts, spool replays, ddns results, bundle verify results) appear here.
- **dnf**: package patching. Security updates are applied to SELinux-aligned
  host packages by the `dnf-automatic` security-only timer (R4-Q10); non-security
  updates are a **manual** host-plane action here. The daily `gitd-reboot`
  timer reboots automatically when applied security updates require it
  (R5-Q11).
- **/etc/gitd**: runtime config edits are host-plane file edits + a SIGHUP to
  `gitd-serve` (`ctr task kill --signal SIGHUP gitd-serve`), per R13-Q4/R8-Q6.
  The host copy is authoritative after first boot; a botched edit keeps the
  old config (fail-safe, R8-Q6). Do not mutate `/etc/gitd` from the container
  shell.

### Emergency access

The host is also reachable via the EC2 keypair named at deploy (`--key-name`)
as the SSM-host-plane emergency path — but SSM Session Manager is the
blessed route and the keypair exists primarily to satisfy CloudFormation's
launch template. Prefer SSM unless SSM itself is down.

## 3. Deciding which plane

Rule of thumb — **data in `/srv/git` or `/var/spool/gitd` → SSH container
shell; anything timed/systemd/host-OS → SSM.** Concretely:

| Task | Plane |
|------|-------|
| Reply to a dead-lettered webhook | SSH → `sudo -u git gitd spool replay <id>` |
| Repo restore from S3 | SSH → `sudo -u git gitd mirror fetch ...` |
| Delete a repo | SSH → `rm -rf` + `sudo -u git gitd mirror delete` |
| Inspect push/delivery audit logs | SSM → `journalctl -u gitd-serve` |
| Restart a container unit after a crash | SSM → `systemctl restart gitd-sshd` |
| In-place image update | SSM → fetch/verify/`ctr image import`/restart (`docs/update.md`) |
| Apply non-security host patches | SSM → `dnf update` (manual) |
| Edit + reload configs | SSM → file edit + `ctr task kill --signal SIGHUP gitd-serve` |

Remember the memory budget: `gitd-sshd` caps at 320MiB, `gitd-serve` 128MiB,
`gitd-ddns` 64MiB on a 1GiB `t4g.micro` (R8-Q4). Keep host-plane dnf/fish
activity light so a pathological git index-pack OOM fails cleanly instead of
starving containerd — that bound is the point of the caps.
