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
# for the bucket, and a GitHub token in GH_TOKEN.
#
# S3 prefixes (2.4): openssh/ git/ fish/ containerd/ (runc rides containerd/).

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

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

# GitHub tag + asset upload.
require_cmd gh
[[ -n "${GH_TOKEN:-}" ]] || die "GH_TOKEN must be set to publish a GitHub release"
tag="${product}-${version}"
if gh release view "${tag}" --repo "${DIST_REPO}" >/dev/null 2>&1; then
    gh release upload "${tag}" "${src}" --repo "${DIST_REPO}" --clobber
else
    gh release create "${tag}" "${src}" --repo "${DIST_REPO}" \
        --title "${product} ${version}" \
        --notes "Deterministic build artifact (gitd dist pipeline). sha256: $(sha256_of "${src}")"
fi
echo "gitd: publish-dist: ${product}: GitHub release ${tag}"

# S3 fallback mirror.
require_cmd aws
aws s3 cp "${src}" "s3://${S3_BUCKET}/${prefix}/${asset}" \
    --region "${S3_REGION}" --only-show-errors
echo "gitd: publish-dist: ${product}: s3://${S3_BUCKET}/${prefix}/${asset}"