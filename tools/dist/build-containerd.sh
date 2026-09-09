#!/usr/bin/env bash
# tools/dist/build-containerd.sh — build containerd as a HOST binary (R2-Q19).
#
# Produces:  tools/dist/out/containerd-2.3.5.linux-<arch>.tar.gz
#            (usr/local/bin/containerd, ctr, containerd-shim-runc-v2)
#
# Built in the AL2023 (glibc) chroot with CGO_ENABLED=0 static Go and no
# seccomp/apparmor/selinux build tags: the host kernel's seccomp is handled by
# runc at runtime, and containerd's plugin tags only add CGO surface we do not
# want in a static host binary. The image never contains containerd; it runs on
# the host via systemd (tools/dist/containerd/containerd.service).
#
# Usage:
#   tools/dist/build-containerd.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/dist/chroot.sh
source "${DIST_DIR}/chroot.sh"

export PRODUCT="containerd"
OUT_DIR="${1:-${DIST_DIR}/out}"
PRODUCT_DNF_DEPS="golang make gcc"

require_cmd curl
require_cmd tar
require_chroot_priv

# 1. Fetch + verify the pinned source tarball.
WORK="$(mktemp -d "${DIST_DIR}/.work-containerd.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
source_tar="${WORK}/containerd-${CONTAINERD_VERSION}.tar.gz"
download "${CONTAINERD_URL}" "${source_tar}" "${CONTAINERD_SHA256}"

# 2. Prepare the AL2023 chroot with Go + make. dnf runs inside the AL2023
#    chroot (never on the host); the install fails loudly if it is missing
#    from the rootfs.
rootfs="$(ensure_rootfs al2023)"
install_deps "${rootfs}" al2023 "${PRODUCT_DNF_DEPS}"

# 3. Stage the source inside the chroot.
mkdir -p "${WORK}/src"
tar -xzf "${source_tar}" -C "${WORK}/src"
# The cached rootfs may already hold a previous run's staging; wipe it so the
# build always compiles from the pristine pinned source (a reused rootfs must
# never see stale source/objects, and cp -a of an existing dir would nest).
rm -rf "${rootfs}/src" "${rootfs}/out"
cp -a "${WORK}/src" "${rootfs}/src"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
export PATH="/usr/local/go/bin:\$PATH"
cd /src/containerd-${CONTAINERD_VERSION}
# Static, CGO-free, no seccomp/apparmor/selinux tags (R2-Q19). STATIC=1 makes
# containerd's Makefile add the osusergo/netgo/static_build tags and
# -extldflags "-static"; CGO_ENABLED=0 must be EXPORTED (a bare
# "CGO_ENABLED=0 make" assignment is not seen by make's recipe shells).
export CGO_ENABLED=0
export GOFLAGS="-mod=vendor"
make -j"\$(nproc)" STATIC=1 BUILDTAGS="" binaries
cp bin/containerd bin/ctr bin/containerd-shim-runc-v2 /out/
EOF

run_in_rootfs "${rootfs}" "${WORK}/build.sh" || die "containerd build failed"

# 4. Copy the static binaries out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-containerd.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
mkdir -p "${stage}/usr/local/bin"
for bin in containerd ctr containerd-shim-runc-v2; do
    [[ -f "${rootfs}/out/${bin}" ]] || die "containerd produced no ${bin}"
    cp -a "${rootfs}/out/${bin}" "${stage}/usr/local/bin/${bin}"
done

# 5. Verify static linkage, then assemble the deterministic tarball.
# Go's -buildmode=pie + internal linking yields a static-PIE; accept both
# "statically linked" and "static-pie linked" (both have no interpreter).
file "${stage}/usr/local/bin/containerd" | grep -qE "statically linked|static-pie linked" \
    || die "containerd is not statically linked"

asset="${OUT_DIR}/$(product_asset_name containerd "${CONTAINERD_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: containerd: ${asset}"
echo "gitd: containerd: sha256 $(sha256_of "${asset}")"