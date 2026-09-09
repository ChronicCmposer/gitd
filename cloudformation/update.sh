#!/usr/bin/env bash
# cloudformation/update.sh — in-place update of a live gitd server (R3-Q10, R6-Q3).
#
# This is NOT deploy.sh. CloudFormation stays for initial infra + emergency
# rebuild only; routine upgrades ship a new image in place: build (or reuse) the
# OCI tarball, publish it to the artifact channel, then over SSM have the host
# fetch it, verify the pinned sha256, ctr-images import it, and restart the
# three container units. EBS (/srv/git) + spool (/var/spool/gitd) are preserved
# untouched — an update never does a bundle restore (mirrors are a backup, not a
# deploy input).
#
# The critical security property (R6-Q3): the sha256 pin is provided OUT OF BAND
# by the operator, as an argument or the GITD_IMAGE_SHA256 env var — the value
# printed by `make image-container` / the release notes. It is NEVER fetched from
# the artifact channel. The script verifies the local tarball against that pin
# before publishing, and the host re-verifies its download against the SAME pin
# before importing. Any mismatch aborts.
#
# Transport: host-plane ops (ctr import, systemctl) go over SSM Session Manager
# (AWS-StartNonInteractiveCommand), which is the only elevation the admin split
# allows for the host plane (R2-Q16); the container SSH shell has no
# ctr/systemctl.
#
# Usage:
#   cloudformation/update.sh --sha256 <hex> [opts]
#   GITD_IMAGE_SHA256=<hex> cloudformation/update.sh [opts]
#
# Options:
#   --sha256 <hex>          pinned sha256 of the NEW gitd-container.tar (R6-Q3;
#                           required, or GITD_IMAGE_SHA256)
#   --image-tar <path>      prebuilt gitd-container.tar (default: build via
#                           `make image-container`)
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
IMAGE_TAR=""
BUCKET="git.cmposer.cc"
REGION="us-east-2"
STACK_NAME="gitd"
INSTANCE_ID=""
GITD_RELEASE_TAG="gitd-container"
DIST_REPO="ChronicCmposer/gitd"
IMAGE_REF="git.cmposer.cc/gitd:latest"

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

The sha256 pin must be supplied out-of-band (never fetched from the artifact
channel). Build (or --image-tar), publish, then update the live host over SSM:
fetch -> verify pinned sha256 -> ctr images import -> restart gitd units.

Options:
  --sha256 <hex>          pinned sha256 of the new gitd-container.tar
  --image-tar <path>      prebuilt gitd-container.tar (default: make image-container)
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
# Publish to GitHub needs gh installed AND authenticated — fail fast, never
# silently skip a publish (code-philosophy).
require_gh_auth

# --- obtain the image tarball: prebuilt, or build it (R3-Q10) ---------------------
if [[ -n "${IMAGE_TAR}" ]]; then
    [[ -f "${IMAGE_TAR}" ]] || die "--image-tar not found: ${IMAGE_TAR}"
else
    echo "gitd: update: building the OCI image (make image-container)"
    ( cd "${REPO_ROOT}" && make image-container )
    IMAGE_TAR="${REPO_ROOT}/tools/dist/out/gitd-container.tar"
    [[ -f "${IMAGE_TAR}" ]] || die "build did not produce ${IMAGE_TAR}"
fi

# R6-Q3: the operator-supplied pin must match the artifact we are about to ship.
# If a rebuild produced a different hash, the operator gave us the wrong pin —
# abort rather than publish something that won't verify on the host.
echo "gitd: update: verifying local tarball ${IMAGE_TAR} against pinned sha256"
verify_sha256 "${SHA256}" "${IMAGE_TAR}"
echo "gitd: update: local sha256 OK: ${SHA256}"

# GPG-sign the image before publishing: the host refuses any image whose .asc
# it cannot verify against the pinned public key. sign_artifact fails loudly.
[[ -f "${SIGNING_KEY}" ]] || die "pinned GPG signing key not found: ${SIGNING_KEY}"
sign_artifact "${IMAGE_TAR}"

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
# GPG verification is REQUIRED (provenance on top of the pinned sha256). Install
# gnupg2 if absent; fail fast if it still cannot verify.
command -v gpg >/dev/null 2>&1 || { echo "gitd: update: gpg not found; installing gnupg2" >&2; dnf install -y -q gnupg2; }
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
gpg --homedir "\$VHOME" --batch --quiet --import "\$PINNED_PUBKEY" \\
    || die "failed to import pinned GPG public key"
gpg --homedir "\$VHOME" --batch --quiet --verify "\$SIG" "\$TAR" \\
    || die "GPG signature verification FAILED for \$TAR; provenance not proven — aborting"
rm -rf "\$VHOME"; trap - EXIT
echo "gitd: update: GPG signature verified (pinned key)"
ACTUAL=\$(sha256sum "\$TAR" | cut -d' ' -f1)
[[ "\$ACTUAL" == "\$EXPECTED_SHA" ]] || {
    echo "gitd: update: sha256 MISMATCH for \$TAR: expected \$EXPECTED_SHA, got \$ACTUAL; aborting (R6-Q3)" >&2
    exit 1
}
echo "gitd: update: sha256 verified: \$EXPECTED_SHA"
ctr -n default images import "\$TAR"
ctr -n default images ls | grep -q '${IMAGE_REF}' || {
    echo "gitd: update: ERROR: import did not register ${IMAGE_REF}" >&2
    exit 1
}
echo "gitd: update: ${IMAGE_REF} imported"
systemctl restart gitd-serve.service   # first: gitd-sshd After=gitd-serve (R12-Q5)
systemctl restart gitd-sshd.service
systemctl restart gitd-ddns.service
echo "gitd: update: units restarted (gitd-serve, gitd-sshd, gitd-ddns); EBS + spool untouched"
REMOTE_EOF
}

REMOTE_B64="$(build_remote_body | base64 -w0)"
REMOTE_CMD="printf '%s' '${REMOTE_B64}' | base64 -d | bash"

echo "gitd: update: running in-place update on ${INSTANCE_ID} over SSM"
aws ssm start-session \
    --region "${REGION}" \
    --target "${INSTANCE_ID}" \
    --document-name AWS-StartNonInteractiveCommand \
    --parameters "{\"command\":[\"${REMOTE_CMD}\"]}" >/dev/null

echo "gitd: update: done (new image ${SHA256} live on ${INSTANCE_ID})"
