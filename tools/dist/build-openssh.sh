#!/usr/bin/env bash
# tools/dist/build-openssh.sh — build a static musl OpenSSH (R2-Q17).
#
# Produces:  tools/dist/out/openssh-10.5p1.linux-<arch>.tar.gz
#            (usr/local/bin/sshd, usr/local/bin/ssh-keygen, usr/local/libexec/...)
#
# The build runs inside an alpine (musl) chroot and links OpenSSL statically
# via the alpine openssl-libs-static package (R2-Q17). --without-openssl is
# supported ONLY as an explicitly-marked fallback via
#   OPENSSH_OPENSSL_MODE=without-openssl ./tools/dist/build-openssh.sh
# and is flagged EXPERIMENTAL upstream. The seccomp privsep sandbox
# (--with-sandbox=seccomp_filter) is required and verified.
#
# Usage:
#   tools/dist/build-openssh.sh [OUT_DIR]   # default tools/dist/out

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"
# shellcheck source=tools/dist/chroot.sh
source "${DIST_DIR}/chroot.sh"

export PRODUCT="openssh"
OPENSSH_MODE="${OPENSSH_OPENSSL_MODE:-with-openssl}"
OUT_DIR="${1:-${DIST_DIR}/out}"

require_cmd curl
require_cmd tar
require_chroot_priv

case "${OPENSSH_MODE}" in
    with-openssl)
        # Preferred path: static link against the pinned-branch
        # openssl-libs-static (R2-Q17).
        OPENSSH_CONFIG_OPENSSL=""
        PRODUCT_APK_DEPS="build-base musl-dev linux-headers openssl-dev openssl-libs-static zlib-dev zlib-static"
        ;;
    without-openssl)
        # EXPERIMENTAL upstream fallback only (OpenSSH configure marks
        # --without-openssl as EXPERIMENTAL). No OpenSSL at all; the internal
        # crypto is used. Deterministic but not the primary path. Note the
        # content-addressed rootfs key changes with this dep set, so the
        # without-openssl build gets its own rootfs cache dir.
        echo "gitd: openssh: WARNING: --without-openssl fallback is EXPERIMENTAL upstream; prefer with-openssl" >&2
        OPENSSH_CONFIG_OPENSSL="--without-openssl"
        PRODUCT_APK_DEPS="build-base musl-dev linux-headers"
        ;;
    *)
        die "OPENSSH_OPENSSL_MODE must be with-openssl or without-openssl, got '${OPENSSH_MODE}'"
        ;;
esac

# 1. Fetch + verify the pinned source tarball.
WORK="$(mktemp -d "${DIST_DIR}/.work-openssh.XXXXXX")"
trap 'rm -rf "${WORK}"' EXIT
source_tar="${WORK}/openssh-${OPENSSH_VERSION}.tar.gz"
download "${OPENSSH_URL}" "${source_tar}" "${OPENSSH_SHA256}"

# 2. Prepare the alpine chroot with the toolchain + (optionally) OpenSSL statics.
rootfs="$(ensure_rootfs alpine)"
install_deps "${rootfs}" alpine "${PRODUCT_APK_DEPS}"

# 3. Stage the source + patch inside the chroot.
mkdir -p "${WORK}/src"
tar -xzf "${source_tar}" -C "${WORK}/src"
# The committed patch is a git format-patch whose index line carries
# all-zero blob hashes; GNU patch misreads that as a new-file creation and
# refuses to apply it. Normalize it to a plain unified diff at staging time
# (drop the index line; the mail preamble/diffstat are already harmless),
# then stage it as a sibling of the source tree.
sed '/^index /d' \
    "${DIST_DIR}/patches/openssh-${OPENSSH_VERSION}-auth-identity.patch" \
    > "${WORK}/src/openssh-${OPENSSH_VERSION}-auth-identity.patch"
# The cached rootfs may already hold a previous run's staging; wipe it so the
# build always compiles from the pristine pinned source (a reused rootfs must
# never see stale source/objects, and cp -a of an existing dir would nest).
rm -rf "${rootfs}/src" "${rootfs}/out"
cp -a "${WORK}/src" "${rootfs}/src"
mkdir -p "${rootfs}/out"

cat > "${WORK}/build.sh" <<EOF
set -euo pipefail
cd /src/openssh-${OPENSSH_VERSION}
# The patch is staged next to the source tree (/src/<name>, sibling of
# /src/openssh-<version>/); reference it by its absolute chroot path.
patch -p1 < /src/openssh-${OPENSSH_VERSION}-auth-identity.patch
./configure \
    --prefix=/usr/local \
    --host=${HOST_ARCH}-alpine-linux-musl \
    --with-privsep-user=sshd \
    --with-privsep-path=/var/empty \
    --with-sandbox=seccomp_filter \
    --without-pam \
    --without-kerberos5 \
    --without-libedit \
    --without-ldns \
    --without-selinux \
    --without-ssl-engine \
    --without-rpath \
    --disable-shared \
    ${OPENSSH_CONFIG_OPENSSL}
# Note: LDFLAGS must keep configure's -L. -Lopenbsd-compat/ paths (the static
# libssh.a / libopenbsd-compat.a live in the build tree) — overriding them
# wholesale makes the final link fail with "cannot find -lssh".
make -j"$(nproc)" CFLAGS="-O2 -static" LDFLAGS="-static -L. -Lopenbsd-compat/"
make install DESTDIR=/out
# The seccomp privsep sandbox is a hard requirement (R2-Q17); fail loudly if
# configure did not compile it in.
grep -q "SANDBOX_SECCOMP_FILTER" config.h || { echo "gitd: openssh: seccomp sandbox not compiled in" >&2; exit 1; }
EOF

run_in_rootfs "${rootfs}" "${WORK}/build.sh" || die "openssh build failed"

# 4. Copy the static binaries out of the chroot.
mkdir -p "${OUT_DIR}"
stage="$(mktemp -d "${DIST_DIR}/.stage-openssh.XXXXXX")"
trap 'rm -rf "${WORK}" "${stage}"' EXIT
cp -a "${rootfs}/out/usr/." "${stage}/usr/"
# OpenSSH installs the (unused) sftp-server under libexec; keep it out of the
# image — sshd_config sets Subsystem sftp none (R7-Q9).
rm -f "${stage}/usr/local/libexec/sftp-server"

# 5. Verify static linkage, then assemble the deterministic tarball.
file "${stage}/usr/local/sbin/sshd" | grep -q "statically linked" \
    || die "sshd is not statically linked"
file "${stage}/usr/local/bin/ssh-keygen" | grep -q "statically linked" \
    || die "ssh-keygen is not statically linked"

asset="${OUT_DIR}/$(product_asset_name openssh "${OPENSSH_VERSION}")"
tar_reproducible "${asset}" "${stage}"
echo "gitd: openssh: ${asset}"
echo "gitd: openssh: sha256 $(sha256_of "${asset}")"