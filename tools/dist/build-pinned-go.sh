#!/usr/bin/env bash
# tools/dist/build-pinned-go.sh — obtain the pinned Go toolchain (2.3).
#
# Produces:  tools/dist/out/go1.26.5.linux-<arch>.tar.gz (+ SHA256SUMS line)
#
# The toolchain MUST stay aligned with MODULE.bazel's go_sdk.download
# (rules_go 0.63.0 supports 1.26.5). The tarball is fetched from go.dev and
# verified against the pinned sha256 from versions.sh; the build integration
# is the go_sdk repo already declared in MODULE.bazel — this script exists so
# CI and local tooling use the same pinned SDK outside Bazel (go vet, coverage,
# fuzzing) without drifting to a newer Go.
#
# Usage:
#   tools/dist/build-pinned-go.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

PRODUCT="pinned-go"
OUT_DIR="${1:-${DIST_DIR}/out}"

require_cmd curl

# Select the sha256 for this build arch (fail fast on an unknown arch).
case "${HOST_ARCH}" in
    amd64) GO_SHA256="${GO_AMD64_SHA256}" ;;
    arm64) GO_SHA256="${GO_ARM64_SHA256}" ;;
    *) die "no Go toolchain sha256 for arch ${HOST_ARCH}" ;;
esac

url="https://go.dev/dl/go${GO_VERSION}.linux-${HOST_ARCH}.tar.gz"
mkdir -p "${OUT_DIR}"
asset="${OUT_DIR}/go${GO_VERSION}.linux-${HOST_ARCH}.tar.gz"

if [[ -f "${asset}" ]]; then
    # Already staged; verify the pin before reporting success.
    actual="$(sha256_of "${asset}")"
    [[ "${actual}" == "${GO_SHA256}" ]] \
        || die "staged Go toolchain sha256 mismatch: expected ${GO_SHA256}, got ${actual}"
else
    download "${url}" "${asset}" "${GO_SHA256}"
fi

# Sanity: the staged SDK must extract and run.
sdk_dir="${OUT_DIR}/go${GO_VERSION}.linux-${HOST_ARCH}"
[[ -x "${sdk_dir}/bin/go" ]] || tar -xzf "${asset}" -C "${OUT_DIR}"
"${sdk_dir}/bin/go" version

echo "gitd: pinned-go: ${asset}"
echo "gitd: pinned-go: sha256 ${GO_SHA256}"