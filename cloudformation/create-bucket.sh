#!/usr/bin/env bash
# cloudformation/create-bucket.sh — provision the git.cmposer.cc S3 bucket
# OUT OF BAND, before the CloudFormation stack exists.
#
# WHY THIS SCRIPT EXISTS (deploy-before-stack ordering):
#   cloudformation/deploy.sh uploads the deployment bundle to
#   s3://git.cmposer.cc/... (line 173) BEFORE create-stack, but the bucket is
#   normally created by the stack's GitdBucket resource. On the first deploy
#   the stack does not exist yet, so that upload 404s with NoSuchBucket. This
#   script provisions the bucket ONCE, with EXACTLY the spec the stack's
#   GitdBucket resource declares, so create-stack succeeds: the resource adopts
#   the pre-provisioned bucket instead of failing on a name collision.
#
# NEVER DELETE:
#   The stack declares DeletionPolicy: Retain / UpdateReplacePolicy: Retain on
#   GitdBucket (cloudformation/stack.yaml, lines 168-194) — the bucket holds
#   repos, bundles, and image fallbacks and is intentionally NOT destroyed with
#   the stack. This script has NO delete path, by design.
#
# The spec (matches GitdBucket in cloudformation/stack.yaml):
#   - BucketName:            git.cmposer.cc (a global/partition-unique name)
#   - Versioning:            Enabled
#   - PublicAccessBlock:     BlockPublicAcls / IgnorePublicAcls /
#                            BlockPublicPolicy / RestrictPublicBuckets = true
#   - Encryption:            SSE-S3 (SSEAlgorithm: AES256)
#   - Lifecycle:             NoncurrentExpire30d (NoncurrentDays: 30)
#                            AbortIncompleteMultipartUpload7d
#                            (DaysAfterInitiation: 7)
#
# Idempotent: if the bucket already exists, each required config piece is
# verified against the spec and only the missing pieces are (re)applied. A
# fully-correct bucket prints "already provisioned; no changes" and exits 0.
# Any unexpected AWS error fails loudly — a half-provisioned bucket is never
# silently accepted (code-philosophy: fail fast, fail loud).
#
# Usage:
#   cloudformation/create-bucket.sh
# Environment overrides:
#   REGION   AWS region (default us-east-2)
#   BUCKET   S3 bucket name (default git.cmposer.cc)
#
# Requires: aws CLI with credentials that can create/configure the bucket.

set -euo pipefail

die() {
    echo "gitd: create-bucket: $*" >&2
    exit 1
}
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}

REGION="${REGION:-us-east-2}"
BUCKET="${BUCKET:-git.cmposer.cc}"

require_cmd aws

# --- helpers -------------------------------------------------------------------

# aws_get runs a read-only `aws s3api <sub>` getter against BUCKET and prints
# the JSON response on success. A documented "not configured" 404 (the state
# that means "this piece must be applied") prints nothing and returns 1; any
# other error dies loudly — we never mask a real failure.
aws_get() {
    local sub="$1" rc=0 out=""
    out="$(aws s3api "${sub}" --bucket "${BUCKET}" --region "${REGION}" 2>&1)" || rc=$?
    if [[ ${rc} -eq 0 ]]; then
        printf '%s\n' "${out}"
        return 0
    fi
    case "${out}" in
        *NoSuchPublicAccessBlockConfiguration*|*ServerSideEncryptionConfigurationNotFoundError*|*NoSuchLifecycleConfiguration*)
            return 1
            ;;
        *)
            die "aws s3api ${sub} failed on '${BUCKET}': ${out}"
            ;;
    esac
}

# Each *_satisfied predicate returns 0 when the required config is present on
# the bucket and 1 when it is missing (so the caller applies it). The
# predicates are only ever called from `if`/`||` contexts, where `set -e` is
# suppressed for the whole function body; a missing piece simply yields
# "not satisfied" and a real error still dies loudly via aws_get.
versioning_satisfied() {
    [[ "$(aws_get get-bucket-versioning)" == *'"Status": "Enabled"'* ]]
}
public_access_block_satisfied() {
    local cfg
    cfg="$(aws_get get-public-access-block)"
    [[ "${cfg}" == *'"BlockPublicAcls": true'* \
      && "${cfg}" == *'"IgnorePublicAcls": true'* \
      && "${cfg}" == *'"BlockPublicPolicy": true'* \
      && "${cfg}" == *'"RestrictPublicBuckets": true'* ]]
}
encryption_satisfied() {
    [[ "$(aws_get get-bucket-encryption)" == *'"SSEAlgorithm": "AES256"'* ]]
}
lifecycle_satisfied() {
    local cfg
    cfg="$(aws_get get-bucket-lifecycle-configuration)"
    [[ "${cfg}" == *'"ID": "NoncurrentExpire30d"'* \
      && "${cfg}" == *'"NoncurrentDays": 30'* \
      && "${cfg}" == *'"ID": "AbortIncompleteMultipartUpload7d"'* \
      && "${cfg}" == *'"DaysAfterInitiation": 7'* ]]
}

# read_property prints one JMESPath --query value from S3, or dies loudly.
read_property() {
    local sub="$1" query="$2"
    aws s3api "${sub}" --bucket "${BUCKET}" --region "${REGION}" \
        --query "${query}" --output text \
        || die "failed to read ${sub} on s3://${BUCKET}"
}

# create_bucket creates BUCKET with the full spec in one call. Dotted bucket
# names are globally/partition unique; regions other than us-east-1 require an
# explicit LocationConstraint (aws s3api create-bucket rules).
create_bucket() {
    local -a args=(--bucket "${BUCKET}" --region "${REGION}")
    if [[ "${REGION}" != "us-east-1" ]]; then
        args+=(--create-bucket-configuration "LocationConstraint=${REGION}")
    fi
    aws s3api create-bucket "${args[@]}" --output text >/dev/null \
        || die "failed to create bucket '${BUCKET}' in ${REGION}: S3 bucket names are globally unique, so it may already be owned by another account"
    echo "gitd: create-bucket: created s3://${BUCKET}"
}

# verify_and_report re-reads every property from S3 and fails loudly if
# anything did not converge (never exit 0 on a half-provisioned bucket), then
# prints the live state straight from S3.
verify_and_report() {
    versioning_satisfied || die "versioning is not Enabled on s3://${BUCKET} after provisioning"
    public_access_block_satisfied || die "public access block is not fully enabled on s3://${BUCKET} after provisioning"
    encryption_satisfied || die "encryption is not SSE-S3 (AES256) on s3://${BUCKET} after provisioning"
    lifecycle_satisfied || die "lifecycle rules are missing on s3://${BUCKET} after provisioning"

    local versioning pab encryption lifecycle
    versioning="$(read_property get-bucket-versioning 'Status')"
    pab="$(read_property get-public-access-block 'PublicAccessBlockConfiguration.*')"
    encryption="$(read_property get-bucket-encryption 'ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm')"
    lifecycle="$(read_property get-bucket-lifecycle-configuration 'Rules[].ID')"
    echo "gitd: create-bucket: verified s3://${BUCKET} (${REGION}):"
    echo "  versioning:          ${versioning}"
    echo "  public access block: ${pab}"
    echo "  encryption:          ${encryption}"
    echo "  lifecycle rules:     ${lifecycle}"
    echo "  deletion policy:     never (GitdBucket has DeletionPolicy: Retain)"
}

# --- flow -----------------------------------------------------------------------

echo "gitd: create-bucket: provisioning s3://${BUCKET} in ${REGION}"

# Does the bucket exist? head-bucket exit 0 = exists; 404 = does not exist;
# 403 = exists but owned by another account (S3 names are globally unique —
# fail fast, never create over someone else's bucket).
BUCKET_EXISTS=false
if head_out="$(aws s3api head-bucket --bucket "${BUCKET}" --region "${REGION}" 2>&1)"; then
    BUCKET_EXISTS=true
    echo "gitd: create-bucket: s3://${BUCKET} already exists; verifying required config"
elif [[ "${head_out}" == *"(404)"* || "${head_out}" == *"NoSuchBucket"* ]]; then
    echo "gitd: create-bucket: s3://${BUCKET} does not exist"
elif [[ "${head_out}" == *"(403)"* || "${head_out}" == *"Forbidden"* || "${head_out}" == *"AccessDenied"* ]]; then
    die "s3://${BUCKET} exists but is not accessible from this account (403): S3 bucket names are globally unique, so another account may own it"
else
    die "cannot check s3://${BUCKET}: ${head_out}"
fi

# Create only when missing.
if [[ "${BUCKET_EXISTS}" == false ]]; then
    create_bucket
fi

# Idempotently apply whatever config pieces are missing (matches GitdBucket
# spec exactly). Each put-* replaces the whole property, so this also heals
# drift on an existing bucket.
CHANGES_APPLIED=false

if ! versioning_satisfied; then
    aws s3api put-bucket-versioning \
        --bucket "${BUCKET}" \
        --region "${REGION}" \
        --versioning-configuration Status=Enabled \
        || die "failed to enable versioning on s3://${BUCKET}"
    echo "gitd: create-bucket: enabled versioning on s3://${BUCKET}"
    CHANGES_APPLIED=true
fi

if ! public_access_block_satisfied; then
    aws s3api put-public-access-block \
        --bucket "${BUCKET}" \
        --region "${REGION}" \
        --public-access-block-configuration BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true \
        || die "failed to set the public access block on s3://${BUCKET}"
    echo "gitd: create-bucket: enabled public access block on s3://${BUCKET}"
    CHANGES_APPLIED=true
fi

if ! encryption_satisfied; then
    aws s3api put-bucket-encryption \
        --bucket "${BUCKET}" \
        --region "${REGION}" \
        --server-side-encryption-configuration '{
            "Rules": [{
                "ApplyServerSideEncryptionByDefault": { "SSEAlgorithm": "AES256" }
            }]
        }' \
        || die "failed to set SSE-S3 (AES256) encryption on s3://${BUCKET}"
    echo "gitd: create-bucket: enabled SSE-S3 (AES256) encryption on s3://${BUCKET}"
    CHANGES_APPLIED=true
fi

if ! lifecycle_satisfied; then
    aws s3api put-bucket-lifecycle-configuration \
        --bucket "${BUCKET}" \
        --region "${REGION}" \
        --lifecycle-configuration '{
            "Rules": [
                {
                    "ID": "NoncurrentExpire30d",
                    "Status": "Enabled",
                    "NoncurrentVersionExpiration": { "NoncurrentDays": 30 }
                },
                {
                    "ID": "AbortIncompleteMultipartUpload7d",
                    "Status": "Enabled",
                    "AbortIncompleteMultipartUpload": { "DaysAfterInitiation": 7 }
                }
            ]
        }' \
        || die "failed to set the lifecycle configuration on s3://${BUCKET}"
    echo "gitd: create-bucket: set lifecycle configuration on s3://${BUCKET}"
    CHANGES_APPLIED=true
fi

# Prove the final state and report it.
verify_and_report

if [[ "${BUCKET_EXISTS}" == false ]]; then
    echo "gitd: create-bucket: bucket '${BUCKET}' created and provisioned"
elif [[ "${CHANGES_APPLIED}" == true ]]; then
    echo "gitd: create-bucket: bucket '${BUCKET}' existed; missing config applied"
else
    echo "gitd: create-bucket: bucket '${BUCKET}' already provisioned; no changes"
fi