#!/usr/bin/env bash
# cloudformation/userdata.sh — git.cmposer.cc first-boot script (R3-Q2, R4-Q10,
# R5-Q11, R6-Q5, R7-Q3/Q6, R8-Q4, R12-Q5/Q6, R13-Q4).
#
# Runs as root on the AL2023 arm64 instance, invoked by the stack's bootstrap
# (stack.yaml UserData) which has already:
#   - installed aws-cli + aws-cfn-bootstrap;
#   - downloaded the deployment bundle from S3, verified its sha256, and
#     extracted it to /opt/gitd-bundle;
#   - exported the CFN-derived inputs below.
#
# Inputs (environment):
#   STACK_NAME / REGION / RESOURCE_NAME   cfn-signal target (this resource)
#   BUCKET                                S3 bucket (artifacts + repos + bundles)
#   IMAGE_SHA256                          pinned sha256 of gitd-container.tar (R3-Q2)
#   CONTAINERD_SHA256 / RUNC_SHA256       pinned sha256 of host binaries (R3-Q2)
#   GITD_RELEASE_TAG                      gitd-container family release tag on ChronicCmposer/gitd for the image
#
# The EIP is discovered from IMDSv2 (public-ipv4) so the stack does not have to
# pass it in: it is substituted into gitd.yaml's host_allowlist at boot.
#
# Fail-fast by design (code-philosophy): set -euo pipefail, every artifact is
# integrity-checked before install, and any error aborts boot (the bootstrap
# cfn-signals the failure).

set -euo pipefail

# Fail-loud (code-philosophy): any unhandled non-zero exit prints the failing
# command + line number to stderr before aborting. The ERR trap does NOT fire
# for commands in if-conditions or after ||/&&, so guarded paths (require_env,
# `if ! command -v gpg`, `curl ... || die`) are unaffected.
trap 'echo "gitd: userdata: ERROR at line $LINENO (last command: $BASH_COMMAND)" >&2' ERR

# --- helpers ------------------------------------------------------------------
die() {
    echo "gitd: userdata: $*" >&2
    exit 1
}
require_env() {
    local name="$1"
    [[ -n "${!name:-}" ]] || die "required environment variable ${name} is unset"
}
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}
# verify_sha256 <expected-hex> <file> — abort boot on mismatch (R3-Q2).
verify_sha256() {
    local expected="$1" file="$2"
    require_cmd sha256sum
    local actual
    actual="$(sha256sum "${file}" | cut -d' ' -f1)"
    # Fixed-length 64-hex compare; never accept a truncated/partial match.
    [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || die "malformed expected sha256 '${expected}'"
    [[ "${actual}" == "${expected}" ]] || {
        die "sha256 mismatch for ${file}: expected ${expected}, got ${actual}; aborting boot"
    }
}
# ssm_get <param-name> writes the decrypted value of /gitd/<name> to stdout.
# Fails loud (code-philosophy) with the param name on any non-zero exit.
ssm_get() {
    local name="$1"
    local out
    out="$(aws ssm get-parameter --region "${REGION}" --name "/gitd/${name}" --with-decryption \
        --query Parameter.Value --output text)" || die "ssm_get failed for /gitd/${name}"
    printf '%s\n' "${out}"
}

# --- early exit: all required inputs present -----------------------------------
require_env STACK_NAME
require_env REGION
require_env RESOURCE_NAME
require_env BUCKET
require_env IMAGE_SHA256
require_env CONTAINERD_SHA256
require_env RUNC_SHA256
require_env GITD_RELEASE_TAG

require_cmd aws
require_cmd curl
require_cmd tar
require_cmd systemctl

BUNDLE_DIR="/opt/gitd-bundle"
ARCH="$(uname -m)"                                  # arm64 / aarch64
case "${ARCH}" in
    aarch64|arm64)  ARTIFACT_ARCH="arm64" ;;
    x86_64|amd64)   ARTIFACT_ARCH="amd64" ;;
    *) die "unsupported architecture: ${ARCH}" ;;
esac

echo "gitd: userdata: env checks + arch resolution OK (${ARTIFACT_ARCH})"

echo "gitd: userdata: discovering EIP from IMDSv2"
# --- discover the EIP from IMDSv2 (no CFN pass-through needed) ------------------
IMDS_TOKEN="$(curl -fsS -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")"
EIP="$(curl -fsS -H "X-aws-ec2-metadata-token: ${IMDS_TOKEN}" \
    "http://169.254.169.254/latest/meta-data/public-ipv4")" \
    || die "cannot discover public IP (EIP) from IMDSv2"
[[ "${EIP}" =~ ^[0-9.]+$ ]] || die "discovered public IP is not an IPv4 address: ${EIP}"

# --- install our containerd/runc/ctr (HOST binaries, R2-Q19) --------------------
# NO distro containerd/git/openssh is installed: the git/openssh/gitd/fish all
# live inside the OCI image; the host only runs containerd + ctr.
# resolve_one <glob> resolves a single matching file in the bundle, or dies.
resolve_one() {
    local glob="$1"
    local matches
    matches="$(find "${BUNDLE_DIR}" -maxdepth 1 -name "${glob##*/}" -type f 2>/dev/null || true)"
    [[ -n "${matches}" ]] || die "no bundle artifact matches '${glob}'"
    local n
    n="$(printf '%s\n' "${matches}" | wc -l)"
    [[ "${n}" -eq 1 ]] || die "expected exactly one bundle artifact matching '${glob}', found ${n}"
    printf '%s\n' "${matches}"
}

# install_host_tar <expected-sha256> <glob> — verify + extract a host tarball.
# The dist tarballs unpack to usr/local/bin/... at the top level (dist.bzl
# layout), so they extract to the filesystem root.
install_host_tar() {
    local expected="$1" glob="$2" file
    file="$(resolve_one "${glob}")"
    verify_sha256 "${expected}" "${file}"
    tar -xzf "${file}" -C /
    echo "gitd: installed host binary tarball: ${file}"
}

mkdir -p /usr/local/bin
install_host_tar "${CONTAINERD_SHA256}" "${BUNDLE_DIR}/containerd-*.linux-${ARTIFACT_ARCH}.tar.gz"
install_host_tar "${RUNC_SHA256}" "${BUNDLE_DIR}/runc-*.linux-${ARTIFACT_ARCH}.tar.gz"
for bin in containerd ctr containerd-shim-runc-v2 runc; do
    [[ -x "/usr/local/bin/${bin}" ]] || die "host binary ${bin} not installed"
done

echo "gitd: userdata: host binaries installed; starting containerd"
# Own systemd unit + config.toml (from tools/dist/containerd/, packaged by deploy.sh).
[[ -f "${BUNDLE_DIR}/containerd.service" ]] || die "containerd.service missing from bundle"
[[ -f "${BUNDLE_DIR}/config.toml" ]] || die "config.toml missing from bundle"
cp "${BUNDLE_DIR}/containerd.service" /etc/systemd/system/containerd.service
mkdir -p /etc/containerd
cp "${BUNDLE_DIR}/config.toml" /etc/containerd/config.toml
systemctl daemon-reload
systemctl enable --now containerd.service

# --- host users + directory skeleton ---------------------------------------------
# uids match the image (R2-Q15/R7-Q3): git 1001, admin 1000. The admin's real
# shell lives inside the container; the host account exists only to own
# /home/admin (a bind-mount target) with a matching uid.
# On AL2023 the default `ec2-user` owns uid/gid 1000, which collides with the
# image contract (admin must own uid/gid 1000). This is a headless git server
# that shells in as admin, so ec2-user is reassigned to a free uid/gid and
# admin takes 1000. ANY other owner of uid 1000 is an unexpected conflict:
# fail loud (code-philosophy) — silently proceeding would orphan /home/admin.
#
# GID/UID guards are NUMERIC (getent group <gid> / getent passwd <uid>), not
# by name (see commit 81b8c11): on AL2023 gid 1000/1001 may already exist under
# a DIFFERENT group name, and the contract only requires uid/gid admin=1000 and
# uid/gid git=1001 to exist as the user's primary gid — the owning group NAME is
# irrelevant. So guard by numeric gid/uid and let useradd -g <gid> reference
# whatever group already owns that gid.

# find_free_id <getent-db> <start-id> — smallest id >= start absent from the db.
find_free_id() {
    local db="$1" id="$2"
    while getent "${db}" "${id}" >/dev/null 2>&1; do
        id=$((id + 1))
    done
    printf '%s\n' "${id}"
}

uid_1000_owner="$(getent passwd 1000 2>/dev/null | cut -d: -f1 || true)"
gid_1000_group="$(getent group 1000 2>/dev/null | cut -d: -f1 || true)"

if [[ -n "${uid_1000_owner}" && "${uid_1000_owner}" != "admin" ]]; then
    if [[ "${uid_1000_owner}" == "ec2-user" ]]; then
        # Reclaim uid 1000 from ec2-user: reassign it (and, if its primary group
        # also holds gid 1000, that group too) to a free id so BOTH uid 1000 and
        # gid 1000 free up for admin. Start the search at 1002: uid/gid 1001 is
        # reserved for `git` below, so ec2-user must never land there.
        ec2_new_uid="$(find_free_id passwd 1002)"
        if [[ "${gid_1000_group}" == "ec2-user" ]]; then
            ec2_new_gid="$(find_free_id group 1002)"
            # groupmod before usermod keeps the moved group consistent with the
            # user's primary-gid reference; neither is locked or in use on a
            # fresh boot (no ec2-user sessions exist).
            groupmod -g "${ec2_new_gid}" ec2-user
            echo "gitd: userdata: reassigned ec2-user group gid 1000 -> ${ec2_new_gid}"
        fi
        usermod -u "${ec2_new_uid}" ec2-user
        echo "gitd: userdata: reassigned ec2-user uid 1000 -> ${ec2_new_uid}"
    else
        die "uid 1000 is owned by '${uid_1000_owner}' (not ec2-user or admin); refusing to proceed"
    fi
fi

# uid 1000 is now free unless admin already owns it. Ensure a group holds gid
# 1000 for admin's primary gid (recreate it if ec2-user's move freed it, or if
# it never existed).
if ! getent group 1000 >/dev/null; then
    groupadd -g 1000 admin
fi

if getent passwd 1000 >/dev/null; then
    # uid 1000 is owned by admin (reclaimed above, or already present).
    if ! getent passwd admin >/dev/null; then
        die "uid 1000 is owned by a user other than admin; refusing to proceed"
    fi
else
    if getent passwd admin >/dev/null; then
        # admin already exists but with a different uid: useradd below would
        # collide on the name — fail loud rather than guess.
        die "user 'admin' already exists with a uid != 1000; refusing to proceed"
    fi
    useradd -u 1000 -g 1000 -c "gitd admin" -m -d /home/admin -s /usr/sbin/nologin admin
fi
if ! getent group 1001 >/dev/null; then groupadd -g 1001 git; fi
if getent passwd 1001 >/dev/null; then
    if ! getent passwd git >/dev/null; then
        die "uid 1001 already exists under a different user; refusing to proceed"
    fi
else
    if getent passwd git >/dev/null; then
        die "user 'git' already exists with a uid != 1001; refusing to proceed"
    fi
    useradd -u 1001 -g 1001 -c "gitd git gateway" -d /var/spool/gitd -s /usr/sbin/nologin git
fi

mkdir -p /srv/git /var/spool/gitd /etc/gitd/tls /etc/gitd/auth_principals
chown git:git /srv/git /var/spool/gitd
chmod 0755 /srv/git
chmod 0700 /var/spool/gitd
mkdir -p /home/admin && chown admin:admin /home/admin && chmod 0700 /home/admin

# --- configs verbatim from the deployment bundle (R13-Q4) --------------------------
# gitd.yaml carries the <EIP injected at deploy> placeholder; substitute the
# discovered EIP into host_allowlist (R8-Q5).
[[ -f "${BUNDLE_DIR}/gitd.yaml" ]] || die "gitd.yaml missing from bundle"
sed -e "s/<EIP injected at deploy>/${EIP}/g" "${BUNDLE_DIR}/gitd.yaml" > /etc/gitd/gitd.yaml
[[ -f "${BUNDLE_DIR}/webhooks.yaml" ]] || die "webhooks.yaml missing from bundle"
cp "${BUNDLE_DIR}/webhooks.yaml" /etc/gitd/webhooks.yaml

echo "gitd: userdata: pulling SSH/TLS material from SSM"
# --- SSH material from SSM (R3-Q3) + sshd_config + auth_principals + revoked_keys --
# Host key + cert (Ed25519 only, R13-Q3) -> /etc/gitd (overlaid onto /etc/ssh
# in the sshd container).
ssm_get host/ssh_host_ed25519_key        > /etc/gitd/ssh_host_ed25519_key
ssm_get host/ssh_host_ed25519_key.pub    > /etc/gitd/ssh_host_ed25519_key.pub
ssm_get host/ssh_host_ed25519_key-cert.pub > /etc/gitd/ssh_host_ed25519_key-cert.pub
# SSH CA public key -> TrustedUserCAKeys (client-side CA, uploaded by deploy.sh).
ssm_get ca/ssh-user-ca.pub > /etc/gitd/trusted_user_ca_keys.pem

# sshd_config: authoritative host copy (mirrors the image's baked defaults).
cat > /etc/gitd/sshd_config <<'SSHD_EOF'
# gitd hardened sshd_config (authoritative host copy, overlaid onto /etc/ssh
# in the sshd container; R3-Q3 / R13-Q4). Sources: R2-Q3, R7-Q9, R4-Q6,
# R13-Q3, R2-Q14, R10-Q1.
Port 22
ListenAddress 0.0.0.0
Protocol 2
UsePAM no
PermitRootLogin no
PermitUserEnvironment no
PermitEmptyPasswords no
PasswordAuthentication no
KbdInteractiveAuthentication no
AuthenticationMethods publickey
PubkeyAuthentication yes
AllowAgentForwarding no
AllowTcpForwarding no
AllowStreamLocalForwarding no
PermitTunnel no
GatewayPorts no
X11Forwarding no
MaxAuthTries 3
LoginGraceTime 30
ClientAliveInterval 300
ClientAliveCountMax 3
MaxSessions 2
PerSourcePenalties yes
LogLevel VERBOSE
HostKey /etc/ssh/ssh_host_ed25519_key
TrustedUserCAKeys /etc/ssh/trusted_user_ca_keys.pem
AuthorizedPrincipalsFile /etc/ssh/auth_principals/%u
RevokedKeys /etc/ssh/revoked_keys
AuthorizedKeysFile none
Subsystem sftp none
AllowUsers git admin
Match User git
    ForceCommand /usr/local/bin/gitd serve
    PermitTTY no
SSHD_EOF

# Per-user CA principals (R2-Q14): admin cert carries principals git,admin;
# git cert carries principal git.
printf 'git,admin\n' > /etc/gitd/auth_principals/admin
printf 'git\n' > /etc/gitd/auth_principals/git
: > /etc/gitd/revoked_keys

# DDNS password from SSM (R7-Q3: gitd-ddns runs as root, reads root:root 0600).
mkdir -p /etc/gitd
ssm_get ddns/password > /etc/gitd/ddns-password

# --- TLS material from SSM (server cert/key + client-CA pool + CRL) --------------
# These back the browse :443 mTLS server; written now and refreshed hourly by
# gitd-cert-sync (R12-Q6). The probe client cert (pki/client.*) is used for the
# boot liveness probe.
ssm_get server/server.crt      > /etc/gitd/tls/server.crt
ssm_get server/server.key      > /etc/gitd/tls/server.key
ssm_get server/client-ca.crt   > /etc/gitd/tls/client-ca.crt
ssm_get server/revoked.crl     > /etc/gitd/tls/revoked.crl
ssm_get probe/client.crt       > /etc/gitd/tls/probe.crt
ssm_get probe/client.key       > /etc/gitd/tls/probe.key

# --- /etc/gitd ownership matrix (R6-Q5) -------------------------------------------
chown root:git  /etc/gitd/gitd.yaml && chmod 0640 /etc/gitd/gitd.yaml
chown git:git   /etc/gitd/webhooks.yaml && chmod 0600 /etc/gitd/webhooks.yaml
chown root:root /etc/gitd/tls/server.key /etc/gitd/tls/probe.key \
                /etc/gitd/ssh_host_ed25519_key /etc/gitd/ddns-password
chmod 0600 /etc/gitd/tls/server.key /etc/gitd/tls/probe.key \
           /etc/gitd/ssh_host_ed25519_key /etc/gitd/ddns-password
chown root:root /etc/gitd/tls/server.crt /etc/gitd/tls/client-ca.crt \
                /etc/gitd/tls/revoked.crl /etc/gitd/tls/probe.crt \
                /etc/gitd/ssh_host_ed25519_key.pub \
                /etc/gitd/ssh_host_ed25519_key-cert.pub \
                /etc/gitd/trusted_user_ca_keys.pem \
                /etc/gitd/sshd_config
chmod 0644 /etc/gitd/tls/server.crt /etc/gitd/tls/client-ca.crt \
           /etc/gitd/tls/revoked.crl /etc/gitd/tls/probe.crt \
           /etc/gitd/ssh_host_ed25519_key.pub \
           /etc/gitd/ssh_host_ed25519_key-cert.pub \
           /etc/gitd/trusted_user_ca_keys.pem \
           /etc/gitd/sshd_config
chown -R root:root /etc/gitd/auth_principals
chmod -R 0644 /etc/gitd/auth_principals
chown root:root /etc/gitd/revoked_keys && chmod 0644 /etc/gitd/revoked_keys
chmod 0700 /etc/gitd/tls

echo "gitd: userdata: fetching/verifying/importing OCI image"
# --- fetch + verify + import the OCI image (R3-Q2) ---------------------------------
# Primary: GitHub Releases; fallback: S3 (image/ prefix); final fallback: the
# bundle copy. Whichever source wins, the image is GPG-verified against the
# bundle's pinned public key (provenance) BEFORE the pinned sha256 (integrity),
# then imported. Any missing signature or verify mismatch aborts boot — a
# hardened trust chain never falls through to an unsigned image.
GPG_KEY_PIN="${BUNDLE_DIR}/gitd-signing-key.asc"
# Shared GPG verify helper packaged into the bundle by deploy.sh.
# shellcheck source=/dev/null
source "${BUNDLE_DIR}/sign-artifact.sh"
[[ -f "${GPG_KEY_PIN}" ]] || die "pinned GPG signing key missing from bundle: ${GPG_KEY_PIN}"

# GPG verification is REQUIRED (provenance on top of the pinned sha256). It is
# agent-free by design: verify_artifact imports the pinned PUBLIC key and checks
# the detached signature with --no-autostart, which needs NO gpg-agent (no
# secret-key operation). AL2023 ships gnupg2-minimal, whose gpg verifies
# signatures fine but whose package CONFLICTS with the full gnupg2 — so never
# install gnupg2 here. If gpg is somehow absent, install the non-conflicting
# gnupg2-minimal (the AL2023 default that provides gpg), then fail fast if gpg
# is still missing rather than silently trusting the image.
if ! command -v gpg >/dev/null 2>&1; then
    echo "gitd: image: gpg not found; installing gnupg2-minimal (agent-free verification, AL2023 default)" >&2
    dnf install -y -q gnupg2-minimal
fi
require_cmd gpg

IMAGE_TAR="${BUNDLE_DIR}/gitd-container.tar"
GITHUB_IMAGE_URL="https://github.com/ChronicCmposer/gitd/releases/download/${GITD_RELEASE_TAG}/gitd-container.tar"
GITHUB_IMAGE_ASC_URL="${GITHUB_IMAGE_URL}.asc"
S3_IMAGE_URL="s3://${BUCKET}/image/gitd-container.tar"
S3_IMAGE_ASC_URL="s3://${BUCKET}/image/gitd-container.tar.asc"
# Primary: GitHub Releases; fallback: S3; final fallback: the bundle copy. Either
# way the image and its detached .asc must both land from the SAME source; an
# image whose signature cannot be fetched is refused outright.
if curl -fsSL --retry 3 --connect-timeout 15 "${GITHUB_IMAGE_URL}" -o "${IMAGE_TAR}.dl"; then
    curl -fsSL --retry 3 --connect-timeout 15 "${GITHUB_IMAGE_ASC_URL}" -o "${IMAGE_TAR}.asc.dl" \
        || die "fetched gitd-container.tar from GitHub but its signature ${GITHUB_IMAGE_ASC_URL} is missing; refusing to trust an unsigned image"
    mv "${IMAGE_TAR}.dl" "${IMAGE_TAR}"
    mv "${IMAGE_TAR}.asc.dl" "${IMAGE_TAR}.asc"
    echo "gitd: image: downloaded from GitHub Releases (${GITD_RELEASE_TAG}) + .asc"
elif aws s3 cp "${S3_IMAGE_URL}" "${IMAGE_TAR}.dl" --region "${REGION}" --only-show-errors 2>/dev/null; then
    aws s3 cp "${S3_IMAGE_ASC_URL}" "${IMAGE_TAR}.asc.dl" --region "${REGION}" --only-show-errors 2>/dev/null \
        || die "fetched gitd-container.tar from S3 but its signature ${S3_IMAGE_ASC_URL} is missing; refusing to trust an unsigned image"
    mv "${IMAGE_TAR}.dl" "${IMAGE_TAR}"
    mv "${IMAGE_TAR}.asc.dl" "${IMAGE_TAR}.asc"
    echo "gitd: image: downloaded from S3 fallback + .asc"
else
    echo "gitd: image: GitHub/S3 fetch failed; using bundle copy"
    [[ -s "${IMAGE_TAR}.asc" ]] || die "bundle copy of gitd-container.tar has no .asc signature; refusing to trust an unsigned image"
fi
rm -f "${IMAGE_TAR}.dl" "${IMAGE_TAR}.asc.dl"
[[ -s "${IMAGE_TAR}" ]] || die "gitd-container.tar is empty after download"
# Order: GPG provenance first, then the pinned sha256, then import.
verify_artifact "${IMAGE_TAR}" "${GPG_KEY_PIN}"
verify_sha256 "${IMAGE_SHA256}" "${IMAGE_TAR}"

# Wait for containerd readiness before importing (R3-Q2): `systemctl enable --now`
# returns once systemd has accepted the unit, but containerd may not have created
# /run/containerd/containerd.sock yet — ctr would fail with "no such file or
# directory". Poll up to 60s for the socket + `ctr version`; a failed unit dies
# immediately (fail-fast, fail-loud) instead of burning the full timeout.
CONTAINERD_SOCK="/run/containerd/containerd.sock"
containerd_timeout=60
containerd_ready=0
for _ in $(seq 1 "${containerd_timeout}"); do
    if [[ "$(systemctl is-active containerd 2>/dev/null || true)" == "failed" ]]; then
        echo "gitd: userdata: ----- containerd journal (last 40) -----"
        journalctl -u containerd --no-pager -n 40 2>/dev/null || echo "(no journal)"
        die "containerd.service failed to start; status: $(systemctl is-active containerd 2>/dev/null || true)"
    fi
    if [[ -S "${CONTAINERD_SOCK}" ]] && ctr --address "${CONTAINERD_SOCK}" version >/dev/null 2>&1; then
        containerd_ready=1
        break
    fi
    sleep 1
done
if [[ "${containerd_ready}" -ne 1 ]]; then
    echo "gitd: userdata: ----- containerd journal (last 40) -----"
    journalctl -u containerd --no-pager -n 40 2>/dev/null || echo "(no journal)"
    echo "gitd: userdata: ----- containerd status -----"
    systemctl status containerd --no-pager 2>/dev/null || true
    echo "gitd: userdata: ----- /run/containerd -----"
    ls -la /run/containerd 2>/dev/null || echo "(dir missing)"
    echo "gitd: userdata: ----- containerd procs -----"
    ps aux | grep '[c]ontainerd' || echo "(no containerd proc)"
    die "containerd socket ${CONTAINERD_SOCK} did not become ready within ${containerd_timeout}s; status: $(systemctl is-active containerd 2>/dev/null || true)"
fi
echo "gitd: userdata: containerd ready (${CONTAINERD_SOCK})"

# The OCI archive carries only the tag "latest" (baked into index.json as
# org.opencontainers.image.ref.name by tools/dist/package-image.sh); the repo
# comes from --base-name. --ref is not a ctr images import flag.
ctr images import --base-name git.cmposer.cc/gitd "${IMAGE_TAR}" || die "ctr images import failed"
ctr images ls | grep -q "git.cmposer.cc/gitd:latest" || die "image import did not register git.cmposer.cc/gitd:latest"

echo "gitd: userdata: writing systemd units"
# --- the three ctr systemd units + timer (R2-Q15, R5-Q4, R8-Q4, R12-Q5) ------------
# image ref, per-unit cap sets, bind mounts, memory caps. All units:
#   --rootfs-ro (R5-Q4), --host-resolv-conf + --host-hosts-file (DNS in a
#   from-scratch image, R5-Q4), --net-host (share host netns, R6-Q7), --rm.
cat > /etc/systemd/system/gitd-serve.service <<'SERVE_EOF'
[Unit]
Description=gitd browse (:443 mTLS) + socket server (uid 1001)
After=containerd.service
Requires=containerd.service

[Service]
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --uid 1001 --gid 1001 \
  --rootfs-ro --host-resolv-conf --host-hosts-file \
  --cap-drop ALL --cap-add NET_BIND_SERVICE \
  --memory 128MiB \
  --mount type=bind,src=/srv/git,dst=/srv/git,ro \
  --mount type=bind,src=/var/spool/gitd,dst=/var/spool/gitd,rw \
  --mount type=bind,src=/etc/gitd,dst=/etc/gitd,ro \
  git.cmposer.cc/gitd:latest /usr/local/bin/gitd serve --config /etc/gitd/gitd.yaml
Restart=always
RestartSec=5
SERVE_EOF

cat > /etc/systemd/system/gitd-sshd.service <<'SSHD_SERVICE_EOF'
[Unit]
Description=gitd OpenSSH server (container, PQC kex)
After=gitd-serve.service containerd.service
Requires=containerd.service

[Service]
# /etc/gitd is overlaid onto BOTH /etc/gitd (gitd config for the ForceCommand
# gateway) and /etc/ssh (sshd_config + host key + trusted CA + auth_principals +
# revoked_keys), matching the image's baked paths.
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --rootfs-ro --host-resolv-conf --host-hosts-file \
  --cap-drop ALL --cap-add CHOWN --cap-add SETGID --cap-add SETUID --cap-add SYS_CHROOT \
  --memory 320MiB \
  --mount type=bind,src=/srv/git,dst=/srv/git,rw \
  --mount type=bind,src=/var/spool/gitd,dst=/var/spool/gitd,rw \
  --mount type=bind,src=/etc/gitd,dst=/etc/gitd,ro \
  --mount type=bind,src=/etc/gitd,dst=/etc/ssh,ro \
  --mount type=bind,src=/home/admin,dst=/home/admin,rw \
  git.cmposer.cc/gitd:latest /usr/local/bin/sshd -D -f /etc/ssh/sshd_config -e
Restart=always
RestartSec=5
SSHD_SERVICE_EOF

cat > /etc/systemd/system/gitd-ddns.service <<'DDNS_SERVICE_EOF'
[Unit]
Description=gitd Namecheap dynamic DNS refresh (one-shot)
After=containerd.service network-online.target
Requires=containerd.service

[Service]
Type=oneshot
# Runs as root so it can read ddns-password (root:root 0600, R6-Q5/R7-Q3).
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --rootfs-ro --host-resolv-conf --host-hosts-file \
  --cap-drop ALL \
  --memory 64MiB \
  --mount type=bind,src=/etc/gitd,dst=/etc/gitd,ro \
  git.cmposer.cc/gitd:latest /usr/local/bin/gitd ddns --config /etc/gitd/gitd.yaml
DDNS_SERVICE_EOF

cat > /etc/systemd/system/gitd-ddns.timer <<'DDNS_TIMER_EOF'
[Unit]
Description=Refresh gitd DDNS record every 6h (gitd.yaml ddns.interval)

[Timer]
OnCalendar=*-*-* 0/6:00:00
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
DDNS_TIMER_EOF

systemctl daemon-reload
systemctl enable --now gitd-serve.service
systemctl enable --now gitd-sshd.service
systemctl enable --now gitd-ddns.timer

# --- host gitd-cert-sync hourly timer (R12-Q6) --------------------------------------
# pki/gitd-cert-sync.sh pulls /gitd/server/* from SSM to /etc/gitd/tls/ root:root;
# per-handshake reads make rotation zero-downtime.
[[ -f "${BUNDLE_DIR}/gitd-cert-sync.sh" ]] || die "gitd-cert-sync.sh missing from bundle"
install -m 0755 -o root -g root "${BUNDLE_DIR}/gitd-cert-sync.sh" /usr/local/sbin/gitd-cert-sync.sh
cat > /etc/systemd/system/gitd-cert-sync.service <<'CERT_SYNC_SERVICE_EOF'
[Unit]
Description=Refresh gitd browse TLS material from SSM
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/gitd-cert-sync.sh
CERT_SYNC_SERVICE_EOF
cat > /etc/systemd/system/gitd-cert-sync.timer <<'CERT_SYNC_TIMER_EOF'
[Unit]
Description=Hourly gitd browse TLS material sync

[Timer]
OnCalendar=*-*-* *:15:00
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
CERT_SYNC_TIMER_EOF
systemctl daemon-reload
systemctl enable --now gitd-cert-sync.timer

# --- host OS patching: dnf-automatic security-only (R4-Q10) ------------------------
dnf install -y -q dnf-automatic
cat > /etc/dnf/automatic.conf <<'AUTOMATIC_EOF'
[commands]
upgrade_type = security
apply_updates = yes
random_sleep = 360
AUTOMATIC_EOF
systemctl enable --now dnf-automatic-install.timer

# --- automatic reboot when security updates require it (R5-Q11) ---------------------
dnf install -y -q dnf-utils
cat > /usr/local/sbin/gitd-maybe-reboot.sh <<'REBOOT_EOF'
#!/usr/bin/env bash
# Reboot the host when applied security updates require it (R5-Q11).
# needs-restarting -r exits 0 when no reboot is required, non-zero when it is.
set -euo pipefail
if needs-restarting -r 2>/dev/null; then
    echo "gitd: no reboot required"
else
    echo "gitd: reboot required by applied updates; scheduling"
    shutdown -r +1 "gitd: reboot for applied security updates"
fi
REBOOT_EOF
chmod 0755 /usr/local/sbin/gitd-maybe-reboot.sh
cat > /etc/systemd/system/gitd-reboot.service <<'REBOOT_SERVICE_EOF'
[Unit]
Description=Reboot when applied security updates require it
After=dnf-automatic-install.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/gitd-maybe-reboot.sh
REBOOT_SERVICE_EOF
cat > /etc/systemd/system/gitd-reboot.timer <<'REBOOT_TIMER_EOF'
[Unit]
Description=Daily reboot check (after the dnf-automatic apply window)

[Timer]
OnCalendar=*-*-* 07:00:00
Persistent=true

[Install]
WantedBy=timers.target
REBOOT_TIMER_EOF
systemctl daemon-reload
systemctl enable --now gitd-reboot.timer

echo "gitd: userdata: waiting for units + running liveness probe"
# --- wait for units + liveness probe (R9-Q9) ----------------------------------------
# Liveness is the ssh greeting + an mTLS curl against :443 (no cert-less healthz).
for _ in $(seq 1 30); do
    systemctl is-active --quiet gitd-serve.service && break
    sleep 2
done
systemctl is-active --quiet gitd-serve.service || die "gitd-serve did not start"
systemctl is-active --quiet gitd-sshd.service || die "gitd-sshd did not start"

# mTLS probe against the browse UI using the probe client cert from SSM. Resolve
# the hostname to loopback so the server cert (CN=git.cmposer.cc) verifies and
# the Host header is on the allowlist.
curl --fail --silent --show-error \
    --cacert /etc/gitd/tls/client-ca.crt \
    --cert /etc/gitd/tls/probe.crt \
    --key /etc/gitd/tls/probe.key \
    --resolve "git.cmposer.cc:443:127.0.0.1" \
    --max-time 20 \
    "https://git.cmposer.cc/" >/dev/null \
    || die "browse mTLS liveness probe failed"

echo "gitd: userdata: boot complete; git.cmposer.cc is up"

# --- signal boot success (R7-Q6 creation policy) -------------------------------------
# The stack's CreationPolicy waits for exactly one signal (20-min timeout). We
# signal here after all units are up + the liveness probe; the bootstrap also
# signals the exit code as a fail-fast fallback if we never reach this line.
/opt/aws/bin/cfn-signal -e 0 --stack "${STACK_NAME}" \
    --resource "${RESOURCE_NAME}" --region "${REGION}"
