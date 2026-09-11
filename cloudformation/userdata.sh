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
# admin is a git group member (gid 1001, guaranteed above): the control socket
# is 0770 git:git and /var/spool/gitd is setgid git, so admin can connect to
# the serve socket and list/read the data-plane material. gitd mirror restore
# routes through that socket (serve owns /srv/git); no sudo exists in the
# image anymore (the scoped sudoers grant was removed).
usermod -aG git admin

mkdir -p /srv/git /var/spool/gitd /var/spool/gitd/restore /etc/gitd/tls /etc/gitd/auth_principals
# admin runs gitd data-plane verbs directly (no sudo; containerd sets
# NoNewPrivileges), so /var/spool/gitd is setgid git (2770): new spool files
# and the control socket inherit group git, and admin (a git group member)
# can list/read events and connect to the socket.
chown git:git /srv/git /var/spool/gitd /var/spool/gitd/restore
chmod 0755 /srv/git
chmod 2770 /var/spool/gitd
# The restore job spool is writable by both serve (root stages jobs) and the
# gitd-restore agent (git consumes them + writes results): setgid git 2770,
# so files created in it inherit group git and stay cross-readable.
chmod 2770 /var/spool/gitd/restore
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
PidFile /run/sshd/sshd.pid
LogLevel VERBOSE
HostKey /etc/ssh/ssh_host_ed25519_key
HostCertificate /etc/ssh/ssh_host_ed25519_key-cert.pub
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

# Fail-fast (code-philosophy): a host sshd_config that cannot parse can never open
# :22, so abort boot immediately. The authoritative `sshd -t` runs inside the sshd
# container; the host may not carry an sshd binary, so a missing one is tolerated
# (the container-side parse is surfaced by dump_sshd_diagnostics).
if command -v sshd >/dev/null 2>&1; then
    sshd -t -f /etc/gitd/sshd_config || die "host sshd rejected /etc/gitd/sshd_config"
else
    echo "gitd: userdata: host has no sshd binary; skipping host-side config parse (container-side -t is authoritative)"
fi

# Per-user CA principals (R2-Q14): admin cert carries principals git,admin;
# git cert carries principal git. AuthorizedPrincipalsFile is line-per-principal:
# each line is exactly one principal — whitespace and commas are NOT delimiters,
# so a space or comma on a line yields one literal principal matching neither.
# One principal per line below.
printf 'admin\ngit\n' > /etc/gitd/auth_principals/admin
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
# admin (git group) reads webhooks.yaml for `gitd spool replay`.
chown git:git   /etc/gitd/webhooks.yaml && chmod 0640 /etc/gitd/webhooks.yaml
# gitd-serve now runs as root (commit 8ce38ec): containerd/runc does not put
# CAP_NET_BIND_SERVICE into a non-root process's EFFECTIVE set, so root is
# required to bind privileged :443; CAP_DAC_OVERRIDE lets root read/write the
# gitd material (/var/spool/gitd is git:git 2770). The root:git 0640 grants
# below are now redundant but harmless (they pin group read on the browse TLS
# files). probe/ssh/ddns material stays root-only (serve never reads it).
chown root:git /etc/gitd/tls && chmod 0750 /etc/gitd/tls
chown root:root /etc/gitd/tls/probe.key \
                /etc/gitd/ssh_host_ed25519_key /etc/gitd/ddns-password
chmod 0600 /etc/gitd/tls/probe.key \
           /etc/gitd/ssh_host_ed25519_key /etc/gitd/ddns-password
chown root:git /etc/gitd/tls/server.key /etc/gitd/tls/server.crt \
               /etc/gitd/tls/client-ca.crt /etc/gitd/tls/revoked.crl
chmod 0640 /etc/gitd/tls/server.key /etc/gitd/tls/server.crt \
           /etc/gitd/tls/client-ca.crt /etc/gitd/tls/revoked.crl
chown root:root /etc/gitd/tls/probe.crt \
                /etc/gitd/ssh_host_ed25519_key.pub \
                /etc/gitd/ssh_host_ed25519_key-cert.pub \
                /etc/gitd/trusted_user_ca_keys.pem \
                /etc/gitd/sshd_config
chmod 0644 /etc/gitd/tls/probe.crt \
           /etc/gitd/ssh_host_ed25519_key.pub \
           /etc/gitd/ssh_host_ed25519_key-cert.pub \
           /etc/gitd/trusted_user_ca_keys.pem \
           /etc/gitd/sshd_config
chown -R root:root /etc/gitd/auth_principals
# auth_principals is a DIRECTORY: 0644 (from -R) would strip the execute bit, so
# the git user could not traverse it to read its principals file. sshd reads
# AuthorizedPrincipalsFile as the authenticating user, so the dir must be
# traversable (0755) while the principal files stay world-readable (0644).
chmod -R u=rwX,go=rX /etc/gitd/auth_principals
chown root:root /etc/gitd/revoked_keys && chmod 0644 /etc/gitd/revoked_keys

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
#   --read-only (R5-Q4), ro bind mounts of /etc/resolv.conf + /etc/hosts (DNS in
#   a from-scratch image, R5-Q4), --net-host (share host netns, R6-Q7), --rm.
# Flag syntax is containerd 2.x: --user UID:GID, --memory-limit <bytes>,
# --mount type=bind,source=...,destination=...,options=rbind:ro.
# Positionals are ctr run [flags] <image> <container-id> <command>...; each unit
# passes a per-unit alphanumeric container-id (gitd-serve/gitd-sshd/gitd-ddns)
# between the image ref and the command — without it the command is consumed as
# the container-id and ctr rejects it with "container.ID ... must match".
#
# Capability flags (containerd 2.3.5, verified against the pinned ctr):
#   * EVERY --cap-drop/--cap-add token must start with "CAP_" (run_unix.go
#     267-283); there is NO "ALL" token — "--cap-drop ALL" is rejected with
#     "capabilities must be specified with 'CAP_' prefix", and "CAP_ALL" is a
#     silent no-op (removeCap exact-matches the 14 default unix caps in
#     pkg/oci/spec.go defaultUnixCaps(), and "CAP_ALL" is not one of them).
#   * --cap-add is applied BEFORE --cap-drop (run_unix.go 267 then 276), so any
#     cap listed in BOTH is dropped after being added. Each drop list below is
#     therefore the default-unix-caps COMPLEMENT of that unit's adds, so the
#     container's final cap set is exactly the adds (least privilege).
#   * Only repeated --cap-drop flags work: a comma-separated list is treated as
#     one token and silently drops nothing.
cat > /etc/systemd/system/gitd-serve.service <<'SERVE_EOF'
[Unit]
# Runs as root (like gitd-sshd/gitd-ddns): as non-root with only
# CAP_NET_BIND_SERVICE, containerd/runc does not put the cap into the process's
# EFFECTIVE set, so binding privileged :443 is denied (containerd 2.3.5).
# CAP_DAC_OVERRIDE is added (and NOT dropped) so root serve can read/write the
# gitd material (/var/spool/gitd is git:git 0700; /etc/gitd root:git 0640).
Description=gitd browse (:443 mTLS) + socket server (runs as root)
After=containerd.service
Requires=containerd.service

[Service]
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --read-only \
  --cap-drop CAP_CHOWN --cap-drop CAP_FSETID --cap-drop CAP_FOWNER \
  --cap-drop CAP_MKNOD --cap-drop CAP_NET_RAW \
  --cap-drop CAP_SETGID --cap-drop CAP_SETUID --cap-drop CAP_SETFCAP \
  --cap-drop CAP_SETPCAP --cap-drop CAP_SYS_CHROOT --cap-drop CAP_KILL \
  --cap-drop CAP_AUDIT_WRITE --cap-add CAP_NET_BIND_SERVICE \
  --cap-add CAP_DAC_OVERRIDE \
  --memory-limit 134217728 \
  --mount type=bind,source=/srv/git,destination=/srv/git,options=rbind:ro \
  --mount type=bind,source=/var/spool/gitd,destination=/var/spool/gitd,options=rbind:rw \
  --mount type=bind,source=/etc/gitd,destination=/etc/gitd,options=rbind:ro \
  --mount type=bind,source=/etc/resolv.conf,destination=/etc/resolv.conf,options=rbind:ro \
  --mount type=bind,source=/etc/hosts,destination=/etc/hosts,options=rbind:ro \
  git.cmposer.cc/gitd:latest gitd-serve /usr/local/bin/gitd serve --config /etc/gitd/gitd.yaml
Restart=always
RestartSec=5
SERVE_EOF

cat > /etc/systemd/system/gitd-sshd.service <<'SSHD_SERVICE_EOF'
[Unit]
# Runs as root (like gitd-serve/gitd-ddns). CAP_NET_BIND_SERVICE is added
# (and NOT dropped) so the container sshd can bind privileged :22.
Description=gitd OpenSSH server (container, PQC kex)
After=gitd-serve.service containerd.service
Requires=containerd.service

[Service]
# /etc/gitd is overlaid onto BOTH /etc/gitd (gitd config for the ForceCommand
# gateway) and /etc/ssh (sshd_config + host key + trusted CA + auth_principals +
# revoked_keys), matching the image's baked paths.
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --read-only \
  --cap-drop CAP_DAC_OVERRIDE --cap-drop CAP_FSETID --cap-drop CAP_FOWNER \
  --cap-drop CAP_MKNOD --cap-drop CAP_NET_RAW --cap-drop CAP_SETFCAP \
  --cap-drop CAP_SETPCAP --cap-drop CAP_KILL --cap-drop CAP_AUDIT_WRITE \
  --cap-add CAP_CHOWN --cap-add CAP_SETGID --cap-add CAP_SETUID \
  --cap-add CAP_SYS_CHROOT --cap-add CAP_NET_BIND_SERVICE \
  --memory-limit 335544320 \
  --mount type=bind,source=/srv/git,destination=/srv/git,options=rbind:rw \
  --mount type=bind,source=/var/spool/gitd,destination=/var/spool/gitd,options=rbind:rw \
  --mount type=bind,source=/etc/gitd,destination=/etc/gitd,options=rbind:ro \
  --mount type=bind,source=/etc/gitd,destination=/etc/ssh,options=rbind:ro \
  --mount type=bind,source=/home/admin,destination=/home/admin,options=rbind:rw \
  --mount type=bind,source=/etc/resolv.conf,destination=/etc/resolv.conf,options=rbind:ro \
  --mount type=bind,source=/etc/hosts,destination=/etc/hosts,options=rbind:ro \
  --mount type=tmpfs,destination=/run/sshd,options=mode=1777 \
  git.cmposer.cc/gitd:latest gitd-sshd /usr/local/sbin/sshd -D -f /etc/ssh/sshd_config -e
Restart=always
RestartSec=5
SSHD_SERVICE_EOF

cat > /etc/systemd/system/gitd-restore.service <<'RESTORE_SERVICE_EOF'
[Unit]
# Runs as the git user (uid/gid 1001, the repo-store owner) — NO elevated
# caps: containerd/runc does not put caps into a non-root process's EFFECTIVE
# set, and the agent only reads its ro config mounts and writes the
# git-owned /srv/git + /var/spool/gitd rw mounts. gitd-serve stages restore
# jobs in /var/spool/gitd/restore; this agent scan-then-watches that dir,
# re-verifies the staged bundle, and writes /srv/git/<repo>.git via the
# mirror (serve keeps /srv/git rbind:ro).
Description=gitd restore agent (git-context, no elevated caps)
After=gitd-serve.service containerd.service
Requires=containerd.service

[Service]
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --read-only \
  --user 1001:1001 \
  --cap-drop CAP_CHOWN --cap-drop CAP_DAC_OVERRIDE --cap-drop CAP_FSETID \
  --cap-drop CAP_FOWNER --cap-drop CAP_MKNOD --cap-drop CAP_NET_RAW \
  --cap-drop CAP_SETGID --cap-drop CAP_SETUID --cap-drop CAP_SETFCAP \
  --cap-drop CAP_SETPCAP --cap-drop CAP_SYS_CHROOT --cap-drop CAP_KILL \
  --cap-drop CAP_AUDIT_WRITE --cap-drop CAP_NET_BIND_SERVICE \
  --memory-limit 134217728 \
  --mount type=bind,source=/srv/git,destination=/srv/git,options=rbind:rw \
  --mount type=bind,source=/var/spool/gitd,destination=/var/spool/gitd,options=rbind:rw \
  --mount type=bind,source=/etc/gitd,destination=/etc/gitd,options=rbind:ro \
  --mount type=bind,source=/etc/resolv.conf,destination=/etc/resolv.conf,options=rbind:ro \
  --mount type=bind,source=/etc/hosts,destination=/etc/hosts,options=rbind:ro \
  git.cmposer.cc/gitd:latest gitd-restore /usr/local/bin/gitd mirror-agent --config /etc/gitd/gitd.yaml
Restart=always
RestartSec=5
RESTORE_SERVICE_EOF

cat > /etc/systemd/system/gitd-ddns.service <<'DDNS_SERVICE_EOF'
[Unit]
Description=gitd Namecheap dynamic DNS refresh (one-shot)
After=containerd.service network-online.target
Requires=containerd.service

[Service]
Type=oneshot
# Runs as root so it can read ddns-password (root:root 0600, R6-Q5/R7-Q3).
ExecStart=/usr/local/bin/ctr run --rm --net-host \
  --read-only \
  --cap-drop CAP_CHOWN --cap-drop CAP_DAC_OVERRIDE --cap-drop CAP_FSETID \
  --cap-drop CAP_FOWNER --cap-drop CAP_MKNOD --cap-drop CAP_NET_RAW \
  --cap-drop CAP_SETGID --cap-drop CAP_SETUID --cap-drop CAP_SETFCAP \
  --cap-drop CAP_SETPCAP --cap-drop CAP_NET_BIND_SERVICE \
  --cap-drop CAP_SYS_CHROOT --cap-drop CAP_KILL --cap-drop CAP_AUDIT_WRITE \
  --memory-limit 67108864 \
  --mount type=bind,source=/etc/gitd,destination=/etc/gitd,options=rbind:ro \
  --mount type=bind,source=/etc/resolv.conf,destination=/etc/resolv.conf,options=rbind:ro \
  --mount type=bind,source=/etc/hosts,destination=/etc/hosts,options=rbind:ro \
  git.cmposer.cc/gitd:latest gitd-ddns /usr/local/bin/gitd ddns --config /etc/gitd/gitd.yaml
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
# Only the containerized gitd sshd may serve :22 (R10-Q1). The stock AL2023
# sshd would otherwise hold the port (and use host keys/config, bypassing our
# CA auth + hardened config). Disable + mask so it can never start.
systemctl disable --now sshd.socket sshd.service 2>/dev/null || true
systemctl mask sshd.socket sshd.service 2>/dev/null || true
systemctl enable --now gitd-sshd.service
# The restore agent is a background role: it watches /var/spool/gitd/restore
# and performs git-context restores staged by gitd-serve. Ordered after serve
# so the spool exists before the agent starts watching.
systemctl enable --now gitd-restore.service
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

# --- debug-only: stream gitd-sshd verbose journal to console (post-boot) -------------
# After an SSH auth failure the operator must be able to read the exact rejection
# reason from `aws ec2 get-console-output` / serial console WITHOUT any host-plane
# access (no SSH/SSM). gitd-sshd runs `sshd -D -e`, so its VERBOSE auth decisions
# land in `journalctl -u gitd-sshd.service` on the host; this helper tees those
# lines to /dev/console (fallback /dev/kmsg) for 20 minutes after boot. It is
# purely diagnostic and self-bounds (timeout 1200) so it never runs forever; a
# missing binary or unwritable console device is a silent no-op, never a boot break.
cat > /etc/systemd/system/gitd-sshd-journal.service <<'SSHD_JOURNAL_SERVICE_EOF'
[Unit]
Description=Stream gitd-sshd verbose auth journal to console (debug helper, 20 min window)

[Service]
Type=oneshot
# Debug-only: bounded follow of the gitd-sshd journal; tees each new VERBOSE line
# to the console (fallback /dev/kmsg) so post-boot SSH auth failures are readable
# via get-console-output without any host access. stdout stays in this unit's
# journal too. Guarded: a missing binary or unwritable device is a silent no-op,
# never a boot break.
ExecStart=/bin/sh -c '\
  command -v timeout >/dev/null 2>&1 || exit 0; \
  command -v journalctl >/dev/null 2>&1 || exit 0; \
  command -v tee >/dev/null 2>&1 || exit 0; \
  OUT=/dev/console; [ -w "$OUT" ] || OUT=/dev/kmsg; [ -w "$OUT" ] || OUT=; \
  if [ -n "$OUT" ]; then \
    echo "gitd: userdata: ----- sshd journal -> console (20 min window) -----" | tee "$OUT"; \
  else \
    echo "gitd: userdata: ----- sshd journal -> console (20 min window) -----"; \
  fi; \
  if [ -n "$OUT" ]; then \
    timeout 1200 journalctl -u gitd-sshd.service -f -n 20 --no-pager 2>/dev/null | tee "$OUT"; \
  else \
    timeout 1200 journalctl -u gitd-sshd.service -f -n 20 --no-pager 2>/dev/null; \
  fi; \
  exit 0'
SSHD_JOURNAL_SERVICE_EOF
cat > /etc/systemd/system/gitd-sshd-journal.timer <<'SSHD_JOURNAL_TIMER_EOF'
[Unit]
Description=Start gitd-sshd journal -> console stream once, 90s after boot

[Timer]
OnBootSec=90s

[Install]
WantedBy=timers.target
SSHD_JOURNAL_TIMER_EOF
systemctl daemon-reload
systemctl enable --now gitd-sshd-journal.timer

echo "gitd: userdata: waiting for units + running liveness probe"
# --- wait for units + liveness probe (R9-Q9) ----------------------------------------
# Liveness is the ssh greeting + an mTLS curl against :443 (no cert-less healthz).
# Fail-loud (code-philosophy): a container unit that is not active dumps its
# journal/status and the containerd task/container/image tables BEFORE boot dies,
# so a failed boot carries the evidence needed to diagnose a ctr run failure.
dump_unit_diagnostics() {
    local unit="$1"
    echo "gitd: userdata: ----- ${unit} journal (last 40) -----"
    journalctl -u "${unit}" --no-pager -n 40 2>/dev/null || echo "(no journal)"
    echo "gitd: userdata: ----- ${unit} status -----"
    systemctl status "${unit}" --no-pager 2>/dev/null || true
    echo "gitd: userdata: ----- containerd tasks -----"
    ctr -n default tasks ls 2>/dev/null || echo "(ctr tasks ls failed)"
    echo "gitd: userdata: ----- containerd containers -----"
    ctr -n default containers ls 2>/dev/null || echo "(ctr containers ls failed)"
    echo "gitd: userdata: ----- containerd images -----"
    ctr -n default images ls 2>/dev/null || echo "(ctr images ls failed)"
    echo "gitd: userdata: ----- gitd procs -----"
    ps aux | grep '[g]itd' || echo "(no gitd process)"
}
fail_unit_diagnostics() {
    local unit="$1"
    dump_unit_diagnostics "${unit}"
    die "${unit} did not start"
}
# sshd-specific boot diagnostics (code-philosophy: fail-loud). systemd seeing the
# `ctr run` wrapper alive does NOT prove the in-container sshd parsed its config,
# bound, and stayed LISTENING on :22. These probes surface sshd's real state so a
# boot that loses sshd carries the evidence; every probe is guarded (|| echo/|| true)
# so one failure never masks another. Also run right after the liveness probe as a
# positive confirmation that sshd was still listening at boot-complete time.
dump_sshd_diagnostics() {
    echo "gitd: userdata: ----- gitd-sshd.service journal (last 40) -----"
    journalctl -u gitd-sshd.service --no-pager -n 40 2>/dev/null || echo "(no gitd-sshd journal)"
    echo "gitd: userdata: ----- gitd-sshd.service status -----"
    systemctl status gitd-sshd.service --no-pager 2>/dev/null || true
    echo "gitd: userdata: ----- :22/:443 listener on host -----"
    if command -v ss >/dev/null 2>&1; then
        ss -tlnp 2>/dev/null | grep -E ':(22|443)\b' || echo "(no :22/:443 listener)"
    elif command -v netstat >/dev/null 2>&1; then
        netstat -tlnp 2>/dev/null | grep -E ':(22|443)\b' || echo "(no :22/:443 listener)"
    else
        echo "(neither ss nor netstat available; cannot check :22/:443)"
    fi
    echo "gitd: userdata: ----- containerd containers -----"
    ctr -n default containers ls 2>/dev/null || echo "(ctr containers ls failed)"
    echo "gitd: userdata: ----- containerd tasks -----"
    ctr -n default tasks ls 2>/dev/null || echo "(ctr tasks ls failed)"
    echo "gitd: userdata: ----- containerd task ps gitd-sshd -----"
    ctr -n default tasks ps gitd-sshd 2>/dev/null || echo "(ctr tasks ps gitd-sshd failed)"
    echo "gitd: userdata: ----- sshd/ctr gitd procs -----"
    ps aux | grep -E '[s]shd|[g]itd-sshd|[c]tr run' || echo "(no sshd/ctr gitd process)"
    echo "gitd: userdata: ----- sshd_config on host -----"
    ls -l /etc/gitd/sshd_config 2>/dev/null || echo "(no /etc/gitd/sshd_config)"
    head -n 30 /etc/gitd/sshd_config 2>/dev/null || echo "(cannot read /etc/gitd/sshd_config)"
    echo "gitd: userdata: ----- in-container sshd auth files (overlay view) -----"
    # Best-effort (informational): dump the sshd auth files through the same
    # /etc/gitd -> /etc/ssh overlay the container sshd trusts (RevokedKeys +
    # AuthorizedPrincipalsFile). The image is from-scratch — no /bin/sh, no
    # cat — so the one-shot uses the image's fish shell (builtin read/echo)
    # on the exact paths sshd consults; each read carries 2>&1 and an
    # unreadable/missing file falls back to a marker, so every file still
    # surfaces in the boot log. The /tmp tmpfs lets runc start the --read-only
    # one-shot (see the CA-fingerprint probe above); --rm so the probe never
    # lingers.
    ctr -n default run --rm --read-only \
        --mount type=bind,source=/etc/gitd,destination=/etc/ssh,options=rbind:ro \
        --mount type=tmpfs,destination=/tmp,options=mode=1777 \
        git.cmposer.cc/gitd:latest gitd-sshd-auth-fp \
        /usr/local/bin/fish -c 'set -l files /etc/ssh/revoked_keys /etc/ssh/auth_principals/git /etc/ssh/auth_principals/admin; for f in $files; echo "--- $f ---"; if test -r $f; while read -l line; echo $line; end < $f 2>&1; else; echo "($f unreadable or missing)"; end; end' 2>&1 \
        || echo "(in-container sshd auth-files dump not available)"
    echo "gitd: userdata: ----- in-container sshd config parse -----"
    # Best-effort (informational): exec a config test inside the running sshd
    # container. Requires the container task to be up; a failure is not fatal —
    # the listener + journal probes above are authoritative.
    ctr -n default tasks exec --exec-id sshd-t gitd-sshd /usr/local/sbin/sshd -t -f /etc/ssh/sshd_config 2>&1 \
        || echo "(in-container sshd -t not available)"
    echo "gitd: userdata: ----- in-container trusted CA fingerprint -----"
    # ctr tasks exec into a --read-only container fails: runc must write
    # /tmp/runc-process* before the container's tmpfs mounts are active, so the
    # write hits the read-only rootfs. Instead run a one-shot container from the
    # same image whose spec carries its own /tmp tmpfs (so runc can start it) —
    # the OpenSSH 10.5 ssh-keygen then fingerprints the CA exactly as sshd
    # trusts it (/etc/gitd overlay). --rm so the probe container never lingers.
    ctr -n default run --rm --read-only \
        --mount type=bind,source=/etc/gitd,destination=/etc/gitd,options=rbind:ro \
        --mount type=tmpfs,destination=/tmp,options=mode=1777 \
        git.cmposer.cc/gitd:latest gitd-sshd-ca-fp \
        /usr/local/bin/ssh-keygen -lf /etc/gitd/trusted_user_ca_keys.pem 2>&1 \
        || echo "(in-container ssh-keygen for trusted CA not available)"
}
fail_sshd_diagnostics() {
    dump_sshd_diagnostics
    die "gitd-sshd.service is not active and/or not listening on :22"
}
# DDNS boot diagnostics (code-philosophy: fail-loud). A stale DDNS record leaves
# git.cmposer.cc unreachable, so a failed refresh must carry its evidence; every
# probe is guarded (|| echo/|| true) so one failure never masks another.
dump_ddns_diagnostics() {
    echo "gitd: userdata: ----- gitd-ddns.service journal (last 40) -----"
    journalctl -u gitd-ddns.service --no-pager -n 40 2>/dev/null || echo "(no gitd-ddns journal)"
    echo "gitd: userdata: ----- gitd-ddns.service status -----"
    systemctl status gitd-ddns.service --no-pager 2>/dev/null || true
    echo "gitd: userdata: ----- gitd-ddns.timer status -----"
    systemctl status gitd-ddns.timer --no-pager 2>/dev/null || true
    echo "gitd: userdata: ----- ddns-password file -----"
    ls -l /etc/gitd/ddns-password 2>/dev/null || echo "(no /etc/gitd/ddns-password)"
    echo "gitd: userdata: ----- ddns: section of gitd.yaml (password redacted) -----"
    # Print the ddns: section up to the next top-level key; any inline password
    # value is redacted. The password itself lives in ddns-password and is never
    # echoed (least privilege / evidence-carrying diagnostics).
    awk '
        /^ddns:/ { in_ddns = 1 }
        in_ddns && /^[^[:space:]#]/ && !/^ddns:/ { exit }
        in_ddns { print }
    ' /etc/gitd/gitd.yaml 2>/dev/null | sed 's/password:.*/password: <redacted>/i' \
        || echo "(cannot read ddns section of /etc/gitd/gitd.yaml)"
}
fail_ddns_diagnostics() {
    dump_ddns_diagnostics
    die "gitd-ddns.service did not refresh the DDNS record"
}
# The unit being 'active' can be a crash-loop (Restart=always keeps it active
# between attempts). Require a real :22 listener that is OUR ctr-spawned sshd,
# not the stock host sshd — otherwise the boot 'passes' but git SSH is dead.
# --net-host puts the container socket in the host netns, so ss/netstat here
# sees it.
container_sshd_listening() {
    # Fail-loud guard: without ss or netstat we cannot prove the listener.
    command -v ss >/dev/null 2>&1 || command -v netstat >/dev/null 2>&1 || return 1

    local lines pids pid exe
    if command -v ss >/dev/null 2>&1; then
        lines="$(ss -tlnpH 2>/dev/null)"
        # ss process column: users:(("sshd",pid=N,fd=M))
        pids="$(printf '%s\n' "$lines" | awk '/LISTEN/ && /:22 / { print }' \
            | sed -n 's/.*pid=\([0-9][0-9]*\).*/\1/p')"
    else
        lines="$(netstat -tlnp 2>/dev/null)"
        # netstat last column: PID/name
        pids="$(printf '%s\n' "$lines" | awk '/LISTEN/ && /:22 / { print $NF }' \
            | sed -n 's|/.*||p')"
    fi
    [[ -n "$pids" ]] || return 1

    # The stock host sshd (/usr/sbin/sshd) would also LISTEN on :22; only the
    # container's binary path proves OUR sshd holds the port.
    for pid in $pids; do
        exe="$(readlink "/proc/${pid}/exe" 2>/dev/null || true)"
        case "$exe" in
            /usr/local/sbin/sshd*) return 0 ;;
        esac
    done
    return 1
}
# Poll serve + sshd together: sshd is ordered After=gitd-serve, so a single
# is-active check on sshd right after serve comes up would race sshd's start.
for _ in $(seq 1 30); do
    systemctl is-active --quiet gitd-serve.service \
        && systemctl is-active --quiet gitd-sshd.service && break
    sleep 2
done
systemctl is-active --quiet gitd-serve.service || fail_unit_diagnostics gitd-serve.service
systemctl is-active --quiet gitd-sshd.service || fail_sshd_diagnostics
# Layer the real-listener check on top: 'active' alone can be a crash-loop, so
# a passing boot must mean the container sshd actually holds :22.
container_sshd_listening || fail_sshd_diagnostics

# DDNS one-shot at boot (code-philosophy: fail-loud): refreshes the A record
# against the live EIP and proves the ddns pipeline end-to-end. Boot is the best
# moment to re-pin the record — the 6h timer only catches drift later, and a
# failed refresh means git.cmposer.cc points at the wrong IP (service outage),
# so we fail loud. Type=oneshot blocks until completion, so a non-zero exit
# here is a real refresh failure.
systemctl start gitd-ddns.service || fail_ddns_diagnostics

# mTLS probe against the browse UI using the probe client cert from SSM. Resolve
# the hostname to loopback so the server cert (CN=git.cmposer.cc) verifies and
# the Host header is on the allowlist. If the probe fails with active units, the
# container process is up but not serving :443; dump serve's diagnostics too.
curl --fail --silent --show-error \
    --cacert /etc/gitd/tls/client-ca.crt \
    --cert /etc/gitd/tls/probe.crt \
    --key /etc/gitd/tls/probe.key \
    --resolve "git.cmposer.cc:443:127.0.0.1" \
    --max-time 20 \
    "https://git.cmposer.cc/" >/dev/null \
    || { dump_unit_diagnostics gitd-serve.service; die "browse mTLS liveness probe failed"; }

# Positive confirmation (code-philosophy: fail-loud, not silent): after the mTLS
# probe passes, dump sshd diagnostics so the boot log records that sshd was in
# fact LISTENING on :22 at boot-complete time — directly addressing "did sshd
# remain open after cloud-init".
dump_sshd_diagnostics
echo "gitd: userdata: boot complete; git.cmposer.cc is up"

# --- signal boot success (R7-Q6 creation policy) -------------------------------------
# The stack's CreationPolicy waits for exactly one signal (20-min timeout). We
# signal here after all units are up + the liveness probe; the bootstrap also
# signals the exit code as a fail-fast fallback if we never reach this line.
/opt/aws/bin/cfn-signal -e 0 --stack "${STACK_NAME}" \
    --resource "${RESOURCE_NAME}" --region "${REGION}"
