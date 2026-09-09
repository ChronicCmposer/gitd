#!/usr/bin/env bash
# tools/dist/package-image.sh — package the rules_oci image for ctr import.
#
# Usage:
#   bazel build //image:image
#   tools/dist/package-image.sh [OUT_FILE]   # default tools/dist/out/gitd-container.tar
#
# containerd's `ctr images import` accepts an OCI-layout tar (the directory
# produced by //image:image contains oci-layout, index.json, blobs/). This
# script tars that directory deterministically (sorted names, pinned mtimes,
# numeric owners) so the publish pipeline and Phase 7 userdata consume one
# stable artifact: gitd-container.tar (R3-Q2).

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

PRODUCT="image"
OUT_FILE="${1:-${DIST_DIR}/out/gitd-container.tar}"

# Locate the built image directory from the Bazel output base.
image_dir="$(bazel info bazel-bin 2>/dev/null)/image/image"
[[ -f "${image_dir}/oci-layout" ]] \
    || die "OCI layout not found at ${image_dir}; run: bazel build //image:image"

mkdir -p "$(dirname "${OUT_FILE}")"
tar \
    --sort=name \
    --mtime="@${SOURCE_DATE_EPOCH}" \
    --owner=0 --group=0 --numeric-owner \
    --format=gnu \
    -C "${image_dir}" \
    -cf "${OUT_FILE}" \
    .

echo "gitd: image: ${OUT_FILE}"
echo "gitd: image: sha256 $(sha256_of "${OUT_FILE}")"