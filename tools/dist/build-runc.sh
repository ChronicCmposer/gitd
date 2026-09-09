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

export PRODUCT="runc"
OUT_DIR="${1:-${DIST_DIR}/out}"
# runc has no source tarball release asset; the build clones the pinned git
# tag inside the chroot, so git-core is part of this product's dnf dependency
# set (and therefore of its content-addressed rootfs key). git-core (not the
# full git package, which drags in perl/openssh/systemd) provides the git
# binary; glibc-static provides libc.a for runc's -linkmode external
# -static-pie link.
PRODUCT_DNF_DEPS="golang make gcc git-core glibc-static"

require_cmd curl
require_cmd tar
require_chroot_priv

# 1. Fetch + verify the pinned runc release binary URL (runc ships prebuilt
#    release binaries; the chroot build below is the source build fallback).
#    Primary path: source build in the AL2023 chroot for full determinism.
WORK="$(mktemp -d "${DIST_DIR}/.work-runc.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT

# 2. Prepare the AL2023 chroot with Go + make + git.
rootfs="$(ensure_rootfs al2023)"
install_deps "${rootfs}" al2023 "${PRODUCT_DNF_DEPS}"

# 3. Stage the source: the pinned git tag is cloned shallowly inside the
#    chroot by build.sh (the tag is the source of truth). Wipe any previous
#    run's staging so a reused rootfs never sees a stale clone or output.
rm -rf "${rootfs}/src" "${rootfs}/out"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
export PATH="/usr/local/go/bin:\$PATH"
git clone --depth 1 --branch "v${RUNC_VERSION}" \
    https://github.com/opencontainers/runc /src/runc
cd /src/runc
# Static, no seccomp tag (R2-Q19). BUILDTAGS must be a COMMAND-LINE make arg
# (after the target): runc's Makefile defaults BUILDTAGS to
# "seccomp urfave_cli_no_docs" with a := (plain env-prefix assignments lose).
# CGO stays enabled — runc's static-bin uses -linkmode external (cgo) with
# -extldflags -static-pie, which needs cgo and yields a fully static binary.
make -j"\$(nproc)" static BUILDTAGS="netgo osusergo"
cp runc /out/runc
EOF

run_in_rootfs "${rootfs}" "${WORK}/build.sh" || die "runc build failed"

# 4. Copy the static binary out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-runc.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
mkdir -p "${stage}/usr/local/bin"
[[ -f "${rootfs}/out/runc" ]] || die "runc produced no binary"
cp -a "${rootfs}/out/runc" "${stage}/usr/local/bin/runc"

# 5. Verify static linkage + version, then assemble the deterministic tarball.
# Go's -buildmode=pie + internal linking yields a static-PIE; accept both
# "statically linked" and "static-pie linked" (both have no interpreter).
file "${stage}/usr/local/bin/runc" | grep -qE "statically linked|static-pie linked" \
    || die "runc is not statically linked"

asset="${OUT_DIR}/$(product_asset_name runc "${RUNC_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: runc: ${asset}"
echo "gitd: runc: sha256 $(sha256_of "${asset}")"