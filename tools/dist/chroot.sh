# tools/dist/chroot.sh — shared chroot setup for the dist pipeline.
#
# Two rootfs flavors:
#   alpine  (musl)  -> image binaries (openssh, git, fish, sudo, ca-certificates)
#   al2023  (glibc) -> host binaries (containerd, runc)
#
# Both are cached under tools/dist/.cache/<flavor>-<arch>/; a cache marker
# records the exact rootfs source so a rebuild reuses the same base instead of
# silently drifting. Every function fails fast and loudly.

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

DIST_CACHE="${DIST_DIR}/.cache"

# alpine_minirootfs_url resolves the newest minirootfs tarball in the pinned
# branch for the build arch. The listing is sorted with `sort -V` so the last
# match is the newest patch release of the branch.
alpine_minirootfs_url() {
    local listing arch
    arch="$( [[ "${HOST_ARCH}" == "amd64" ]] && echo x86_64 || echo aarch64 )"
    listing="$(curl -fsSL --max-time 30 "${ALPINE_MIRROR}/${ALPINE_BRANCH}/releases/${arch}/")" \
        || die "cannot list alpine ${ALPINE_BRANCH} ${arch} releases"
    printf '%s\n' "${listing}" \
        | grep -oE "alpine-minirootfs-${ALPINE_BRANCH#v}[0-9.]*-${arch}\.tar\.gz" \
        | sort -uV \
        | tail -1 \
        || die "no alpine minirootfs found for ${ALPINE_BRANCH} ${arch}"
}

# alpine_setup_rootfs ROOTFS downloads + extracts the alpine minirootfs,
# pins the apk repositories to the branch, and copies the host resolver so apk
# and builds can reach the network. Idempotent: a populated rootfs is reused.
alpine_setup_rootfs() {
    local rootfs="$1" tarball marker url
    require_root
    marker="${rootfs}/.gitd-chroot"

    if [[ -f "${marker}" ]]; then
        return 0   # cached chroot from a previous run; reuse for determinism
    fi
    [[ -d "${rootfs}" ]] && die "rootfs '${rootfs}' exists without a marker; remove it to rebuild"

    url="$(alpine_minirootfs_url)"
    mkdir -p "$(dirname "${rootfs}")"
    require_network "${url}"
    tarball="${DIST_CACHE}/$(basename "${url}")"
    mkdir -p "${DIST_CACHE}"
    download "${url}" "${tarball}"

    mkdir -p "${rootfs}"
    tar -xzf "${tarball}" -C "${rootfs}" \
        || die "failed to extract alpine minirootfs into ${rootfs}"
    cp /etc/resolv.conf "${rootfs}/etc/resolv.conf"
    cat > "${rootfs}/etc/apk/repositories" <<EOF
${ALPINE_MIRROR}/${ALPINE_BRANCH}/main
${ALPINE_MIRROR}/${ALPINE_BRANCH}/community
EOF
    printf 'alpine minirootfs %s (%s)\n' "$(basename "${url}")" "$(date -u +%Y-%m-%d)" > "${marker}"
}

# alpine_install ROOTFS PKG... runs apk inside the chroot to install the
# pinned-branch build dependencies. --no-scripts keeps installs deterministic.
alpine_install() {
    local rootfs="$1"
    shift
    require_root
    chroot "${rootfs}" /sbin/apk add --no-cache --no-scripts "$@" \
        || die "apk install failed inside ${rootfs}"
}

# chroot_run ROOTFS SCRIPT copies a build script into the chroot and executes
# it with /bin/sh. The script must be self-contained (it may source nothing
# from the host; the product build script writes it via a heredoc).
chroot_run() {
    local rootfs="$1" script="$2"
    require_root
    cp "${script}" "${rootfs}/build.sh"
    chroot "${rootfs}" /bin/sh /build.sh \
        || die "build script failed inside ${rootfs}"
}

# al2023_setup_rootfs ROOTFS exports the AL2023 container image to a rootfs
# tarball via crane (a static binary downloaded to the cache), then extracts
# it. AL2023 has no minirootfs tarball; the ECR container image is the rootfs.
al2023_setup_rootfs() {
    local rootfs="$1" tarball marker crane
    require_root
    marker="${rootfs}/.gitd-chroot"

    if [[ -f "${marker}" ]]; then
        return 0
    fi
    [[ -d "${rootfs}" ]] && die "rootfs '${rootfs}' exists without a marker; remove it to rebuild"

    require_cmd curl
    require_cmd tar
    crane="${DIST_CACHE}/crane"
    if [[ ! -x "${crane}" ]]; then
        mkdir -p "${DIST_CACHE}"
        download \
            "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_${HOST_ARCH}.tar.gz" \
            "${DIST_CACHE}/crane.tar.gz"
        tar -xzf "${DIST_CACHE}/crane.tar.gz" -C "${DIST_CACHE}" crane
        chmod +x "${crane}"
    fi

    require_network "https://public.ecr.aws"
    tarball="${DIST_CACHE}/al2023-rootfs-${HOST_ARCH}.tar"
    "${crane}" export "${AL2023_IMAGE}" "${tarball}" \
        || die "crane export of ${AL2023_IMAGE} failed"

    mkdir -p "${rootfs}"
    tar -xf "${tarball}" -C "${rootfs}" \
        || die "failed to extract AL2023 rootfs into ${rootfs}"
    cp /etc/resolv.conf "${rootfs}/etc/resolv.conf"
    printf 'al2023 container image %s\n' "${AL2023_IMAGE}" > "${marker}"
}