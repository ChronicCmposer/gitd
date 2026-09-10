#!/usr/bin/env bash
# cloudformation/deploy.sh — build the deployment bundle and deploy the stack.
#
# Phase 7 orchestration (7.3): packages the host binaries + configs + userdata
# into a deployment bundle, VERIFIES the already-signed gitd-container.tar
# (signed + published by 'make release') against the pinned key, creates the
# gitd-container family release on ChronicCmposer/gitd only if missing, pushes
# the client-side certs to SSM, then creates/updates the CloudFormation stack.
# Fail-fast and idempotent-ish (create if absent, update if present).
#
# Order matters (R3-Q2/R3-Q3): the bundle, image, and SSM certs must all exist
# BEFORE create-stack, because the instance's bootstrap pulls them at first
# boot. This script guarantees that ordering.
#
# Usage:
#   cloudformation/deploy.sh --key-name <kp> [opts]
#
# Options:
#   --stack-name <name>          stack name (default gitd)
#   --region <region>            default us-east-2
#   --key-name <kp>              EC2 keypair (required)
#   --instance-type <type>       default t4g.nano
#   --bucket <bucket>            default git.cmposer.cc
#   --image-tar <path>           gitd-container.tar (default tools/dist/out/gitd-container.tar)
#   --gitd-release-tag <tag>     gitd-container family release tag on ChronicCmposer/gitd (default gitd-container)
#   --bundle-dir <dir>           staging dir for the bundle (default cloudformation/out)
# Environment: the image release is assumed already signed + published by
# 'make release' (Option B); deploy.sh VERIFIES it and needs gh CLI only when
# the release is missing (create path).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# --- defaults ------------------------------------------------------------------
STACK_NAME="gitd"
REGION="us-east-2"
KEY_NAME=""
INSTANCE_TYPE="t4g.nano"
BUCKET="git.cmposer.cc"
IMAGE_TAR="${REPO_ROOT}/tools/dist/out/gitd-container.tar"
GITD_RELEASE_TAG="gitd-container"
BUNDLE_DIR="${SCRIPT_DIR}/out"
DIST_REPO="ChronicCmposer/gitd"

die() {
    echo "gitd: deploy: $*" >&2
    exit 1
}
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}
# Shared gh CLI auth guard (gh_auth / require_gh_auth) — gh reads its own
# stored credentials (GH_TOKEN, if set, is used by gh as an override).
# shellcheck source=tools/release/gh-auth.sh
source "${SCRIPT_DIR}/../tools/release/gh-auth.sh"

# --- parse flags ---------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --stack-name)        STACK_NAME="${2:?missing value}"; shift 2 ;;
        --region)            REGION="${2:?missing value}"; shift 2 ;;
        --key-name)          KEY_NAME="${2:?missing value}"; shift 2 ;;
        --instance-type)     INSTANCE_TYPE="${2:?missing value}"; shift 2 ;;
        --bucket)            BUCKET="${2:?missing value}"; shift 2 ;;
        --image-tar)         IMAGE_TAR="${2:?missing value}"; shift 2 ;;
        --gitd-release-tag)  GITD_RELEASE_TAG="${2:?missing value}"; shift 2 ;;
        --bundle-dir)        BUNDLE_DIR="${2:?missing value}"; shift 2 ;;
        -h|--help)
            cat <<'HELP'
deploy.sh — build the deployment bundle + deploy the gitd CloudFormation stack.

Usage:
  cloudformation/deploy.sh --key-name <kp> [opts]

Options:
  --stack-name <name>          stack name (default gitd)
  --region <region>            default us-east-2
  --key-name <kp>              EC2 keypair (required)
  --instance-type <type>       default t4g.nano
  --bucket <bucket>            default git.cmposer.cc
  --image-tar <path>           gitd-container.tar (default tools/dist/out/gitd-container.tar)
  --gitd-release-tag <tag>     gitd-container family release tag on ChronicCmposer/gitd (default gitd-container)
  --bundle-dir <dir>           staging dir for the bundle (default cloudformation/out)

Environment: the image is assumed already signed + published by 'make release'
(Option B); deploy.sh VERIFIES it against the pinned key and only creates the
gitd-container release on ChronicCmposer/gitd if missing (gh CLI then required).
HELP
            exit 0
            ;;
        *) die "unknown option '$1' (see --help)" ;;
    esac
done

# --- early exit: required inputs (fail-fast, code-philosophy) --------------------
[[ -n "${KEY_NAME}" ]] || die "--key-name is required"

require_cmd aws
require_cmd sha256sum
require_cmd tar
# gh is needed ONLY in the create path below (when the release is missing);
# require_gh_auth runs there, not here. aws/sha256sum/tar stay fail-fast.

# Shared GPG signing helpers (sign_artifact / verify_artifact) + the committed
# pinned public key the bundle carries so the instance can verify provenance.
# shellcheck source=tools/release/sign-artifact.sh
source "${SCRIPT_DIR}/../tools/release/sign-artifact.sh"
SIGNING_KEY="${SCRIPT_DIR}/../tools/release/gitd-signing-key.asc"
[[ -f "${SIGNING_KEY}" ]] || die "pinned GPG signing key not found: ${SIGNING_KEY}"

# --- artifact inputs must exist ---------------------------------------------------
[[ -f "${IMAGE_TAR}" ]] || die "image tarball not found: ${IMAGE_TAR} (run 'make image-container')"
# Host binaries come from the dist pipeline (already determinism-checked).
CONTAINERD_FILE="$(find "${REPO_ROOT}/tools/dist/out" -maxdepth 1 -name 'containerd-*.linux-arm64.tar.gz' -type f 2>/dev/null | head -n1 || true)"
RUNC_FILE="$(find "${REPO_ROOT}/tools/dist/out" -maxdepth 1 -name 'runc-*.linux-arm64.tar.gz' -type f 2>/dev/null | head -n1 || true)"
[[ -n "${CONTAINERD_FILE}" ]] || die "containerd arm64 tarball not found (run 'make check-containerd-dist')"
[[ -n "${RUNC_FILE}" ]] || die "runc arm64 tarball not found (run 'make check-runc-dist')"
[[ -f "${REPO_ROOT}/configs/gitd.yaml" ]] || die "configs/gitd.yaml missing"
[[ -f "${REPO_ROOT}/configs/webhooks.yaml" ]] || die "configs/webhooks.yaml missing"
[[ -f "${SCRIPT_DIR}/userdata.sh" ]] || die "cloudformation/userdata.sh missing"
[[ -f "${REPO_ROOT}/pki/gitd-cert-sync.sh" ]] || die "pki/gitd-cert-sync.sh missing"
[[ -f "${REPO_ROOT}/tools/dist/containerd/containerd.service" ]] || die "containerd.service missing"
[[ -f "${REPO_ROOT}/tools/dist/containerd/config.toml" ]] || die "config.toml missing"

# --- artifact sha256 pins (R3-Q2) --------------------------------------------------
IMAGE_SHA256="$(sha256sum "${IMAGE_TAR}" | cut -d' ' -f1)"
CONTAINERD_SHA256="$(sha256sum "${CONTAINERD_FILE}" | cut -d' ' -f1)"
RUNC_SHA256="$(sha256sum "${RUNC_FILE}" | cut -d' ' -f1)"
# RepoRef-style pin: the gitd commit SHA that produced these artifacts.
require_cmd git
REPO_REF_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD)"

# GPG-VERIFY the already-signed image against the pinned key before
# packaging/publishing. The image is signed by 'make release'; deploy.sh does
# NOT re-sign (Option B — assume the release is already signed).
# verify_artifact fails loudly on any provenance problem.
[[ -f "${IMAGE_TAR}.asc" ]] || die "signed image .asc missing: ${IMAGE_TAR}.asc (run 'make release' first — it signs and publishes the image)"
verify_artifact "${IMAGE_TAR}" "${SIGNING_KEY}"

# --- build the deployment bundle (R13-Q4: configs written verbatim at boot) ---------
STAGE="$(mktemp -d "${BUNDLE_DIR}/bundle.XXXXXX")"
trap 'rm -rf "${STAGE}"' EXIT
mkdir -p "${STAGE}"
cp "${SCRIPT_DIR}/userdata.sh"                              "${STAGE}/userdata.sh"
cp "${REPO_ROOT}/configs/gitd.yaml"                         "${STAGE}/gitd.yaml"
cp "${REPO_ROOT}/configs/webhooks.yaml"                     "${STAGE}/webhooks.yaml"
cp "${CONTAINERD_FILE}"                                     "${STAGE}/$(basename "${CONTAINERD_FILE}")"
cp "${RUNC_FILE}"                                           "${STAGE}/$(basename "${RUNC_FILE}")"
cp "${REPO_ROOT}/tools/dist/containerd/containerd.service"  "${STAGE}/containerd.service"
cp "${REPO_ROOT}/tools/dist/containerd/config.toml"         "${STAGE}/config.toml"
cp "${REPO_ROOT}/pki/gitd-cert-sync.sh"                     "${STAGE}/gitd-cert-sync.sh"
# The image tarball rides in the bundle as a local fallback (7.3), though
# userdata fetches it from GitHub primary / S3 fallback (7.2, R3-Q2). The
# detached .asc + pinned public key + shared verify helper ride along so the
# fallback path can still prove provenance (fail-fast, never trust unsigned).
cp "${IMAGE_TAR}"                                           "${STAGE}/gitd-container.tar"
[[ -f "${IMAGE_TAR}.asc" ]] || die "signed image .asc missing: ${IMAGE_TAR}.asc (sign_artifact should have produced it)"
cp "${IMAGE_TAR}.asc"                                       "${STAGE}/gitd-container.tar.asc"
cp "${SIGNING_KEY}"                                         "${STAGE}/gitd-signing-key.asc"
cp "${SCRIPT_DIR}/../tools/release/sign-artifact.sh"        "${STAGE}/sign-artifact.sh"

BUNDLE_TAR="${BUNDLE_DIR}/gitd-bundle.tar.gz"
mkdir -p "${BUNDLE_DIR}"
tar -C "${STAGE}" --sort=name --owner=0 --group=0 --numeric-owner \
    -czf "${BUNDLE_TAR}" .
BUNDLE_SHA256="$(sha256sum "${BUNDLE_TAR}" | cut -d' ' -f1)"
BUNDLE_S3_KEY="bundles/gitd-bundle-${BUNDLE_SHA256}.tar.gz"
cp "${BUNDLE_TAR}" "${BUNDLE_DIR}/gitd-bundle-${BUNDLE_SHA256}.tar.gz"
echo "gitd: deploy: bundle: ${BUNDLE_TAR} (sha256 ${BUNDLE_SHA256})"

# --- upload the bundle to S3 (before create-stack) -----------------------------------
aws s3 cp "${BUNDLE_TAR}" "s3://${BUCKET}/${BUNDLE_S3_KEY}" --region "${REGION}" --only-show-errors
echo "gitd: deploy: bundle uploaded to s3://${BUCKET}/${BUNDLE_S3_KEY}"

# --- publish gitd-container.tar to the gitd-container family release on
# ChronicCmposer/gitd + S3 image fallback. Option B: the release is assumed
# already signed + published by 'make release'; only create when MISSING. -----
if gh release view "${GITD_RELEASE_TAG}" --repo "${DIST_REPO}" >/dev/null 2>&1; then
    echo "gitd: deploy: gitd-container release already exists on ChronicCmposer/gitd; assuming already signed+published, skipping"
else
    require_gh_auth
    gh release create "${GITD_RELEASE_TAG}" "${IMAGE_TAR}" "${IMAGE_TAR}.asc" --repo "${DIST_REPO}" \
        --title "gitd OCI image" \
        --notes "gitd-container.tar (R3-Q2). sha256: ${IMAGE_SHA256}. GPG-signed (gitd-signing-key.asc)."
    echo "gitd: deploy: created gitd-container release on ChronicCmposer/gitd (tar + .asc)"
fi
aws s3 cp "${IMAGE_TAR}" "s3://${BUCKET}/image/gitd-container.tar" --region "${REGION}" --only-show-errors
aws s3 cp "${IMAGE_TAR}.asc" "s3://${BUCKET}/image/gitd-container.tar.asc" --region "${REGION}" --only-show-errors
echo "gitd: deploy: image verified; S3 fallback mirror at s3://${BUCKET}/image/ (tar + .asc)"

# --- push client-side certs to SSM BEFORE create-stack (R3-Q3) ------------------------
"${SCRIPT_DIR}/upload-certs.sh"

# --- create or update the stack ---------------------------------------------------------
PARAMS=(
    "ParameterKey=KeyName,ParameterValue=${KEY_NAME}"
    "ParameterKey=InstanceType,ParameterValue=${INSTANCE_TYPE}"
    "ParameterKey=BucketName,ParameterValue=${BUCKET}"
    "ParameterKey=BundleS3Key,ParameterValue=${BUNDLE_S3_KEY}"
    "ParameterKey=BundleSha256,ParameterValue=${BUNDLE_SHA256}"
    "ParameterKey=ImageSha256,ParameterValue=${IMAGE_SHA256}"
    "ParameterKey=ContainerdSha256,ParameterValue=${CONTAINERD_SHA256}"
    "ParameterKey=RuncSha256,ParameterValue=${RUNC_SHA256}"
    "ParameterKey=GitdReleaseTag,ParameterValue=${GITD_RELEASE_TAG}"
    "ParameterKey=RepoRefSha,ParameterValue=${REPO_REF_SHA}"
)

if aws cloudformation describe-stacks --stack-name "${STACK_NAME}" --region "${REGION}" >/dev/null 2>&1; then
    echo "gitd: deploy: stack '${STACK_NAME}' exists; updating"
    aws cloudformation update-stack \
        --stack-name "${STACK_NAME}" \
        --template-body "file://${SCRIPT_DIR}/stack.yaml" \
        --parameters "${PARAMS[@]}" \
        --capabilities CAPABILITY_IAM \
        --region "${REGION}" >/dev/null
    aws cloudformation wait stack-update-complete --stack-name "${STACK_NAME}" --region "${REGION}"
    echo "gitd: deploy: stack update complete"
else
    echo "gitd: deploy: creating stack '${STACK_NAME}'"
    aws cloudformation create-stack \
        --stack-name "${STACK_NAME}" \
        --template-body "file://${SCRIPT_DIR}/stack.yaml" \
        --parameters "${PARAMS[@]}" \
        --capabilities CAPABILITY_IAM \
        --region "${REGION}" >/dev/null
    # create-stack completes only after the instance's boot signal (CreationPolicy).
    aws cloudformation wait stack-create-complete --stack-name "${STACK_NAME}" --region "${REGION}"
    echo "gitd: deploy: stack create complete (boot signal received)"
fi

echo "gitd: deploy: done"
