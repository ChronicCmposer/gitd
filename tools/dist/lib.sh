# tools/dist/lib.sh — shared fail-fast helpers for the dist pipeline.
#
# Every script in this directory sources this file. The laws that hold here:
#   - set -euo pipefail everywhere; an unset variable or failed command halts.
#   - every failure is loud: die() prints "gitd: <script>: <reason>" to stderr.
#   - prerequisites are checked up front (early exit), never mid-build.
#   - the deterministic tarball helper makes output byte-identical across runs
#     (SOURCE_DATE_EPOCH, sorted names, numeric owners, gzip -n).

set -euo pipefail

# The directory this file lives in (tools/dist); used to find sibling files.
DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/versions.sh
source "${DIST_DIR}/versions.sh"

# die prints a fail-fast error to stderr and exits 1. Call as: die "reason"
die() {
    echo "gitd: ${PRODUCT:-dist}: $*" >&2
    exit 1
}

# require_cmd fails fast when a required executable is missing.
require_cmd() {
    local cmd="$1"
    command -v "$cmd" >/dev/null 2>&1 \
        || die "required command '${cmd}' not found in PATH"
}

# require_root fails fast because chroot/apk installs need uid 0.
require_root() {
    [[ "$(id -u)" -eq 0 ]] || die "must run as root (chroot builds need uid 0); use sudo"
}

# require_network fails fast with a readable message when the mirror is down.
require_network() {
    local url="$1"
    curl -fsSI --max-time 15 "$url" >/dev/null 2>&1 \
        || die "cannot reach ${url}; the dist pipeline needs network access"
}

# require_empty_dir fails fast when the output directory already holds a build
# (deterministic builds never resume or overwrite; rebuild from scratch).
require_empty_dir() {
    local dir="$1"
    [[ -d "$dir" && -n "$(ls -A "$dir" 2>/dev/null)" ]] \
        && die "refusing to reuse non-empty build dir '${dir}'; remove it and rebuild"
}

# download verifies a pinned sha256 when one is set ("" = skip verification).
download() {
    local url="$1" dest="$2" sha256="${3:-}"
    curl -fsSL --max-time 300 "$url" -o "$dest" \
        || die "failed to download ${url}"
    if [[ -n "$sha256" ]]; then
        local actual
        actual="$(sha256sum "$dest" | awk '{print $1}')"
        [[ "$actual" == "$sha256" ]] \
            || die "sha256 mismatch for ${dest}: expected ${sha256}, got ${actual}"
    fi
}

# tar_reproducible archives a directory with every timestamp/ownership bit
# pinned, so two builds produce byte-identical tarballs. GNU tar strips the
# leading "./" from paths by default; gzip -n drops the gzip header timestamp.
tar_reproducible() {
    local out="$1" dir="$2"
    tar \
        --sort=name \
        --mtime="@${SOURCE_DATE_EPOCH}" \
        --owner=0 --group=0 --numeric-owner \
        --format=gnu \
        --use-compress-program='gzip -n' \
        -C "$dir" \
        -cf "$out" \
        .
}

# sha256_of prints the checksum of a file.
sha256_of() {
    sha256sum "$1" | awk '{print $1}'
}

# stage_mkdirs creates the fixed directory skeleton inside a staging root.
stage_mkdirs() {
    local root="$1"
    mkdir -p "$root"/{usr/local/bin,usr/local/libexec,usr/local/share,etc,var}
}

# arch_go maps the HOST_ARCH to the Go GOARCH spelling (they already match).
go_arch() { echo "${HOST_ARCH}"; }

# product_asset_name builds the deterministic artifact filename, e.g.
# openssh-10.5p1.linux-arm64.tar.gz
product_asset_name() {
    local product="$1" version="$2"
    echo "${product}-${version}.linux-${HOST_ARCH}.tar.gz"
}