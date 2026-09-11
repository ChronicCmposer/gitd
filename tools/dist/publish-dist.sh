#!/usr/bin/env bash
# tools/dist/publish-dist.sh — publish a determinism-checked artifact.
#
# Usage:
#   tools/dist/publish-dist.sh <product>    # openssh|git|fish|ca-certs|containerd|runc
#
# Publishes the product tarball from tools/dist/out to:
#   - GitHub Releases:      ChronicCmposer/gitd family release tag
#                           (openssh-dist, git-dist, fish-dist,
#                           ca-certs-dist, containerd-dist, runc-dist —
#                           strimserver family-tag strategy on the PRIMARY repo)
#   - S3 fallback:          s3://git.cmposer.cc/<prefix>/<asset>
# GitHub Releases is the primary artifact source for Bazel (R3-Q2); S3 is the
# fallback mirror. Requires: the determinism check passed and an authenticated
# gh CLI (gh auth login). The S3 upload is best-effort: the bucket is created by
# the CloudFormation stack at deploy time, so a missing bucket must NOT block
# the GitHub publish (a failed S3 upload is a loud warning, not fatal).
#
# S3 prefixes (2.4): openssh/ git/ fish/ containerd/ (runc rides containerd/).

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/release/sign-artifact.sh
source "${DIST_DIR}/../release/sign-artifact.sh"
# shellcheck source=tools/release/gh-auth.sh
source "${DIST_DIR}/../release/gh-auth.sh"

PRODUCT="publish-dist"

[[ $# -eq 1 ]] || die "usage: publish-dist.sh <product>"

product="$1"
# asset_name mirrors the build scripts' product_asset_name output: the pipeline
# product key is "ca-certs" but the artifact is named "ca-certificates-...".
# family is the stable strimserver-style release tag on the primary repo.
case "${product}" in
    openssh)    version="${OPENSSH_VERSION}"    ; prefix="openssh"     ; asset_name="openssh"        ; family="openssh-dist" ;;
    git)        version="${GIT_VERSION}"        ; prefix="git"         ; asset_name="git"            ; family="git-dist" ;;
    fish)       version="${FISH_VERSION}"        ; prefix="fish"       ; asset_name="fish"           ; family="fish-dist" ;;
    ca-certs)   version="${CA_CERTS_VERSION}"    ; prefix="ca-certs"   ; asset_name="ca-certificates"; family="ca-certs-dist" ;;
    containerd) version="${CONTAINERD_VERSION}"  ; prefix="containerd" ; asset_name="containerd"     ; family="containerd-dist" ;;
    runc)       version="${RUNC_VERSION}"        ; prefix="containerd" ; asset_name="runc"           ; family="runc-dist" ;;
    *) die "unknown product '${product}'" ;;
esac

asset="$(product_asset_name "${asset_name}" "${version}")"
src="${DIST_DIR}/out/${asset}"
[[ -f "${src}" ]] || die "artifact not found: ${src} (run make check-${product}-dist first)"

# GPG-sign the artifact BEFORE publishing: the consumer (Bazel fetch / boot /
# update) expects a detached .asc next to every product tarball (provenance on
# top of the pinned sha256). A sign failure blocks publish — no unsigned dist
# product is ever uploaded.
sign_artifact "${src}"

# GitHub family-tag release + asset upload (tar + .asc together) on the PRIMARY
# repo (DIST_REPO=ChronicCmposer/gitd). Create the family release once with a
# descriptive title; re-runs clobber-upload the assets so a pin bump never
# needs a new tag (strimserver pattern). gh reads its own stored credentials
# (GH_TOKEN, if set, is used by gh as an override).
require_gh_auth
if gh release view "${family}" --repo "${DIST_REPO}" >/dev/null 2>&1; then
    gh release upload "${family}" "${src}" "${src}.asc" --repo "${DIST_REPO}" --clobber
else
    gh release create "${family}" "${src}" "${src}.asc" --repo "${DIST_REPO}" \
        --title "${family}" \
        --notes "Deterministic build artifact (gitd dist pipeline, family tag). sha256: $(sha256_of "${src}"). GPG-signed (gitd-signing-key.asc)."
fi
echo "gitd: publish-dist: ${product}: GitHub release ${family} on ${DIST_REPO}"

# S3 fallback mirror (tar + .asc together). Best-effort: the bucket is created
# by the CloudFormation stack at deploy time, so a missing bucket (or aws CLI)
# must not block the GitHub publish — a failed S3 upload is a loud warning.
if ! command -v aws >/dev/null 2>&1; then
    echo "gitd: publish-dist: ${product}: WARNING: aws CLI not found; skipped S3 fallback upload (GitHub release ${family} is the working primary)" >&2
elif ! { aws s3 cp "${src}" "s3://${S3_BUCKET}/${prefix}/${asset}" \
            --region "${S3_REGION}" --only-show-errors \
        && aws s3 cp "${src}.asc" "s3://${S3_BUCKET}/${prefix}/${asset}.asc" \
            --region "${S3_REGION}" --only-show-errors; }; then
    echo "gitd: publish-dist: ${product}: WARNING: S3 fallback upload failed (bucket ${S3_BUCKET} may not exist until deploy); GitHub release ${family} is the working primary" >&2
else
    echo "gitd: publish-dist: ${product}: s3://${S3_BUCKET}/${prefix}/${asset} (+ .asc)"
fi
