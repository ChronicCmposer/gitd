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

PRODUCT="containerd"
OUT_DIR="${1:-${DIST_DIR}/out}"

require_cmd curl
require_cmd tar
require_root

# 1. Fetch + verify the pinned source tarball.
WORK="$(mktemp -d "${DIST_DIR}/.work-containerd.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
source_tar="${WORK}/containerd-${CONTAINERD_VERSION}.tar.gz"
download "${CONTAINERD_URL}" "${source_tar}" "${CONTAINERD_SHA256}"

# 2. Prepare the AL2023 chroot with Go + make.
rootfs="${DIST_DIR}/.cache/al2023-${HOST_ARCH}"
al2023_setup_rootfs "${rootfs}"
require_cmd dnf
# Inside the chroot dnf needs a resolv.conf (copied by al2023_setup_rootfs).
chroot "${rootfs}" /usr/bin/dnf install -y golang make gcc \
    || die "dnf install failed inside ${rootfs}"

# 3. Stage the source inside the chroot.
mkdir -p "${WORK}/src"
tar -xzf "${source_tar}" -C "${WORK}/src"
cp -a "${WORK}/src" "${rootfs}/src"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
export PATH="/usr/local/go/bin:\$PATH"
cd /src/containerd-${CONTAINERD_VERSION}
# Static, CGO-free, no seccomp/apparmor/selinux tags (R2-Q19).
CGO_ENABLED=0 BUILDTAGS="" GOFLAGS="-mod=vendor" make -j"\$(nproc)" binaries
cp bin/containerd bin/ctr bin/containerd-shim-runc-v2 /out/
EOF

chroot_run "${rootfs}" "${WORK}/build.sh" || die "containerd build failed"

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
file "${stage}/usr/local/bin/containerd" | grep -q "statically linked" \
    || die "containerd is not statically linked"

asset="${OUT_DIR}/$(product_asset_name containerd "${CONTAINERD_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: containerd: ${asset}"
echo "gitd: containerd: sha256 $(sha256_of "${asset}")"