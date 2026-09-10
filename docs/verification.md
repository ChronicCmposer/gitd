# End-to-End Verification (Phase 8.2)

This runbook verifies the whole stack after a deploy, a reboot, or an in-place
update (`docs/update.md`). It exercises every surface the server exposes and
confirms S3 mirroring, browse, webhooks, and DDNS are all live.

> **Probes = ssh greeting + mTLS curl only (R9-Q9).** There is deliberately
> **no** unauthenticated endpoint — no `/healthz` or cert-less liveness
> surface. Every verification below goes through an authenticated path: the
> SSH gateway greeting, or an mTLS `curl` against `:443`. The only
> host-internal probe (boot liveness) uses the same mTLS curl with the `probe`
> client cert from SSM. If you find yourself reaching for a cert-less health
> check, you are using the wrong stack.

## 0. Prereqs on the client box

- The SSH CA `@cert-authority` + dedicated key + issued cert installed
  (`tools/ssh-ca/setup-client.sh`; admin variant for the shell).
- A browse **device** client cert that chains to the TLS CA (from `pki/`, 30d
  lifetime).
- `gh`/`aws`/`curl` in PATH for the S3/mTLS checks.

## 1. SSH greeting

```sh
ssh git@git.cmposer.cc
```

Expect exactly **two** lines (R12-Q8): the authenticated identity (cert id,
key fingerprint, client IP) and a no-shell-access / push-to-create hint.
*PQC kex check*: `ssh -vv git@git.cmposer.cc` should show
`mlkem768x25519-sha256` negotiated (R5-Q5).

The same cert signed as the **admin** variant (`issue-user --admin`, principals
`git,admin`) should land in the fish shell instead, proving the admin split
works (`docs/admin-split.md`).

## 2. Push / pull roundtrip

```sh
# Create a scratch repo and push it (push-to-create, R10-Q4).
mkdir /tmp/gitd-check && cd /tmp/gitd-check
git init
git config user.name  "verifier"
git config user.email "verifier@git.cmposer.cc"
echo "hello gitd" > README.md
git add README.md && git commit -m "verification push"
git remote add origin git@git.cmposer.cc:verification-check.git
git push -u origin main        # should create the repo server-side

# Clone it back fresh (pull).
cd /tmp && rm -rf gitd-check-clone
git clone git@git.cmposer.cc:verification-check.git /tmp/gitd-check-clone
cat /tmp/gitd-check-clone/README.md     # == hello gitd
```

Notes:

- Multi-ref pushes should produce one spool event per ref (R11-Q1), and the
  push should **succeed** only after the synchronous bundle upload returns
  (R5-Q2). A push error mentioning the socket/mirror means serve is down or
  the upload failed — do not keep going until the roundtrip is green.
- Clean up the scratch repo afterwards with the deletion procedure
  (`docs/restore-from-s3.md`) if you don't want it to persist.

## 3. Bundle in S3

```sh
# The post-receive mirror uploads s3://git.cmposer.cc/repos/<repo>/<ts>.bundle.
aws s3 ls s3://git.cmposer.cc/repos/verification-check.git/ --region us-east-2
```

Expect one or more `.bundle` objects (RFC3339-dash-nano keys, R11-Q3). To
double-check integrity from the host side, the weekly `mirror.verify_interval`
goroutine already `git bundle verify`s each repo's latest bundle audibly —
`journalctl -u gitd-serve` (host plane) should show `bundle verified`
(R8-Q3). A CLI view: `gitd mirror list verification-check.git` (container
shell) → `{repo, bundles:[...]}` (R13-Q6).

## 4. Browse over mTLS

The browse UI is `:443`, **require-and-verify client cert**, TLS 1.3 only
(R4-Q8), Host header allowlist of `git.cmposer.cc` (+ public IP + localhost, R8-Q5).
With the device cert and the TLS CA as `--cacert`:

```sh
curl --fail --silent --show-error --max-time 20 \
  --cacert pki/client-ca.crt \
  --cert pki/client.crt \
  --key  pki/client.key \
  https://git.cmposer.cc/ | head
```

- A repo list should render (server-render mode default, R3-Q4/R13-Q7 empty-repo
  handling for zero-ref repos).
- **mTLS is the gate**: the same curl *without* `--cert`/`--key` must fail
  (that is what "no unauthenticated endpoints" means, R9-Q9). Do not skip the
  negative check.
- Expected security headers on the response (R3-Q6): CSP `default-src 'self'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`,
  `X-Frame-Options: DENY`.

## 5. Webhook delivery

Requires at least one plugin configured. Two ways to assert delivery:

- **`logger` plugin (simplest).** With a `logger` plugin in `webhooks.yaml`
  (`configs/webhooks.yaml` reference), a push should produce an `Info` event in
  `journalctl -u gitd-serve` — metadata only, never subjects (R3-Q1).
- **`http` plugin.** Point a small loopback receiver at a URL template, push,
  and assert it received the signed POST:
  - Header `X-Gitd-Signature: sha256=<hex>` over the payload (R3-Q5); verify
    the HMAC with the receiver's copy of the secret.
  - Envelope fields kebab-case with `event-id` = the spool UUID (R5-Q7/R10-Q7).
  - URL placeholders `{repo}/{ref}/{event-id}` path-escaped (R13-Q5).
- **Dead-letter path.** A delivery to a plugin-id removed from `webhooks.yaml`
  should 404 and dead-letter the event (`plugin-id not configured`, R13-Q8) —
  verify with `gitd spool list` (container shell) showing state `dead`, then
  recover via re-adding the plugin + SIGHUP + `gitd spool replay <id>`.

## 6. DDNS update

Namecheap resolves `git.cmposer.cc` to the instance's public IP:

```sh
host git.cmposer.cc
# should return the stack's public IP (Outputs.GitdPublicIp)
```

The `gitd-ddns` container timer refreshes the record every 6h, per
`gitd.yaml ddns.interval` (the Namecheap records expire ~30d if unrefreshed).
Confirm it is healthy:

```sh
aws cloudformation describe-stacks --stack-name gitd --region us-east-2 \
  --query 'Stacks[0].Outputs[?OutputKey==`GitdPublicIp`].OutputValue' --output text
# host plane:
systemctl status gitd-ddns.timer
journalctl -u gitd-ddns.service | tail    # "ddns updated ... Good <ip>"
```

A stale record (> ~30d with no refresh) breaks `ssh git@git.cmposer.cc` and
browse — remember direct-IP is **not** supported (host cert principal +
Host allowlist both reject it, R10-Q10), so if DNS is stale everything seems
down at once and the fix is the DDNS timer, not a workaround.

## 7. When to run this

- After a **fresh deploy** (`docs/deploy.md` reaches `CREATE_COMPLETE`).
- After an **in-place update** (`docs/update.md`).
- After an **automatic kernel reboot** (`gitd-reboot.timer`, R5-Q11) or any
  host-plane restart of the `gitd-*` units.
- After **CA recovery / rekey** (`docs/ca-loss-recovery.md`) — every trust
  surface changed.

A paste-able one-liner suite:

```sh
# 1+2: greeting + auth (interactive), then:
git push origin HEAD:verification-check-main 2>&1 | tail -3
# 3:
aws s3 ls s3://git.cmposer.cc/repos/ --region us-east-2 | grep verification-check
# 4 (browse positive + negative):
curl --fail -sS --cacert pki/client-ca.crt --cert pki/client.crt --key pki/client.key \
  https://git.cmposer.cc/ -o /dev/null && echo BROWSE_OK
curl -sS --cacert pki/client-ca.crt https://git.cmposer.cc/ -o /dev/null \
  && echo "ERROR: unauthenticated browse should have failed" || echo NO_UNAUTH_OK
host git.cmposer.cc
```
