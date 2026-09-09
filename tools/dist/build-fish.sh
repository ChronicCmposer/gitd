#!/usr/bin/env bash
# tools/dist/build-fish.sh — build a static musl fish (R2-Q18).
#
# Produces:  tools/dist/out/fish-4.9.3.linux-<arch>.tar.gz
#            (usr/local/bin/fish, usr/local/share/)
#
# fish 4.x is Rust-based but still orchestrated by cmake. The static recipe:
#   - CMAKE_FIND_LIBRARY_SUFFIXES=.a (find only static libs)
#   - BUILD_SHARED_LIBS=OFF
#   - C/CXX flags + executable linker flags -static
#   - system pcre2 via pcre2-static (FISH_USE_SYSTEM_PCRE2=ON)
#   - RUSTFLAGS=-C target-feature=+crt-static for the Rust parts
# Alpine's rust/cargo toolchain targets musl by default, so the built fish is
# a static musl binary.
#
# Usage:
#   tools/dist/build-fish.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/dist/chroot.sh
source "${DIST_DIR}/chroot.sh"

export PRODUCT="fish"
OUT_DIR="${1:-${DIST_DIR}/out}"
# The full apk set this product installs into its rootfs (cmake + rust + static
# pcre2); the content-addressed cache key changes with this list.
PRODUCT_APK_DEPS="build-base musl-dev linux-headers cmake ninja rust cargo pcre2-dev pcre2-static"

require_cmd curl
require_cmd tar
require_chroot_priv

# 1. Fetch + verify the pinned source tarball.
WORK="$(mktemp -d "${DIST_DIR}/.work-fish.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
source_tar="${WORK}/fish-${FISH_VERSION}.tar.xz"
download "${FISH_URL}" "${source_tar}" "${FISH_SHA256}"

# 2. Prepare the alpine chroot with cmake + rust + static pcre2.
rootfs="$(ensure_rootfs alpine)"
install_deps "${rootfs}" alpine "${PRODUCT_APK_DEPS}"

# 3. Stage the source inside the chroot.
mkdir -p "${WORK}/src"
tar -xJf "${source_tar}" -C "${WORK}/src"
# The cached rootfs may already hold a previous run's staging; wipe it so the
# build always compiles from the pristine pinned source (a reused rootfs must
# never see stale source/objects, and cp -a of an existing dir would nest).
rm -rf "${rootfs}/src" "${rootfs}/out"
cp -a "${WORK}/src" "${rootfs}/src"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
export RUSTFLAGS="-C target-feature=+crt-static"
cd /src/fish-${FISH_VERSION}
cmake -B build -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_INSTALL_PREFIX=/usr/local \
    -DCMAKE_FIND_LIBRARY_SUFFIXES=".a" \
    -DBUILD_SHARED_LIBS=OFF \
    -DCMAKE_C_FLAGS="-static" \
    -DCMAKE_CXX_FLAGS="-static" \
    -DCMAKE_EXE_LINKER_FLAGS="-static" \
    -DWITH_MESSAGE_LOCALIZATION=OFF \
    -DWITH_DOCS=OFF \
    -DFISH_USE_SYSTEM_PCRE2=ON
cmake --build build
DESTDIR=/out cmake --install build
EOF

run_in_rootfs "${rootfs}" "${WORK}/build.sh" || die "fish build failed"

# 4. Copy the static tree out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-fish.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
cp -a "${rootfs}/out/usr/." "${stage}/usr/"

# 5. Verify static linkage, then assemble the deterministic tarball.
# fish links as a static-PIE (Rust crt-static + alpine's default-PIE gcc);
# "static-pie linked" is fully static too (no dynamic interpreter), so accept
# both spellings.
file "${stage}/usr/local/bin/fish" | grep -qE "statically linked|static-pie linked" \
    || die "fish is not statically linked"

asset="${OUT_DIR}/$(product_asset_name fish "${FISH_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: fish: ${asset}"
echo "gitd: fish: sha256 $(sha256_of "${asset}")"