# tools/dist/versions.sh — single source of truth for every pinned artifact.
#
# Every version, URL, and checksum in the dist pipeline lives here so a pin
# bump is one edit, and so build scripts, gen-dist-pins.sh, and the Bazel
# dist.bzl stay consistent by construction.
#
# Determinism convention (R3-Q2, 2.3):
#   SOURCE_DATE_EPOCH is fixed per release; the build scripts pass it to every
#   tool that can embed timestamps (gcc, make, cmake, cargo, tar, gzip). The
#   build-twice determinism check (make check-*-dist) must produce byte-identical
#   tarballs before any publish is allowed.

set -euo pipefail

# --- Build host ------------------------------------------------------------------
HOST_ARCH=""
case "$(uname -m)" in
    x86_64|amd64) HOST_ARCH="amd64" ;;
    aarch64|arm64) HOST_ARCH="arm64" ;;
    *) echo "gitd: unsupported build host architecture: $(uname -m)" >&2; exit 1 ;;
esac

# The dist pipeline builds in chroots: alpine (musl) for image binaries,
# al2023 (glibc) for host binaries. These are the chroot rootfs pins.
ALPINE_BRANCH="v3.22"                       # alpine stable branch (musl)
ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine"
AL2023_IMAGE="public.ecr.aws/amazonlinux/amazonlinux:2023"

# SOURCE_DATE_EPOCH: fixed at the openssh-10.5p1 release timestamp so every
# build is reproducible independent of when it runs. Bump only when a product
# pin changes.
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-1787424000}"

# --- OpenSSH (image binary; R2-Q17) ----------------------------------------------
OPENSSH_VERSION="10.5p1"
OPENSSH_URL="https://cdn.openbsd.org/pub/OpenBSD/OpenSSH/portable/openssh-${OPENSSH_VERSION}.tar.gz"
OPENSSH_SHA256=""                           # filled from the tarball by check-dist

# --- git (image binary; R2-Q13) ---------------------------------------------------
GIT_VERSION="2.53.0"
GIT_URL="https://mirrors.edge.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.xz"
GIT_SHA256=""                               # filled from the tarball by check-dist

# --- fish (image shell; R2-Q18) ---------------------------------------------------
FISH_VERSION="4.9.3"
FISH_URL="https://github.com/fish-shell/fish-shell/releases/download/${FISH_VERSION}/fish-${FISH_VERSION}.tar.xz"
FISH_SHA256=""                              # filled from the tarball by check-dist

# --- sudo (image admin elevation; R8-Q1) ------------------------------------------
SUDO_VERSION="1.9.17p2"
SUDO_URL="https://github.com/sudo-project/sudo/releases/download/v${SUDO_VERSION}/sudo-${SUDO_VERSION}.tar.gz"
SUDO_SHA256=""                              # filled from the tarball by check-dist

# --- ca-certificates (image TLS trust store; R7-Q2) --------------------------------
# The apk package ships the mozilla bundle; the post-install step generates
# /etc/ssl/certs/* (hashed symlinks + ca-certificates.crt). Pinned exactly.
# Alpine's apk arch differs from the Go arch spelling used above.
case "${HOST_ARCH}" in
    amd64) ALPINE_APK_ARCH="x86_64" ;;
    arm64) ALPINE_APK_ARCH="aarch64" ;;
    *) echo "gitd: unsupported HOST_ARCH ${HOST_ARCH}" >&2; exit 1 ;;
esac
CA_CERTS_PACKAGE="ca-certificates"
CA_CERTS_VERSION="20260611-r0"
CA_CERTS_APK_URL="${ALPINE_MIRROR}/${ALPINE_BRANCH}/main/${ALPINE_APK_ARCH}/${CA_CERTS_PACKAGE}-${CA_CERTS_VERSION}.apk"
CA_CERTS_SHA256="6b491dcda951129c80e8d7b0f509253ab640b20653b208d3b0994d893189b3f5"

# --- containerd + runc (host binaries; R2-Q19) -------------------------------------
CONTAINERD_VERSION="2.3.5"
CONTAINERD_URL="https://github.com/containerd/containerd/releases/download/v${CONTAINERD_VERSION}/containerd-${CONTAINERD_VERSION}-linux-${HOST_ARCH}.tar.gz"
CONTAINERD_SHA256=""                        # filled from the tarball by check-dist
RUNC_VERSION="1.2.9"
RUNC_URL="https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${HOST_ARCH}"
RUNC_SHA256=""                              # filled from the binary by check-dist

# --- Go toolchain (2.3) -------------------------------------------------------------
# Must match MODULE.bazel's go_sdk.download and rules_go 0.63.0 support.
GO_VERSION="1.26.5"
GO_AMD64_SHA256="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"
GO_ARM64_SHA256="fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49"

# --- Mirror (2.4, R3-Q2) --------------------------------------------------------------
# GitHub Releases is the primary artifact source; S3 is the fallback. Bucket
# git.cmposer.cc, product prefixes openssh/ git/ fish/ containerd/.
DIST_REPO="ChronicCmposer/gitd-dist"
S3_BUCKET="git.cmposer.cc"
S3_REGION="us-east-2"
S3_BASE="https://s3.${S3_REGION}.amazonaws.com/${S3_BUCKET}"
GITHUB_BASE="https://github.com/${DIST_REPO}/releases/download"