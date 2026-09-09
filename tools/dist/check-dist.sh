#!/usr/bin/env bash
# tools/dist/check-dist.sh — build-twice determinism check for one product.
#
# Usage:
#   tools/dist/check-dist.sh <product>    # openssh|git|fish|sudo|ca-certs|containerd|runc|pinned-go
#
# The product build runs twice into two separate output dirs; the resulting
# tarballs must be byte-identical (same sha256) or the check fails loudly.
# This is the gate before any publish (make check-*-dist), per R3-Q2/2.3.

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

PRODUCT="check-dist"

[[ $# -eq 1 ]] || die "usage: check-dist.sh <product>"
product="$1"

# Map product -> build script + asset name.
case "${product}" in
    openssh)    build_script="build-openssh.sh"    ; version="${OPENSSH_VERSION}" ;;
    git)        build_script="build-git.sh"        ; version="${GIT_VERSION}" ;;
    fish)       build_script="build-fish.sh"       ; version="${FISH_VERSION}" ;;
    sudo)       build_script="build-sudo.sh"       ; version="${SUDO_VERSION}" ;;
    ca-certs)   build_script="build-ca-certs.sh"   ; version="${CA_CERTS_VERSION}" ;;
    containerd) build_script="build-containerd.sh" ; version="${CONTAINERD_VERSION}" ;;
    runc)       build_script="build-runc.sh"       ; version="${RUNC_VERSION}" ;;
    pinned-go)  build_script="build-pinned-go.sh"  ; version="${GO_VERSION}" ;;
    *) die "unknown product '${product}'; expected openssh|git|fish|sudo|ca-certs|containerd|runc|pinned-go" ;;
esac

script="${DIST_DIR}/${build_script}"
[[ -x "${script}" ]] || die "build script not executable: ${script}"

echo "gitd: check-dist: ${product}: build #1"
out1="$(mktemp -d "${DIST_DIR}/.check-${product}-1.XXXXXX")"
"${script}" "${out1}"

echo "gitd: check-dist: ${product}: build #2"
out2="$(mktemp -d "${DIST_DIR}/.check-${product}-2.XXXXXX")"
"${script}" "${out2}"

# Compare every produced artifact (a product may emit more than one file).
fail=""
for a in "${out1}"/*; do
    name="$(basename "${a}")"
    b="${out2}/${name}"
    [[ -f "${b}" ]] || { echo "gitd: check-dist: ${product}: missing ${name} in build #2" >&2; fail=1; continue; }
    sha1="$(sha256_of "${a}")"
    sha2="$(sha256_of "${b}")"
    if [[ "${sha1}" != "${sha2}" ]]; then
        echo "gitd: check-dist: ${product}: NON-DETERMINISTIC ${name}" >&2
        echo "  build #1: ${sha1}" >&2
        echo "  build #2: ${sha2}" >&2
        fail=1
    else
        echo "gitd: check-dist: ${product}: deterministic ${name} (${sha1})"
    fi
done

rm -rf "${out1}" "${out2}"
[[ -z "${fail}" ]] || die "determinism check failed for ${product}"