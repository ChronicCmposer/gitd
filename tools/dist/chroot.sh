# tools/dist/chroot.sh — shared chroot setup for the dist pipeline.
#
# Two rootfs flavors:
#   alpine  (musl)  -> image binaries (openssh, git, fish, sudo)
#   al2023  (glibc) -> host binaries (containerd, runc)
#
# The rootfs cache is content-addressed and lives in the user's home
# directory, never under /tmp (a small tmpfs) and never inside the repo:
#   ${XDG_CACHE_HOME:-$HOME/.cache}/gitd-dist/rootfs-<flavor>-<arch>-<id>/
# where <id> is the first 12 hex chars of the sha256 of a deterministic
# fingerprint of everything that changes the rootfs content: the flavor, the
# build arch, the pinned versions.sh, the rootfs base (ALPINE_BRANCH/mirror or
# AL2023_IMAGE), and the sorted apk/dnf dependency set of the requesting
# product. Same deps => same id => same cache dir, reused across runs and
# across hosts; two products that share the identical dep fingerprint share
# the same rootfs dir, and a product that changes its dependency set gets a
# NEW dir automatically (no manual cache invalidation). Provisioning is
# atomic (strimserver-style): the rootfs is extracted into $rootfs.new, marked
# with a .provisioned sentinel, and mv'd into place, so an aborted build
# resumes cleanly on the next run.
#
# Build scripts declare PRODUCT_APK_DEPS (alpine) or PRODUCT_DNF_DEPS
# (al2023) and use the small API here — ensure_rootfs, install_deps,
# run_in_rootfs — instead of inlining chroot/unshare logic.
#
# Privilege model (strimserver wrapper order): chroot builds need uid-0
# semantics. Real root gets a private mount+pid namespace (fast path);
# passwordless sudo gets the same via `sudo -n`; otherwise each privileged
# phase runs inside ONE `unshare --user --map-root-user --mount --pid --fork`
# namespace (the uid/gid mapping is fixed at namespace creation and the
# capabilities needed for mount/chroot only exist inside that same namespace).
# When none of the three works the pipeline fails loudly with the actionable
# remediation (run as root, with passwordless sudo, or where
# 'unshare -Urmpf true' works).
#
# The rootfs tarballs contain char device nodes under /dev/ (e.g. AL2023's
# /dev/null as 1,3). mknod of char/block devices requires CAP_MKNOD in the
# initial user namespace, which an unprivileged userns does not have, so
# extraction excludes the whole /dev tree and the host device nodes are
# bind-mounted in instead. Extraction also uses --no-same-owner because
# archive gids that are unmapped here make chown fail with EINVAL.

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

# --- Content-addressed rootfs cache ------------------------------------------

# deps_sorted PKG... — canonical, deterministic package list: whitespace-split,
# de-duplicated, sorted one-per-line. The single source of both the rootfs
# fingerprint and the per-rootfs installed marker, so the two can never drift.
deps_sorted() {
    printf '%s\n' "$@" | tr ' ' '\n' | sed '/^[[:space:]]*$/d' | sort -u
}

# rootfs_deps FLAVOR — the sorted, space-joined package set that the requesting
# product installs into the FLAVOR rootfs. Alpine products declare
# PRODUCT_APK_DEPS; al2023 products declare PRODUCT_DNF_DEPS. Fail fast when
# the declaring variable is unset: a rootfs requested without its dependency
# set would silently reuse a cache dir keyed on the wrong content.
rootfs_deps() {
    local flavor="$1"
    case "${flavor}" in
        alpine)
            [[ -n "${PRODUCT_APK_DEPS:-}" ]] \
                || die "rootfs_deps alpine: PRODUCT_APK_DEPS is unset; declare the apk set in the build script"
            deps_sorted "${PRODUCT_APK_DEPS}" | paste -sd ' ' -
            ;;
        al2023)
            [[ -n "${PRODUCT_DNF_DEPS:-}" ]] \
                || die "rootfs_deps al2023: PRODUCT_DNF_DEPS is unset; declare the dnf set in the build script"
            deps_sorted "${PRODUCT_DNF_DEPS}" | paste -sd ' ' -
            ;;
        *) die "unknown rootfs flavor '${flavor}'" ;;
    esac
}

# rootfs_fingerprint FLAVOR — a deterministic, complete description of the
# FLAVOR rootfs content: flavor, build arch, the sha256 of the whole pinned
# versions.sh (so any pin bump anywhere invalidates every cached rootfs), the
# rootfs base pins, and the sorted dependency set.
rootfs_fingerprint() {
    local flavor="$1"
    {
        printf 'flavor=%s\n' "${flavor}"
        printf 'arch=%s\n' "${HOST_ARCH}"
        printf 'versions_sha256=%s\n' "$(sha256_of "${DIST_DIR}/versions.sh")"
        case "${flavor}" in
            alpine)
                printf 'branch=%s\n' "${ALPINE_BRANCH}"
                printf 'mirror=%s\n' "${ALPINE_MIRROR}"
                printf 'apk_deps=%s\n' "$(rootfs_deps alpine)"
                ;;
            al2023)
                printf 'image=%s\n' "${AL2023_IMAGE}"
                printf 'dnf_deps=%s\n' "$(rootfs_deps al2023)"
                ;;
            *) die "unknown rootfs flavor '${flavor}'" ;;
        esac
    }
}

# rootfs_key FLAVOR — the content-addressed cache id: first 12 hex chars of
# the sha256 of the fingerprint. Deterministic: same deps => same key => same
# cache dir, across runs and across hosts.
rootfs_key() {
    local flavor="$1"
    printf '%s' "$(rootfs_fingerprint "${flavor}")" | sha256sum | cut -c1-12
}

# cache_prune FLAVOR KEY — best-effort garbage collection of stale
# content-addressed rootfs dirs (untouched for 30 days, not the current key)
# so a long-lived cache does not grow without bound when dependency sets
# change. Best-effort only: failures print a warning and never abort the
# build.
cache_prune() {
    local flavor="$1" current_key="$2" path
    for path in "${DIST_CACHE}"/rootfs-${flavor}-${HOST_ARCH}-*; do
        [[ -d "${path}" ]] || continue
        [[ "$(basename "${path}")" == "rootfs-${flavor}-${HOST_ARCH}-${current_key}" ]] && continue
        [[ -n "$(find "${path}" -maxdepth 0 -mtime +30 2>/dev/null)" ]] || continue
        echo "gitd: ${PRODUCT:-dist}: pruning stale rootfs cache ${path}" >&2
        rm -rf "${path}" 2>/dev/null \
            || echo "gitd: ${PRODUCT:-dist}: WARNING: could not prune ${path}" >&2
    done
}

# ensure_rootfs FLAVOR [PRODUCT] — compute the content-addressed cache dir for
# FLAVOR from the requesting product's dependency fingerprint, extract the
# rootfs base if it is not cached yet (atomic .new + .provisioned + mv), and
# print the rootfs path (stdout carries ONLY the path). This is the single
# entry point build scripts use to obtain a rootfs; they must declare
# PRODUCT_APK_DEPS (alpine) or PRODUCT_DNF_DEPS (al2023) first. PRODUCT is
# only used in log messages (it is NOT part of the fingerprint — two products
# with identical deps share one rootfs dir) and defaults to the $PRODUCT env
# var every build script sets.
ensure_rootfs() {
    local flavor="$1" prod="${2:-${PRODUCT:-dist}}" key rootfs
    key="$(rootfs_key "${flavor}")"
    rootfs="${DIST_CACHE}/rootfs-${flavor}-${HOST_ARCH}-${key}"
    case "${flavor}" in
        alpine) alpine_setup_rootfs "${rootfs}" "${prod}" ;;
        al2023) al2023_setup_rootfs "${rootfs}" "${prod}" ;;
        *) die "unknown rootfs flavor '${flavor}'" ;;
    esac
    cache_prune "${flavor}" "${key}"
    printf '%s\n' "${rootfs}"
}

# ns_enter CMD... — run CMD with root-equivalent privileges, mirroring the
# strimserver ffmpeg-dist publish.sh privilege wrapper exactly: real root gets
# a private mount+pid namespace (so the bind mounts never leak into the host
# mount namespace), passwordless sudo gets the same, and everyone else gets a
# fresh user namespace via `unshare -Urmpf`. When none of the three works the
# failure is loud and actionable (the reference harness's exact message). The
# command's exit status is propagated so callers attach context with `|| die`.
ns_enter() {
    require_cmd unshare
    if [[ "${EUID}" -eq 0 ]]; then
        unshare --mount --pid --fork "$@"
    elif sudo -n true 2>/dev/null; then
        sudo -n unshare --mount --pid --fork "$@"
    elif unshare -Urmpf true 2>/dev/null; then
        unshare -Urmpf "$@"
    else
        echo "gitd: ${PRODUCT:-dist}: error: need root to mount proc and bind device nodes for the chroot;" >&2
        echo "       run as root, with passwordless sudo, or where 'unshare -Urmpf true' works." >&2
        exit 1
    fi
}

# rootfs_extract ROOTFS TARBALL — extract a rootfs tarball so it is usable in
# an unprivileged user namespace: the whole /dev tree is skipped (device nodes
# cannot be created there) and host devices are bind-mounted later;
# --no-same-owner avoids EINVAL chowns for archive gids that are unmapped.
# Benign tar warnings are silenced; any real error still fails loudly.
rootfs_extract() {
    local rootfs="$1" tarball="$2"
    mkdir -p "${rootfs}"
    tar -xf "${tarball}" -C "${rootfs}" \
        --no-same-owner \
        --exclude='dev/*' --exclude='dev' \
        --warning=no-unknown-keyword \
        || die "failed to extract ${tarball} into ${rootfs}"
    mkdir -p "${rootfs}/dev" "${rootfs}/proc" "${rootfs}/sys"
}

# rootfs_mount_special ROOTFS — bind-mount the host devices and proc into the
# rootfs so a chrooted build works. Must run inside the namespace (mount needs
# the userns capabilities). sysfs cannot be mounted in an unprivileged userns
# on most kernels and is not needed by these builds, so its failure is a
# warning, not an error.
rootfs_mount_special() {
    local rootfs="$1" dev
    mkdir -p "${rootfs}/dev" "${rootfs}/proc" "${rootfs}/sys"
    # /dev standard symlinks were excluded with the dev tree; recreate them.
    ln -sfn /proc/self/fd "${rootfs}/dev/fd"
    ln -sfn fd/0 "${rootfs}/dev/stdin"
    ln -sfn fd/1 "${rootfs}/dev/stdout"
    ln -sfn fd/2 "${rootfs}/dev/stderr"
    # Bind-mount the host device nodes (a bind mount needs an existing
    # mountpoint, so create an empty file first).
    for dev in null zero random urandom full tty; do
        [[ -e "/dev/${dev}" ]] || continue
        touch "${rootfs}/dev/${dev}"
        mount --bind "/dev/${dev}" "${rootfs}/dev/${dev}" \
            || die "cannot bind-mount /dev/${dev} into ${rootfs}"
    done
    # proc requires a pid namespace, which ns_enter creates.
    mount -t proc proc "${rootfs}/proc" \
        || die "cannot mount proc into ${rootfs}"
    # devpts is a nice-to-have for configure scripts that probe ptys.
    mkdir -p "${rootfs}/dev/pts"
    mount -t devpts devpts "${rootfs}/dev/pts" 2>/dev/null || true
    if ! mount -t sysfs sysfs "${rootfs}/sys" 2>/dev/null; then
        echo "gitd: ${PRODUCT:-dist}: WARNING: sysfs not mountable in this userns; continuing without /sys" >&2
    fi
}

# chroot_ns ROOTFS CMD... — mount the special filesystems and run CMD inside
# the rootfs, all under one namespace when not root.
chroot_ns() {
    local rootfs="$1"
    shift
    ns_enter bash -c '
        set -euo pipefail
        # shellcheck source=tools/dist/chroot.sh
        source "${DIST_DIR}/chroot.sh"
        rootfs="$1"; shift
        rootfs_mount_special "${rootfs}"
        chroot "${rootfs}" "$@"
    ' _ "${rootfs}" "$@"
}

# alpine_install_rootfs ROOTFS PKG... — install apk packages into an alpine
# rootfs. Idempotent: the canonical sorted package list is recorded in
# ROOTFS/.gitd-apk-deps and matching re-installs are skipped. The PKG list may
# be a single space-separated string (as declared in PRODUCT_APK_DEPS).
alpine_install_rootfs() {
    local rootfs="$1" marker deps
    shift
    marker="${rootfs}/.gitd-apk-deps"
    deps="$(deps_sorted "$@" | paste -sd ' ' -)"
    if [[ -f "${marker}" ]] && [[ "$(cat "${marker}")" == "${deps}" ]]; then
        return 0
    fi
    read -r -a pkgs <<< "${deps}"
    chroot_ns "${rootfs}" /sbin/apk add --no-cache --no-scripts "${pkgs[@]}" \
        || die "apk install failed inside ${rootfs}"
    printf '%s\n' "${deps}" > "${marker}"
}

# al2023_install_rootfs ROOTFS PKG... — dnf equivalent of
# alpine_install_rootfs (idempotent marker ROOTFS/.gitd-dnf-deps).
#
# The first dnf run on a fresh AL2023 rootfs can fail: dnf pulls incidental
# base-image package updates, and rpm's cpio cannot chown their setgid/setuid
# files to gids that are unmapped in the user namespace ("cpio: chown
# failed"), aborting the transaction. The REQUESTED packages are installed
# regardless (rpm commits each package's step before the failing one). So on
# failure we retry once (dnf is idempotent; the second run reports success)
# and then verify the requested set with rpm -q — a stale base-image update
# must never block the build.
al2023_install_rootfs() {
    local rootfs="$1" marker deps
    shift
    marker="${rootfs}/.gitd-dnf-deps"
    deps="$(deps_sorted "$@" | paste -sd ' ' -)"
    if [[ -f "${marker}" ]] && [[ "$(cat "${marker}")" == "${deps}" ]]; then
        return 0
    fi
    read -r -a pkgs <<< "${deps}"
    if ! chroot_ns "${rootfs}" /usr/bin/dnf install -y "${pkgs[@]}"; then
        echo "gitd: ${PRODUCT:-dist}: WARNING: first dnf transaction failed (userns chown on base-image updates); retrying" >&2
        chroot_ns "${rootfs}" /usr/bin/dnf install -y "${pkgs[@]}" \
            || die "dnf install failed inside ${rootfs}"
    fi
    chroot_ns "${rootfs}" /usr/bin/rpm -q "${pkgs[@]}" >/dev/null 2>&1 \
        || die "dnf install did not provide ${deps} inside ${rootfs}"
    printf '%s\n' "${deps}" > "${marker}"
}

# install_deps ROOTFS FLAVOR PKG... — idempotently install the apk (alpine) or
# dnf (al2023) package set into ROOTFS: the canonical sorted package list is
# recorded in ROOTFS/.gitd-apk-deps or ROOTFS/.gitd-dnf-deps and matching
# re-installs are skipped. PKG... may be a single space-separated string (as
# declared in PRODUCT_APK_DEPS / PRODUCT_DNF_DEPS).
install_deps() {
    local rootfs="$1" flavor="$2"
    shift 2
    case "${flavor}" in
        alpine) alpine_install_rootfs "${rootfs}" "$@" ;;
        al2023) al2023_install_rootfs "${rootfs}" "$@" ;;
        *) die "install_deps: unknown rootfs flavor '${flavor}'" ;;
    esac
}

# run_in_rootfs ROOTFS SCRIPT — run a self-contained build script inside the
# rootfs: mount the special filesystems, copy SCRIPT in, and chroot. All
# privileged steps run inside a single namespace (see ns_enter). This is the
# one entry point the product build scripts use for their build phase.
run_in_rootfs() {
    local rootfs="$1" script="$2"
    [[ -f "${rootfs}/.provisioned" ]] \
        || die "run_in_rootfs: ${rootfs} has no .provisioned marker; call ensure_rootfs first"
    ns_enter bash -c '
        set -euo pipefail
        # shellcheck source=tools/dist/chroot.sh
        source "${DIST_DIR}/chroot.sh"
        rootfs="$1" script="$2"
        [[ -f "${rootfs}/.provisioned" ]] \
            || die "run_in_rootfs: ${rootfs} has no .provisioned marker; call ensure_rootfs first"
        cp "${script}" "${rootfs}/build.sh"
        rootfs_mount_special "${rootfs}"
        chroot "${rootfs}" /bin/sh /build.sh
    ' _ "${rootfs}" "${script}"
}

# alpine_minirootfs_url resolves the newest minirootfs tarball in the pinned
# branch for the build arch. The listing is sorted with `sort -V` so the last
# match is the newest patch release of the branch. Returns the full URL.
alpine_minirootfs_url() {
    local listing arch base name
    arch="$( [[ "${HOST_ARCH}" == "amd64" ]] && echo x86_64 || echo aarch64 )"
    base="${ALPINE_MIRROR}/${ALPINE_BRANCH}/releases/${arch}"
    listing="$(curl -fsSL --max-time 30 "${base}/")" \
        || die "cannot list alpine ${ALPINE_BRANCH} ${arch} releases"
    name="$(printf '%s\n' "${listing}" \
        | grep -oE "alpine-minirootfs-${ALPINE_BRANCH#v}[0-9.]*-${arch}\.tar\.gz" \
        | sort -uV \
        | tail -1)" \
        || die "no alpine minirootfs found for ${ALPINE_BRANCH} ${arch}"
    [[ -n "${name}" ]] || die "no alpine minirootfs found for ${ALPINE_BRANCH} ${arch}"
    printf '%s/%s\n' "${base}" "${name}"
}

# alpine_setup_rootfs ROOTFS [PRODUCT] downloads + extracts the alpine
# minirootfs (without /dev device nodes), pins the apk repositories to the
# branch, and copies the host resolver so apk and builds can reach the
# network. Atomic (strimserver pattern): the rootfs is provisioned into
# $rootfs.new and mv'd into place only after the .provisioned sentinel is
# written, so an aborted run leaves no half-built rootfs at the final path and
# the next run resumes cleanly. A rootfs carrying .provisioned is reused; the
# cache dir name is content addressed (see ensure_rootfs), so a partial
# $rootfs.new is always stale and safely removed.
alpine_setup_rootfs() {
    local rootfs="$1" prod="${2:-${PRODUCT:-dist}}" tarball url

    if [[ -f "${rootfs}/.provisioned" ]]; then
        return 0   # cached rootfs for exactly this fingerprint; reuse
    fi

    url="$(alpine_minirootfs_url)"
    mkdir -p "${DIST_CACHE}"
    require_network "${url}"
    tarball="${DIST_CACHE}/$(basename "${url}")"
    if [[ ! -f "${tarball}" ]]; then
        download "${url}" "${tarball}"
    fi

    rm -rf "${rootfs}" "${rootfs}.new"
    mkdir -p "${rootfs}.new"
    ns_enter bash -c '
        set -euo pipefail
        # shellcheck source=tools/dist/chroot.sh
        source "${DIST_DIR}/chroot.sh"
        rootfs_extract "$1" "$2"
    ' _ "${rootfs}.new" "${tarball}"
    cp /etc/resolv.conf "${rootfs}.new/etc/resolv.conf"
    cat > "${rootfs}.new/etc/apk/repositories" <<EOF
${ALPINE_MIRROR}/${ALPINE_BRANCH}/main
${ALPINE_MIRROR}/${ALPINE_BRANCH}/community
EOF
    printf 'gitd: %s: alpine minirootfs %s (%s)\n' "${prod}" "$(basename "${tarball}")" "$(date -u +%Y-%m-%d)" \
        > "${rootfs}.new/.provisioned"
    mv "${rootfs}.new" "${rootfs}"
}

# al2023_setup_rootfs ROOTFS exports the AL2023 container image to a rootfs
# tarball via crane (a static binary downloaded to the cache), then extracts
# it without /dev device nodes. AL2023 has no minirootfs tarball; the ECR
# container image is the rootfs. The export tarball is keyed on the image ref
# so a changed AL2023_IMAGE exports fresh instead of reusing a stale base.
al2023_setup_rootfs() {
    local rootfs="$1" prod="${2:-${PRODUCT:-dist}}" tarball crane image_key

    if [[ -f "${rootfs}/.provisioned" ]]; then
        return 0
    fi

    require_cmd curl
    require_cmd tar
    mkdir -p "${DIST_CACHE}"
    crane="${DIST_CACHE}/crane"
    if [[ ! -x "${crane}" ]]; then
        download \
            "https://github.com/google/go-containerregistry/releases/download/v0.22.1/go-containerregistry_Linux_${HOST_ARCH}.tar.gz" \
            "${DIST_CACHE}/crane.tar.gz"
        tar -xzf "${DIST_CACHE}/crane.tar.gz" -C "${DIST_CACHE}" crane
        chmod +x "${crane}"
    fi

    require_network "https://public.ecr.aws"
    image_key="$(printf '%s' "${AL2023_IMAGE}" | sha256sum | cut -c1-8)"
    tarball="${DIST_CACHE}/al2023-rootfs-${HOST_ARCH}-${image_key}.tar"
    if [[ ! -f "${tarball}" ]]; then
        # crane defaults to linux/amd64; pin the export to the build arch.
        "${crane}" export --platform "linux/${HOST_ARCH}" "${AL2023_IMAGE}" "${tarball}" \
            || die "crane export of ${AL2023_IMAGE} failed"
    fi

    rm -rf "${rootfs}" "${rootfs}.new"
    mkdir -p "${rootfs}.new"
    ns_enter bash -c '
        set -euo pipefail
        # shellcheck source=tools/dist/chroot.sh
        source "${DIST_DIR}/chroot.sh"
        rootfs_extract "$1" "$2"
    ' _ "${rootfs}.new" "${tarball}"
    cp /etc/resolv.conf "${rootfs}.new/etc/resolv.conf"
    printf 'gitd: %s: al2023 container image %s\n' "${prod}" "${AL2023_IMAGE}" \
        > "${rootfs}.new/.provisioned"
    mv "${rootfs}.new" "${rootfs}"
}