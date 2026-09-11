#!/usr/bin/env bash
# cloudformation/update.sh — in-place update of a live gitd server (R3-Q10, R6-Q3).
#
# This is NOT deploy.sh. CloudFormation stays for initial infra + emergency
# rebuild only; routine upgrades ship a new image in place. The image is assumed
# to ALREADY be built + GPG-signed + published by `make release` (release.sh):
# update.sh does NOT build and does NOT sign. It VERIFIES the pre-signed image
# against the pinned public key (verify_artifact) and the operator's out-of-band
# sha256 pin (verify_sha256), mirrors it onto the artifact channel if needed,
# then over SSM has the host fetch it, re-verify the GPG signature and the pinned
# sha256, ctr-images import it, and restart the three container units. EBS
# (/srv/git) + spool (/var/spool/gitd) are preserved untouched — an update never
# does a bundle restore (mirrors are a backup, not a deploy input).
#
# The critical security property (R6-Q3): the sha256 pin is provided OUT OF BAND
# by the operator, as an argument or the GITD_IMAGE_SHA256 env var — the value
# printed by `make release` / the release notes. It is NEVER fetched from the
# artifact channel. The script verifies the local tarball against that pin before
# publishing, and the host re-verifies its download against the SAME pin before
# importing. Any mismatch aborts.
#
# Provenance (GPG): the image is signed by `make release` (release.sh →
# sign_artifact), which produces gitd-container.tar.asc. update.sh requires that
# .asc to exist and calls verify_artifact to confirm the signature is good
# against the pinned public key — it does NOT re-sign. The published .asc is the
# existing "${IMAGE_TAR}.asc" carried through from `make release`.
#
# Transport: host-plane ops (ctr import, systemctl) go over SSM Session Manager
# (AWS-StartNonInteractiveCommand), which is the only elevation the admin split
# allows for the host plane (R2-Q16); the container SSH shell has no
# ctr/systemctl.
#
# Verification: the local `aws ssm start-session` exit code is ALWAYS 0
# regardless of the remote command's outcome (AWS "by design"), so it is never
# trusted. The session output is captured (tee -> a temp file, the operator
# still sees it live) and the document is invoked with separateOutputStream=true
# so it emits an EXIT_CODE: N line. Success is reported ONLY after the in-band
# success sentinel "GITD_UPDATE_OK" (printed by the remote body after every
# `set -euo pipefail` step — GPG verify, sha256 verify, import, restarts —
# succeeds) and/or "EXIT_CODE: 0" appears in the captured output; a failed roll
# is reported loudly, never as success. start-session requires the local
# session-manager-plugin and a TTY; in a non-TTY context the session is wrapped
# in `unbuffer` (expect) to avoid "Cannot perform start session: EOF" (unbuffer
# is an optional mitigation). As before, this does NOT build or sign.
#
# Usage:
#   cloudformation/update.sh --sha256 <hex> [opts]
#   GITD_IMAGE_SHA256=<hex> cloudformation/update.sh [opts]
#
# Options:
#   --sha256 <hex>          pinned sha256 of the NEW gitd-container.tar (R6-Q3;
#                           required, or GITD_IMAGE_SHA256)
#   --image-tar <path>      prebuilt, signed gitd-container.tar (default:
#                           ${REPO_ROOT}/tools/dist/out/gitd-container.tar, as
#                           produced by `make release`; must exist — no build)
#   --bucket <bucket>       default git.cmposer.cc
#   --region <region>       default us-east-2
#   --stack-name <name>     default gitd (used to resolve the instance)
#   --instance-id <id>      override instance resolution from the stack
#   --gitd-release-tag <t>  gitd-container family release tag on ChronicCmposer/gitd (default gitd-container)
#   --dist-repo <owner/repo> gitd repo (default ChronicCmposer/gitd)
#
# Environment: GITD_IMAGE_SHA256, authenticated gh CLI (to publish), aws credentials.
# Idempotent: a re-run with the same pin re-verifies + re-imports the same image
# (ctr import overwrites the tag) and restarts the units — safe to repeat.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Shared GPG signing/verification helpers + the committed pinned public key the
# host uses to verify the fetched image's provenance.
# shellcheck source=tools/release/sign-artifact.sh
source "${REPO_ROOT}/tools/release/sign-artifact.sh"
# Shared gh CLI auth guard (gh_auth / require_gh_auth) — gh reads its own
# stored credentials (GH_TOKEN, if set, is used by gh as an override).
# shellcheck source=tools/release/gh-auth.sh
source "${REPO_ROOT}/tools/release/gh-auth.sh"
SIGNING_KEY="${REPO_ROOT}/tools/release/gitd-signing-key.asc"

# --- defaults ------------------------------------------------------------------
SHA256=""
IMAGE_TAR="${REPO_ROOT}/tools/dist/out/gitd-container.tar"
BUCKET="git.cmposer.cc"
REGION="us-east-2"
STACK_NAME="gitd"
INSTANCE_ID=""
GITD_RELEASE_TAG="gitd-container"
DIST_REPO="ChronicCmposer/gitd"
IMAGE_BASE="git.cmposer.cc/gitd"
IMAGE_REF="${IMAGE_BASE}:latest"

die() {
    echo "gitd: update: $*" >&2
    exit 1
}
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}
require_value() {
    [[ -n "$2" ]] || die "$1 is required"
}
# verify_sha256 <expected-hex> <file> — abort on mismatch (R3-Q2/R6-Q3).
verify_sha256() {
    local expected="$1" file="$2"
    local actual
    actual="$(sha256sum "${file}" | cut -d' ' -f1)"
    # Fixed-length 64-hex compare; never accept a truncated/partial match.
    [[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || die "malformed expected sha256 '${expected}'"
    [[ "${actual}" == "${expected}" ]] || {
        die "sha256 mismatch for ${file}: expected ${expected}, got ${actual}; aborting"
    }
}

# --- parse flags (boundary parse: everything is validated up front) -------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --sha256)           SHA256="${2:-}"; shift 2 ;;
        --image-tar)        IMAGE_TAR="${2:-}"; shift 2 ;;
        --bucket)           BUCKET="${2:-}"; shift 2 ;;
        --region)           REGION="${2:-}"; shift 2 ;;
        --stack-name)       STACK_NAME="${2:-}"; shift 2 ;;
        --instance-id)      INSTANCE_ID="${2:-}"; shift 2 ;;
        --gitd-release-tag) GITD_RELEASE_TAG="${2:-}"; shift 2 ;;
        --dist-repo)        DIST_REPO="${2:-}"; shift 2 ;;
        -h|--help)
            cat <<'HELP'
update.sh — in-place update of a live gitd server (R3-Q10/R6-Q3).

Usage:
  cloudformation/update.sh --sha256 <hex> [opts]
  GITD_IMAGE_SHA256=<hex> cloudformation/update.sh [opts]

The image must already be built + GPG-signed + published by `make release`
(release.sh signs it -> gitd-container.tar.asc). update.sh VERIFIES that signed
image (verify_artifact against the pinned public key + the operator's pinned
sha256), mirrors it onto the artifact channel if needed, then updates the live
host over SSM: fetch -> verify GPG + pinned sha256 -> ctr images import ->
restart gitd units. It does NOT build or sign.

The sha256 pin must be supplied out-of-band (never fetched from the artifact
channel). If gitd-container.tar(.asc) is absent, run `make release` first.

The session output is captured and the roll is VERIFIED (in-band success
sentinel "GITD_UPDATE_OK" and/or "EXIT_CODE: 0" from separateOutputStream=true)
before success is reported — the local start-session exit code is always 0 and
is never trusted. start-session needs the session-manager-plugin and a TTY;
non-TTY runs are wrapped in unbuffer (expect) to avoid a start-session EOF.
Still does NOT build or sign.

Options:
  --sha256 <hex>          pinned sha256 of the new gitd-container.tar
  --image-tar <path>      prebuilt, signed gitd-container.tar (default: tools/dist/out/gitd-container.tar)
  --bucket <bucket>       default git.cmposer.cc
  --region <region>       default us-east-2
  --stack-name <name>     default gitd
  --instance-id <id>      override instance resolution from the stack
  --gitd-release-tag <t>  gitd-container family release tag on ChronicCmposer/gitd (default gitd-container)
  --dist-repo <o/r>       gitd repo (default ChronicCmposer/gitd)

Environment: GITD_IMAGE_SHA256, authenticated gh CLI (gh auth login), aws credentials.
HELP
            exit 0
            ;;
        *) die "unknown option '$1' (see --help)" ;;
    esac
done

# --- early exit: required inputs (fail-fast, code-philosophy) --------------------
[[ -n "${SHA256}" ]] || SHA256="${GITD_IMAGE_SHA256:-}"
require_value "--sha256 (or GITD_IMAGE_SHA256)" "${SHA256}"
[[ "${SHA256}" =~ ^[0-9a-f]{64}$ ]] || die "malformed sha256 pin '${SHA256}' (expected 64 lowercase hex)"

require_cmd aws
require_cmd curl
require_cmd sha256sum
require_cmd base64
# start-session needs the local session-manager-plugin (Session Manager client),
# or the call fails before the plugin can even open the tunnel — fail fast.
require_cmd session-manager-plugin
# Publish to GitHub needs gh installed AND authenticated — fail fast, never
# silently skip a publish (code-philosophy).
require_gh_auth

# --- the image tarball must already exist, pre-signed (R3-Q10) ---------------------
# update.sh does NOT build: the image is built + GPG-signed + published by
# `make release` (release.sh). Default is the release output path; a custom
# prebuilt path may be supplied via --image-tar. Fail fast if it is absent.
[[ -f "${IMAGE_TAR}" ]] || die "image tarball not found: ${IMAGE_TAR} (run 'make release' first — it builds and signs the image)"

# R6-Q3: the operator-supplied pin must match the artifact we are about to ship.
# Abort rather than publish something that won't verify on the host.
echo "gitd: update: verifying local tarball ${IMAGE_TAR} against pinned sha256"
verify_sha256 "${SHA256}" "${IMAGE_TAR}"
echo "gitd: update: local sha256 OK: ${SHA256}"

# Provenance: the image was GPG-signed by `make release`; update.sh VERIFIES it,
# it does NOT re-sign. Require the .asc to exist, then confirm the signature is
# good against the pinned public key. verify_artifact fails loudly.
[[ -f "${SIGNING_KEY}" ]] || die "pinned GPG signing key not found: ${SIGNING_KEY}"
[[ -f "${IMAGE_TAR}.asc" ]] || die "signed image .asc missing: ${IMAGE_TAR}.asc (run 'make release' first — it signs and publishes the image)"
verify_artifact "${IMAGE_TAR}" "${SIGNING_KEY}"

# --- publish to the artifact channel, idempotently (R3-Q2) -------------------------
# GitHub Releases primary + S3 fallback; upload tar + .asc together. Skip
# re-uploading an already-published asset so re-runs don't churn the release.
if gh release view "${GITD_RELEASE_TAG}" --repo "${DIST_REPO}" >/dev/null 2>&1; then
    if gh release view "${GITD_RELEASE_TAG}" --repo "${DIST_REPO}" \
        --json assets --jq '.assets[].name' 2>/dev/null | grep -qx "gitd-container.tar"; then
        echo "gitd: update: gitd-container.tar already on ${DIST_REPO}@${GITD_RELEASE_TAG}; skipping upload"
    else
        gh release upload "${GITD_RELEASE_TAG}" "${IMAGE_TAR}" "${IMAGE_TAR}.asc" --repo "${DIST_REPO}" --clobber
        echo "gitd: update: uploaded gitd-container.tar + .asc to ${DIST_REPO}@${GITD_RELEASE_TAG}"
    fi
else
    gh release create "${GITD_RELEASE_TAG}" "${IMAGE_TAR}" "${IMAGE_TAR}.asc" --repo "${DIST_REPO}" \
        --title "gitd OCI image" \
        --notes "gitd-container.tar (R3-Q2). sha256: ${SHA256}. GPG-signed (gitd-signing-key.asc)."
    echo "gitd: update: created ${DIST_REPO}@${GITD_RELEASE_TAG} and uploaded gitd-container.tar + .asc"
fi
aws s3 cp "${IMAGE_TAR}" "s3://${BUCKET}/image/gitd-container.tar" --region "${REGION}" --only-show-errors
aws s3 cp "${IMAGE_TAR}.asc" "s3://${BUCKET}/image/gitd-container.tar.asc" --region "${REGION}" --only-show-errors
echo "gitd: update: image published to ${DIST_REPO}@${GITD_RELEASE_TAG} + s3://${BUCKET}/image/ (tar + .asc)"

# --- resolve the instance (stack, unless overridden) --------------------------------
if [[ -z "${INSTANCE_ID}" ]]; then
    INSTANCE_ID="$(aws cloudformation list-stack-resources --stack-name "${STACK_NAME}" \
        --region "${REGION}" \
        --query "StackResourceSummaries[?LogicalResourceId=='GitdInstance'].PhysicalResourceId" \
        --output text)"
    require_value "resolved instance id (stack '${STACK_NAME}')" "${INSTANCE_ID}"
fi
echo "gitd: update: target instance ${INSTANCE_ID}"

# --- the host-side script (run as root over SSM, host plane, R2-Q16) --------------
# Generated here with the pin + artifact URLs + pinned GPG public key baked in
# (the pin is the operator's out-of-band value carried forward, never fetched
# from the channel; the pubkey is public and committed in-repo). Base64 is used
# as the transport envelope so no shell/JSON quoting survives the trip.
SIGNING_PUBKEY="$(cat "${SIGNING_KEY}")"
build_remote_body() {
    cat <<REMOTE_EOF
set -euo pipefail
# Host-plane elevation (R2-Q16): the SSM agent may run this body as ssm-user, in
# which case every privileged step below (dnf, /opt, systemctl) fails silently.
# Fail LOUDLY up front instead of reporting a false success.
[[ "\${EUID}" -eq 0 ]] || { echo "gitd: update: host plane must run as root (current EUID=\${EUID}); aborting" >&2; exit 1; }
EXPECTED_SHA='${SHA256}'
REGION='${REGION}'
GITHUB_IMAGE_URL='https://github.com/${DIST_REPO}/releases/download/${GITD_RELEASE_TAG}/gitd-container.tar'
GITHUB_IMAGE_ASC_URL='\${GITHUB_IMAGE_URL}.asc'
S3_IMAGE_URL='s3://${BUCKET}/image/gitd-container.tar'
S3_IMAGE_ASC_URL='s3://${BUCKET}/image/gitd-container.tar.asc'
TAR='/opt/gitd-container.tar'
SIG="\${TAR}.asc"
PINNED_PUBKEY='/opt/gitd-signing-key.asc'
die() { echo "gitd: update: \$*" >&2; exit 1; }
# The pinned public key (committed at tools/release/gitd-signing-key.asc) is
# carried into the remote body by the operator-side script; it is public, so
# carrying it inline leaks nothing. It is written to a temp file for gpg.
cat > "\$PINNED_PUBKEY" <<'GPGKEY'
${SIGNING_PUBKEY}
GPGKEY
# GPG verification is REQUIRED (provenance on top of the pinned sha256), but it
# is agent-free: the verify below uses --no-autostart (public-key only, no
# gpg-agent needed). AL2023 ships gnupg2-minimal (gpg present, conflicts with
# the full gnupg2), so the fallback install is gnupg2-minimal — never gnupg2.
command -v gpg >/dev/null 2>&1 || { echo "gitd: update: gpg not found; installing gnupg2-minimal" >&2; dnf install -y -q gnupg2-minimal; }
command -v gpg >/dev/null 2>&1 || die "gpg still missing after install; cannot verify artifact provenance"
echo "gitd: update: fetching gitd-container.tar + .asc (GitHub primary, S3 fallback)"
rm -f "\$TAR.dl" "\$SIG.dl"
if curl -fsSL --retry 3 --connect-timeout 15 "\$GITHUB_IMAGE_URL" -o "\$TAR.dl"; then
    curl -fsSL --retry 3 --connect-timeout 15 "\$GITHUB_IMAGE_ASC_URL" -o "\$SIG.dl" \\
        || die "image from GitHub but signature \$GITHUB_IMAGE_ASC_URL missing; refusing unsigned image"
    mv "\$TAR.dl" "\$TAR"; mv "\$SIG.dl" "\$SIG"
    echo "gitd: update: fetched from GitHub Releases (${GITD_RELEASE_TAG}) + .asc"
elif aws s3 cp "\$S3_IMAGE_URL" "\$TAR.dl" --region "\$REGION" --only-show-errors; then
    aws s3 cp "\$S3_IMAGE_ASC_URL" "\$SIG.dl" --region "\$REGION" --only-show-errors \\
        || die "image from S3 but signature \$S3_IMAGE_ASC_URL missing; refusing unsigned image"
    mv "\$TAR.dl" "\$TAR"; mv "\$SIG.dl" "\$SIG"
    echo "gitd: update: fetched from S3 fallback + .asc"
else
    rm -f "\$TAR.dl"
    echo "gitd: update: ERROR: could not fetch gitd-container.tar from GitHub or S3" >&2
    exit 1
fi
[[ -s "\$TAR" ]] || die "gitd-container.tar empty after download"
# GPG provenance first (against the pinned key), then the pinned sha256.
VHOME=\$(mktemp -d)
trap 'rm -rf "\$VHOME"' EXIT
gpg --homedir "\$VHOME" --batch --quiet --no-tty --no-autostart --import "\$PINNED_PUBKEY" \\
    || die "failed to import pinned GPG public key"
gpg --homedir "\$VHOME" --batch --quiet --no-tty --no-autostart --verify "\$SIG" "\$TAR" \\
    || die "GPG signature verification FAILED for \$TAR; provenance not proven — aborting"
rm -rf "\$VHOME"; trap - EXIT
echo "gitd: update: GPG signature verified (pinned key)"
ACTUAL=\$(sha256sum "\$TAR" | cut -d' ' -f1)
[[ "\$ACTUAL" == "\$EXPECTED_SHA" ]] || {
    echo "gitd: update: sha256 MISMATCH for \$TAR: expected \$EXPECTED_SHA, got \$ACTUAL; aborting (R6-Q3)" >&2
    exit 1
}
echo "gitd: update: sha256 verified: \$EXPECTED_SHA"
# --base-name (NOT --ref, which ctr images import does not have): the tar's
# index.json carries the "latest" ref annotation baked by
# tools/dist/package-image.sh, so the repo comes from the base name and the
# tag from the annotation (git.cmposer.cc/gitd:latest).
ctr -n default images import --base-name ${IMAGE_BASE} "\$TAR"
ctr -n default images ls | grep -q '${IMAGE_REF}' || {
    echo "gitd: update: ERROR: import did not register ${IMAGE_REF}" >&2
    exit 1
}
echo "gitd: update: ${IMAGE_REF} imported"
systemctl restart gitd-serve.service   # first: gitd-sshd After=gitd-serve (R12-Q5)
systemctl restart gitd-sshd.service
systemctl restart gitd-ddns.service
echo "gitd: update: units restarted (gitd-serve, gitd-sshd, gitd-ddns); EBS + spool untouched"
# Success sentinel: printed ONLY after the whole set -euo pipefail body above
# (GPG verify, sha256 verify, ctr import, unit restarts) has run. The operator
# greps this token (plus EXIT_CODE: 0) to confirm the roll really succeeded.
echo "GITD_UPDATE_OK"
REMOTE_EOF
}

REMOTE_B64="$(build_remote_body | base64 -w0)"
REMOTE_CMD="printf '%s' '${REMOTE_B64}' | base64 -d | bash"

# --- run the roll over SSM and VERIFY the remote result --------------------------
# `aws ssm start-session` ALWAYS exits 0 regardless of the remote command's
# outcome (AWS "by design"), so its exit code is never trusted. Instead: (1) the
# session output is captured via tee to a temp file (the operator still sees it
# live), (2) the document is invoked with separateOutputStream=true so it emits
# an EXIT_CODE: N line, and (3) success is reported ONLY once the in-band
# success sentinel GITD_UPDATE_OK and/or "EXIT_CODE: 0" appear in the captured
# output. A failed roll is reported loudly, never as success.
SESSION_LOG="$(mktemp)"
trap 'rm -f "${SESSION_LOG}"' EXIT

# The base64-envelope command (single `command` value) contains no commas, so
# the comma-separated `command=<cmd>,separateOutputStream=true` parameter form
# is safe. separateOutputStream=true makes the document emit the EXIT_CODE: N
# line that we also check below.
PARAMS="{\"command\":[\"${REMOTE_CMD}\"],\"separateOutputStream\":[\"true\"]}"
START_SESSION=(aws ssm start-session \
    --region "${REGION}" \
    --target "${INSTANCE_ID}" \
    --document-name AWS-StartNonInteractiveCommand \
    --parameters "${PARAMS}")

# start-session is TTY-dependent: in a scripted (non-TTY) context it can die
# with "Cannot perform start session: EOF". When stdout is not a TTY we wrap the
# session in `unbuffer` (expect) as a mitigation. unbuffer is OPTIONAL (a soft
# requirement, not a hard require_cmd): if it is absent we still run and the
# sentinel check below catches a botched session.
echo "gitd: update: running in-place update on ${INSTANCE_ID} over SSM (output captured for verification)"
# The start-session local exit code is meaningless (always 0), so suppress
# errexit around the session and judge the outcome from the captured output.
set +e
if [[ ! -t 1 ]] && command -v unbuffer >/dev/null 2>&1; then
    echo "gitd: update: stdout is not a TTY; wrapping session in unbuffer (expect) to avoid 'start session: EOF'"
    unbuffer "${START_SESSION[@]}" 2>&1 | tee "${SESSION_LOG}"
else
    "${START_SESSION[@]}" 2>&1 | tee "${SESSION_LOG}"
fi
SESSION_STATUS="${PIPESTATUS[0]}"
set -e
# shellcheck disable=SC2181
if [[ "${SESSION_STATUS}" -ne 0 ]]; then
    echo "gitd: update: warning: aws ssm start-session exited ${SESSION_STATUS}; remote result is judged from captured output" >&2
fi

echo "gitd: update: session finished; verifying remote result from captured output"
# Authoritative signal: the in-band sentinel only prints after the WHOLE remote
# `set -euo pipefail` body (GPG verify, sha256 verify, import, restarts) ran.
# "EXIT_CODE: 0" (from separateOutputStream=true) is secondary corroboration.
if grep -q "GITD_UPDATE_OK" "${SESSION_LOG}" || grep -q "EXIT_CODE: 0" "${SESSION_LOG}"; then
    echo "gitd: update: verified: new image ${SHA256} rolled onto ${INSTANCE_ID}"
else
    echo "gitd: update: remote update did NOT report success; captured output:" >&2
    cat "${SESSION_LOG}" >&2
    die "remote roll of new image ${SHA256} onto ${INSTANCE_ID} failed (no success sentinel / EXIT_CODE: 0 in session output)"
fi
