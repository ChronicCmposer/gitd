#!/usr/bin/env bash
# tools/dist/publish-dist.sh — publish a determinism-checked artifact.
#
# Usage:
#   tools/dist/publish-dist.sh <product>    # openssh|git|fish|sudo|ca-certs|containerd|runc
#
# Publishes the product tarball from tools/dist/out to:
#   - S3 primary fallback:  s3://git.cmposer.cc/<prefix>/<asset>
#   - GitHub Releases:      ChronicCmposer/gitd-dist release tag <product>-<version>
# GitHub Releases is the primary artifact source for Bazel (R3-Q2); S3 is the
# fallback mirror. Requires: the determinism check passed, aws CLI credentials
# for the bucket, and an authenticated gh CLI (gh auth login).
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
case "${product}" in
    openssh)    version="${OPENSSH_VERSION}"    ; prefix="openssh"     ; asset_name="openssh" ;;
    git)        version="${GIT_VERSION}"        ; prefix="git"         ; asset_name="git" ;;
    fish)       version="${FISH_VERSION}"        ; prefix="fish"       ; asset_name="fish" ;;
    sudo)       version="${SUDO_VERSION}"        ; prefix="sudo"       ; asset_name="sudo" ;;
    ca-certs)   version="${CA_CERTS_VERSION}"    ; prefix="ca-certs"   ; asset_name="ca-certificates" ;;
    containerd) version="${CONTAINERD_VERSION}"  ; prefix="containerd" ; asset_name="containerd" ;;
    runc)       version="${RUNC_VERSION}"        ; prefix="containerd" ; asset_name="runc" ;;
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

# GitHub tag + asset upload (tar + .asc together). gh reads its own stored
# credentials (GH_TOKEN, if set, is used by gh as an override).
require_gh_auth
tag="${product}-${version}"
if gh release view "${tag}" --repo "${DIST_REPO}" >/dev/null 2>&1; then
    gh release upload "${tag}" "${src}" "${src}.asc" --repo "${DIST_REPO}" --clobber
else
    gh release create "${tag}" "${src}" "${src}.asc" --repo "${DIST_REPO}" \
        --title "${product} ${version}" \
        --notes "Deterministic build artifact (gitd dist pipeline). sha256: $(sha256_of "${src}"). GPG-signed (gitd-signing-key.asc)."
fi
echo "gitd: publish-dist: ${product}: GitHub release ${tag}"

# S3 fallback mirror (tar + .asc together).
require_cmd aws
aws s3 cp "${src}" "s3://${S3_BUCKET}/${prefix}/${asset}" \
    --region "${S3_REGION}" --only-show-errors
aws s3 cp "${src}.asc" "s3://${S3_BUCKET}/${prefix}/${asset}.asc" \
    --region "${S3_REGION}" --only-show-errors
echo "gitd: publish-dist: ${product}: s3://${S3_BUCKET}/${prefix}/${asset}"