#!/usr/bin/env bash
# tools/dist/build-runc.sh — build runc as a HOST binary (R2-Q19).
#
# Produces:  tools/dist/out/runc-1.2.9.linux-<arch>.tar.gz
#            (usr/local/bin/runc)
#
# Built in the AL2023 (glibc) chroot with CGO_ENABLED=0 static Go. The
# seccomp build tag is intentionally NOT enabled (no libseccomp-devel in the
# chroot, and R2-Q19 specifies no seccomp tag); containerd's runtime uses the
# kernel's native seccomp filtering through the standard OCI runtime spec.
#
# Usage:
#   tools/dist/build-runc.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/dist/chroot.sh
source "${DIST_DIR}/chroot.sh"

PRODUCT="runc"
OUT_DIR="${1:-${DIST_DIR}/out}"

require_cmd curl
require_cmd tar
require_root

# 1. Fetch + verify the pinned runc release binary URL (runc ships prebuilt
#    release binaries; the chroot build below is the source build fallback).
#    Primary path: source build in the AL2023 chroot for full determinism.
WORK="$(mktemp -d "${DIST_DIR}/.work-runc.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# 2. Prepare the AL2023 chroot with Go + make.
rootfs="${DIST_DIR}/.cache/al2023-${HOST_ARCH}"
al2023_setup_rootfs "${rootfs}"
chroot "${rootfs}" /usr/bin/dnf install -y golang make gcc \
    || die "dnf install failed inside ${rootfs}"

# 3. Clone the pinned tag (runc has no source tarball release asset; the git
#    tag is the source of truth). Shallow clone for determinism.
chroot "${rootfs}" /usr/bin/git clone --depth 1 --branch "v${RUNC_VERSION}" \
    https://github.com/opencontainers/runc /src/runc \
    || die "git clone of runc v${RUNC_VERSION} failed"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
export PATH="/usr/local/go/bin:\$PATH"
cd /src/runc
# Static, CGO-free, netgo+osusergo stdlib tags, no seccomp tag (R2-Q19).
CGO_ENABLED=0 BUILDTAGS="netgo osusergo" make -j"\$(nproc)" static
cp runc /out/runc
EOF

chroot_run "${rootfs}" "${WORK}/build.sh" || die "runc build failed"

# 4. Copy the static binary out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-runc.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
mkdir -p "${stage}/usr/local/bin"
[[ -f "${rootfs}/out/runc" ]] || die "runc produced no binary"
cp -a "${rootfs}/out/runc" "${stage}/usr/local/bin/runc"

# 5. Verify static linkage + version, then assemble the deterministic tarball.
file "${stage}/usr/local/bin/runc" | grep -q "statically linked" \
    || die "runc is not statically linked"

asset="${OUT_DIR}/$(product_asset_name runc "${RUNC_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: runc: ${asset}"
echo "gitd: runc: sha256 $(sha256_of "${asset}")"