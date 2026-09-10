#!/usr/bin/env bash
# cloudformation/deploy.sh — build the deployment bundle and deploy the stack.
#
# Phase 7 orchestration (7.3): packages the host binaries + configs + userdata
# into a deployment bundle, VERIFIES the already-signed gitd-container.tar
# (signed + published by 'make release') against the pinned key, creates the
# gitd-container family release on ChronicCmposer/gitd only if missing, pushes
# the client-side certs to SSM, creates/updates the CloudFormation stack, then
# tails the instance console output so boot diagnostics (SSH auth + CA
# fingerprint checks) surface automatically.
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
#   --instance-type <type>       default t4g.micro
#   --bucket <bucket>            default git.cmposer.cc
#   --image-tar <path>           gitd-container.tar (default tools/dist/out/gitd-container.tar)
#   --gitd-release-tag <tag>     gitd-container family release tag on ChronicCmposer/gitd (default gitd-container)
#   --bundle-dir <dir>           staging dir for the bundle (default cloudformation/out)
#   --debug                      shell trace + raw AWS create/update-stack output
#   --no-console-tail            skip the post-deploy console-output tail
#   --console-tail-seconds <N>   console tail duration in seconds (default 120)
# Environment: the image release is assumed already signed + published by
# 'make release' (Option B); deploy.sh VERIFIES it and needs gh CLI only when
# the release is missing (create path). GITD_NO_CONSOLE_TAIL=1 also skips the
# post-deploy console tail.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# --- defaults ------------------------------------------------------------------
STACK_NAME="gitd"
REGION="us-east-2"
KEY_NAME=""
INSTANCE_TYPE="t4g.micro"
BUCKET="git.cmposer.cc"
IMAGE_TAR="${REPO_ROOT}/tools/dist/out/gitd-container.tar"
GITD_RELEASE_TAG="gitd-container"
BUNDLE_DIR="${SCRIPT_DIR}/out"
DIST_REPO="ChronicCmposer/gitd"
DEBUG_FLAG=0
CONSOLE_TAIL_FLAG=0
CONSOLE_TAIL_SECONDS=120

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
        --debug)             DEBUG_FLAG=1; shift ;;
        --no-console-tail)       CONSOLE_TAIL_FLAG=1; shift ;;
        --console-tail-seconds)  CONSOLE_TAIL_SECONDS="${2:?missing value}"; shift 2 ;;
        -h|--help)
            cat <<'HELP'
deploy.sh — build the deployment bundle + deploy the gitd CloudFormation stack.

Usage:
  cloudformation/deploy.sh --key-name <kp> [opts]

Options:
  --stack-name <name>          stack name (default gitd)
  --region <region>            default us-east-2
  --key-name <kp>              EC2 keypair (required)
  --instance-type <type>       default t4g.micro
  --bucket <bucket>            default git.cmposer.cc
  --image-tar <path>           gitd-container.tar (default tools/dist/out/gitd-container.tar)
  --gitd-release-tag <tag>     gitd-container family release tag on ChronicCmposer/gitd (default gitd-container)
  --bundle-dir <dir>           staging dir for the bundle (default cloudformation/out)
  --debug                      shell trace + raw AWS create/update-stack output
  --no-console-tail            skip the post-deploy console-output tail
  --console-tail-seconds <N>   console tail duration in seconds (default 120)

Environment: the image is assumed already signed + published by 'make release'
(Option B); deploy.sh VERIFIES it against the pinned key and only creates the
gitd-container release on ChronicCmposer/gitd if missing (gh CLI then required).
GITD_NO_CONSOLE_TAIL=1 also skips the post-deploy console tail.
HELP
            exit 0
            ;;
        *) die "unknown option '$1' (see --help)" ;;
    esac
done

# --- DEBUG control ----------------------------------------------------------------
# DEBUG turns on when either the env var is truthy (1/true/yes) OR the --debug
# flag was passed. Normalize to a single trusted 0/1 so the rest of the script
# branches on one value (parse, don't validate). When on, shell-trace everything.
case "${DEBUG:-}" in
    1|true|yes) DEBUG=1 ;;
    *)           DEBUG=0 ;;
esac
if [[ "${DEBUG_FLAG}" == "1" ]]; then
    DEBUG=1
fi
if [[ "${DEBUG}" == "1" ]]; then
    set -x
fi

# --- console-tail control -----------------------------------------------------------
# GITD_NO_CONSOLE_TAIL env (1/true/yes) or --no-console-tail both disable the
# post-deploy console tail. Normalize to a single trusted 0/1 so the step below
# branches on one value (parse, don't validate).
case "${GITD_NO_CONSOLE_TAIL:-}" in
    1|true|yes) CONSOLE_TAIL=0 ;;
    *)           CONSOLE_TAIL=1 ;;
esac
if [[ "${CONSOLE_TAIL_FLAG}" == "1" ]]; then
    CONSOLE_TAIL=0
fi

# Emit the AWS CLI --debug flag so the raw HTTPS request/response (which carries
# the full validation-error list) reaches the terminal. No-op when DEBUG is off,
# keeping the default behavior byte-identical.
cfn_debug() {
    if [[ "${DEBUG}" == "1" ]]; then
        echo "--debug"
    fi
}

# --- console tail helpers ------------------------------------------------------------
# Resolve the gitd instance ID: prefer the stack's GitdInstanceId output
# (declared in stack.yaml), else the running instance tagged with this stack.
# Prints the ID or nothing; NEVER fails the deploy (callers handle empty).
resolve_instance_id() {
    local instance_id

    instance_id="$(
        aws cloudformation describe-stacks --stack-name "${STACK_NAME}" --region "${REGION}" \
            --query 'Stacks[0].Outputs[?OutputKey==`GitdInstanceId`].OutputValue' \
            --output text 2>/dev/null || true
    )"
    if [[ -n "${instance_id}" ]]; then
        echo "${instance_id}"
        return
    fi

    instance_id="$(
        aws ec2 describe-instances --region "${REGION}" \
            --filters Name=tag:aws:cloudformation:stack-name,Values="${STACK_NAME}" \
                      Name=instance-state-name,Values=running \
            --query 'Reservations[].Instances[].InstanceId' \
            --output text 2>/dev/null || true
    )"
    # Multiple matches (shouldn't happen) — take the first ID.
    echo "${instance_id}" | awk 'NR==1 {print $1}'
}

# Tail the instance system log until the boot-complete marker ('boot complete',
# the userdata.sh success line) appears or the duration elapses. Prints only NEW
# lines per poll. This diagnostic step NEVER fails the deploy: a missing base64
# skips the step and transient get-console-output failures are noted + retried.
tail_console_output() {
    local instance_id="$1"
    local seconds="$2"
    local marker="boot complete"
    local poll_seconds=5
    local seen=$'\n'
    local output=""
    local line=""
    local elapsed=0
    local booted=0

    # Guard: decoding needs base64; skip (don't fail) the step when absent.
    if ! command -v base64 >/dev/null 2>&1; then
        echo "gitd: deploy: [console] base64 not found; skipping console tail"
        return 0
    fi

    echo "gitd: deploy: [console] tailing system log for instance ${instance_id} (up to ${seconds}s)"
    while (( elapsed < seconds )); do
        # First poll prints the most recent buffered output (everything is new);
        # later polls print only lines not seen in a previous iteration.
        if output="$(
            aws ec2 get-console-output --instance-id "${instance_id}" --region "${REGION}" \
                --latest --output text 2>/dev/null | base64 -d 2>/dev/null
        )"; then
            while IFS= read -r line; do
                [[ -n "${line}" ]] || continue
                if [[ "${seen}" == *$'\n'"${line}"$'\n'* ]]; then
                    continue
                fi
                seen="${seen}${line}"$'\n'
                echo "gitd: deploy: [console] ${line}"
                if [[ "${line}" == *"${marker}"* ]]; then
                    booted=1
                fi
            done <<< "${output}"
            if [[ "${booted}" == "1" ]]; then
                echo "gitd: deploy: [console] boot complete; stopping tail"
                return 0
            fi
        else
            echo "gitd: deploy: [console] get-console-output failed; retrying in ${poll_seconds}s"
        fi
        sleep "${poll_seconds}"
        elapsed=$(( elapsed + poll_seconds ))
    done
    echo "gitd: deploy: [console] tail finished (${seconds}s elapsed; boot marker not seen)"
}

# --- early exit: required inputs (fail-fast, code-philosophy) --------------------
[[ -n "${KEY_NAME}" ]] || die "--key-name is required"
if [[ ! "${CONSOLE_TAIL_SECONDS}" =~ ^[0-9]+$ ]] || (( CONSOLE_TAIL_SECONDS <= 0 )); then
    die "--console-tail-seconds must be a positive integer (got '${CONSOLE_TAIL_SECONDS}')"
fi

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
# Ensure the bundle staging dir exists before mktemp creates its temp dir inside
# it (cloudformation/out is gitignored and absent on a fresh checkout).
mkdir -p "${BUNDLE_DIR}"
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
    if [[ "${DEBUG}" == "1" ]]; then
        aws cloudformation update-stack \
            --stack-name "${STACK_NAME}" \
            --template-body "file://${SCRIPT_DIR}/stack.yaml" \
            --parameters "${PARAMS[@]}" \
            --capabilities CAPABILITY_IAM \
            --region "${REGION}" --debug
    else
        aws cloudformation update-stack \
            --stack-name "${STACK_NAME}" \
            --template-body "file://${SCRIPT_DIR}/stack.yaml" \
            --parameters "${PARAMS[@]}" \
            --capabilities CAPABILITY_IAM \
            --region "${REGION}" >/dev/null
    fi
    aws cloudformation wait stack-update-complete --stack-name "${STACK_NAME}" --region "${REGION}" $(cfn_debug)
    echo "gitd: deploy: stack update complete"
    if [[ "${DEBUG}" == "1" ]]; then
        echo "gitd: deploy: (DEBUG) if the waiter reports a failure, run: aws cloudformation describe-stack-events --stack-name ${STACK_NAME} --region ${REGION} --query 'StackEvents[?ResourceStatus==\`UPDATE_FAILED\`].{Res:LogicalResourceId,Reason:ResourceStatusReason}'"
    fi
else
    echo "gitd: deploy: creating stack '${STACK_NAME}'"
    if [[ "${DEBUG}" == "1" ]]; then
        aws cloudformation create-stack \
            --stack-name "${STACK_NAME}" \
            --template-body "file://${SCRIPT_DIR}/stack.yaml" \
            --parameters "${PARAMS[@]}" \
            --capabilities CAPABILITY_IAM \
            --region "${REGION}" --on-failure DO_NOTHING --debug
    else
        aws cloudformation create-stack \
            --stack-name "${STACK_NAME}" \
            --template-body "file://${SCRIPT_DIR}/stack.yaml" \
            --parameters "${PARAMS[@]}" \
            --capabilities CAPABILITY_IAM \
            --region "${REGION}" >/dev/null
    fi
    # create-stack completes only after the instance's boot signal (CreationPolicy).
    aws cloudformation wait stack-create-complete --stack-name "${STACK_NAME}" --region "${REGION}" $(cfn_debug)
    echo "gitd: deploy: stack create complete (boot signal received)"
    if [[ "${DEBUG}" == "1" ]]; then
        echo "gitd: deploy: (DEBUG) if the waiter reports a failure, run: aws cloudformation describe-stack-events --stack-name ${STACK_NAME} --region ${REGION} --query 'StackEvents[?ResourceStatus==\`CREATE_FAILED\`].{Res:LogicalResourceId,Reason:ResourceStatusReason}'"
    fi
fi

# --- post-deploy: tail the instance system log for boot diagnostics -----------------
if [[ "${CONSOLE_TAIL}" == "1" ]]; then
    INSTANCE_ID="$(resolve_instance_id)"
    if [[ -n "${INSTANCE_ID}" ]]; then
        tail_console_output "${INSTANCE_ID}" "${CONSOLE_TAIL_SECONDS}"
    else
        echo "gitd: deploy: no running instance found for stack '${STACK_NAME}'; skipping console tail"
    fi
fi

echo "gitd: deploy: done"
