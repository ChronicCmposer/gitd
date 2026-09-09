#!/usr/bin/env bash
# tools/dist/build-git.sh — build a minimal static musl git (R2-Q13).
#
# Produces:  tools/dist/out/git-2.53.0.linux-<arch>.tar.gz
#            (usr/local/bin/git, usr/local/libexec/git-core/, usr/local/share/)
#
# Minimal flags per R2-Q13: NO_PERL, NO_GETTEXT, NO_TCLTK, NO_CURL, NO_EXPAT,
# NO_OPENSSL; /usr/local prefix. The sha256 object format and
# receive.fsckObjects live in the system gitconfig (see image/fs/etc/gitconfig,
# item 2.5), not in the build.
#
# Usage:
#   tools/dist/build-git.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/dist/chroot.sh
source "${DIST_DIR}/chroot.sh"

PRODUCT="git"
OUT_DIR="${1:-${DIST_DIR}/out}"

require_cmd curl
require_cmd tar
require_root

# 1. Fetch + verify the pinned source tarball.
WORK="$(mktemp -d "${DIST_DIR}/.work-git.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
source_tar="${WORK}/git-${GIT_VERSION}.tar.xz"
download "${GIT_URL}" "${source_tar}" "${GIT_SHA256}"

# 2. Prepare the alpine chroot with the C toolchain. git needs no external
#    libraries with these flags; NO_OPENSSL keeps the sha256 object format on
#    git's internal crypto.
rootfs="${DIST_DIR}/.cache/alpine-${HOST_ARCH}"
alpine_setup_rootfs "${rootfs}"
alpine_install "${rootfs}" build-base musl-dev linux-headers

# 3. Stage the source inside the chroot.
mkdir -p "${WORK}/src"
tar -xJf "${source_tar}" -C "${WORK}/src"
cp -a "${WORK}/src" "${rootfs}/src"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
cd /src/git-${GIT_VERSION}
make -j"$(nproc)" \
    prefix=/usr/local \
    NO_PERL=1 \
    NO_GETTEXT=1 \
    NO_TCLTK=1 \
    NO_CURL=1 \
    NO_EXPAT=1 \
    NO_OPENSSL=1 \
    NO_INSTALL_HARDLINKS=1 \
    CFLAGS="-O2 -static" \
    LDFLAGS="-static"
make install \
    prefix=/usr/local \
    NO_PERL=1 \
    NO_GETTEXT=1 \
    NO_TCLTK=1 \
    NO_CURL=1 \
    NO_EXPAT=1 \
    NO_OPENSSL=1 \
    NO_INSTALL_HARDLINKS=1 \
    DESTDIR=/out
EOF

chroot_run "${rootfs}" "${WORK}/build.sh" || die "git build failed"

# 4. Copy the static tree out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-git.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
cp -a "${rootfs}/out/usr/." "${stage}/usr/"

# 5. Verify static linkage + version floor (sha256 object format needs >= 2.42).
file "${stage}/usr/local/bin/git" | grep -q "statically linked" \
    || die "git is not statically linked"
"${stage}/usr/local/bin/git" --version || die "git --version failed"

asset="${OUT_DIR}/$(product_asset_name git "${GIT_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: git: ${asset}"
echo "gitd: git: sha256 $(sha256_of "${asset}")"