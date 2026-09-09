#!/usr/bin/env bash
# tools/dist/gen-dist-pins.sh — regenerate dist_pins.bzl from built tarballs.
#
# Usage:
#   tools/dist/gen-dist-pins.sh [DIST_DIR]   # default tools/dist/out
#
# Reads the deterministic tarballs produced by the build-*.sh scripts and
# rewrites the pinned sha256 values in dist_pins.bzl (the file dist.bzl loads).
# Pins that have no built tarball yet are left untouched so a partial build
# only updates the products that were actually built. Run after
# `make check-*-dist`; commit the regenerated dist_pins.bzl.

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

PRODUCT="gen-dist-pins"
OUT_DIR="${1:-${DIST_DIR}/out}"
PINS_FILE="$(git -C "${DIST_DIR}/../.." rev-parse --show-toplevel 2>/dev/null)/dist_pins.bzl"

[[ -f "${PINS_FILE}" ]] || die "dist_pins.bzl not found at ${PINS_FILE}"

# product <-> asset-name mapping, mirroring check-dist.sh.
product_asset() {
    local product="$1"
    case "${product}" in
        openssh)    echo "openssh-${OPENSSH_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        git)        echo "git-${GIT_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        fish)       echo "fish-${FISH_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        sudo)       echo "sudo-${SUDO_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        ca-certs)   echo "ca-certificates-${CA_CERTS_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        containerd) echo "containerd-${CONTAINERD_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        runc)       echo "runc-${RUNC_VERSION}.linux-${HOST_ARCH}.tar.gz" ;;
        *) die "unknown product '${product}'" ;;
    esac
}

# For each product with a built tarball, compute the sha256 and update the
# PIN_<PRODUCT>_<ARCH> line in dist_pins.bzl. The variable name is fully
# upper-cased with '-' -> '_' (PIN_CA_CERTS_ARM64, PIN_OPENSSH_AMD64, ...).
for product in openssh git fish sudo ca-certs containerd runc; do
    asset="${OUT_DIR}/$(product_asset "${product}")"
    [[ -f "${asset}" ]] || { echo "gitd: gen-dist-pins: ${product}: no tarball at ${asset}; pin unchanged"; continue; }
    sha="$(sha256_of "${asset}")"
    var="PIN_$(echo "${product}" | tr '[:lower:]' '[:upper:]' | tr '-' '_')_$(echo "${HOST_ARCH}" | tr '[:lower:]' '[:upper:]')"
    if grep -q "^${var}[[:space:]]*=" "${PINS_FILE}"; then
        sed -i "s/^${var}[[:space:]]*=.*/${var} = \"${sha}\"/" "${PINS_FILE}"
        echo "gitd: gen-dist-pins: ${product}: ${var}=${sha}"
    else
        echo "gitd: gen-dist-pins: ${product}: variable ${var} not found in ${PINS_FILE}; add it manually" >&2
    fi
done