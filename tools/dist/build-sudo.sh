#!/usr/bin/env bash
# tools/dist/build-sudo.sh — build a static musl sudo for the image (R8-Q1).
#
# Produces:  tools/dist/out/sudo-1.9.17p2.linux-<arch>.tar.gz
#            (usr/local/bin/sudo)
#
# The image has no PAM, LDAP, SSSD, or shared libs; sudo is built with the
# sudoers policy module compiled in (--enable-static-sudoers) and everything
# static. The scoped sudoers file (gitd spool/mirror verbs only, NOPASSWD as
# git) is baked into the image at build time (image/fs/etc/sudoers), not here.
#
# Usage:
#   tools/dist/build-sudo.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/dist/chroot.sh
source "${DIST_DIR}/chroot.sh"

PRODUCT="sudo"
OUT_DIR="${1:-${DIST_DIR}/out}"

require_cmd curl
require_cmd tar
require_root

# 1. Fetch + verify the pinned source tarball.
WORK="$(mktemp -d "${DIST_DIR}/.work-sudo.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
source_tar="${WORK}/sudo-${SUDO_VERSION}.tar.gz"
download "${SUDO_URL}" "${source_tar}" "${SUDO_SHA256}"

# 2. Prepare the alpine chroot with the C toolchain.
rootfs="${DIST_DIR}/.cache/alpine-${HOST_ARCH}"
alpine_setup_rootfs "${rootfs}"
alpine_install "${rootfs}" build-base musl-dev linux-headers

# 3. Stage the source inside the chroot.
mkdir -p "${WORK}/src"
tar -xzf "${source_tar}" -C "${WORK}/src"
cp -a "${WORK}/src" "${rootfs}/src"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
cd /src/sudo-${SUDO_VERSION}
./configure \
    --prefix=/usr/local \
    --host=${HOST_ARCH}-alpine-linux-musl \
    --disable-shared \
    --enable-static \
    --enable-static-sudoers \
    --without-pam \
    --without-sssd \
    --without-ldap \
    --without-sendmail \
    --disable-nls \
    CFLAGS="-O2 -static" \
    LDFLAGS="-static"
make -j"$(nproc)"
make install DESTDIR=/out
EOF

chroot_run "${rootfs}" "${WORK}/build.sh" || die "sudo build failed"

# 4. Copy the static binary out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-sudo.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
cp -a "${rootfs}/out/usr/local/bin/sudo" "${stage}/usr/local/bin/sudo"

# 5. Verify static linkage, then assemble the deterministic tarball.
file "${stage}/usr/local/bin/sudo" | grep -q "statically linked" \
    || die "sudo is not statically linked"

asset="${OUT_DIR}/$(product_asset_name sudo "${SUDO_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: sudo: ${asset}"
echo "gitd: sudo: sha256 $(sha256_of "${asset}")"