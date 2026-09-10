#!/usr/bin/env bash
# tools/dist/package-image.sh — package the rules_oci image for ctr import.
#
# Usage:
#   bazel build //image:image
#   tools/dist/package-image.sh [OUT_FILE]   # default tools/dist/out/gitd-container.tar
#
# containerd's `ctr images import` accepts an OCI-layout tar (the directory
# produced by //image:image contains oci-layout, index.json, blobs/). This
# script tars that directory deterministically (sorted names, pinned mtimes,
# numeric owners) so the publish pipeline and Phase 7 userdata consume one
# stable artifact: gitd-container.tar (R3-Q2).
#
# The OCI index carries NO ref annotation (rules_oci omits it), so before
# tarring we bake org.opencontainers.image.ref.name=latest into the manifest
# descriptor in index.json. containerd's `ctr images import --base-name
# git.cmposer.cc/gitd` then registers git.cmposer.cc/gitd:latest; without the
# annotation the archive has no tag for --base-name to attach and the import
# fails ("no image name found").

set -euo pipefail

DIST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/dist/lib.sh
source "${DIST_DIR}/lib.sh"

PRODUCT="image"
OUT_FILE="${1:-${DIST_DIR}/out/gitd-container.tar}"

require_cmd python3

# Locate the built image directory from the Bazel output base.
image_dir="$(bazel info bazel-bin 2>/dev/null)/image/image"
[[ -f "${image_dir}/oci-layout" ]] \
    || die "OCI layout not found at ${image_dir}; run: bazel build //image:image"

# Stage the OCI layout so the ref annotation can be injected without mutating
# the Bazel output tree. The layer blobs stay as symlinks into the bazel-bin
# tree (blobs/sha256/<digest> -> ../../../<layer>.tar); re-point them by
# absolute path so tar --dereference reads the CONTENT without copying
# megabytes into the staging dir.
stage_dir="$(mktemp -d "${TMPDIR:-/tmp}/gitd-image.XXXXXX")"
# The staged tree is chmod'ed read-only below so tar records the same entry
# modes as the source layout; the trap restores write permission first.
trap 'chmod -R u+w "${stage_dir}" 2>/dev/null || true; rm -rf "${stage_dir}"' EXIT
mkdir -p "${stage_dir}/blobs/sha256"
cp -a "${image_dir}/index.json" "${image_dir}/oci-layout" "${stage_dir}/"
for blob in "${image_dir}"/blobs/sha256/*; do
    name="$(basename "${blob}")"
    if [[ -L "${blob}" ]]; then
        ln -s "$(readlink -f "${blob}")" "${stage_dir}/blobs/sha256/${name}"
    else
        # config + manifest blobs are regular files in the layout, not links.
        cp -a "${blob}" "${stage_dir}/blobs/sha256/${name}"
    fi
done

# Bake the OCI ref name (R3-Q2): org.opencontainers.image.ref.name=latest on
# the manifest descriptor. Per the OCI spec this annotation is only valid on
# descriptors in index.json, and containerd's importer reads it from the
# descriptor (not the index top level). Deterministic: parse + re-emit with
# 2-space indent (input order preserved; rules_oci's emit order is fixed).
chmod u+w "${stage_dir}/index.json"
python3 - "${stage_dir}/index.json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as f:
    index = json.load(f)
manifests = index.get("manifests", [])
if len(manifests) != 1:
    raise SystemExit(
        f"expected exactly one manifest in {path}, found {len(manifests)}; "
        "single-tag image contract (git.cmposer.cc/gitd:latest)"
    )
manifests[0].setdefault("annotations", {})[
    "org.opencontainers.image.ref.name"
] = "latest"
with open(path, "w", encoding="utf-8") as f:
    json.dump(index, f, indent=2)
    f.write("\n")
PY
chmod 0555 "${stage_dir}/index.json" "${stage_dir}" \
    "${stage_dir}/blobs" "${stage_dir}/blobs/sha256"

mkdir -p "$(dirname "${OUT_FILE}")"
# rules_oci's oci_image output references the layer blobs as SYMLINKS into the
# bazel-bin tree (blobs/sha256/<digest> -> ../../../<layer>.tar). The packaged
# tar must carry the blob CONTENTS (--dereference), not dangling links, or
# `ctr images import` cannot read the layers. Dereferencing stays deterministic:
# the layer tars are byte-deterministic and every entry mtime is pinned below.
tar \
    --sort=name \
    --mtime="@${SOURCE_DATE_EPOCH}" \
    --owner=0 --group=0 --numeric-owner \
    --format=gnu \
    --dereference \
    -C "${stage_dir}" \
    -cf "${OUT_FILE}" \
    .

echo "gitd: image: ${OUT_FILE}"
echo "gitd: image: sha256 $(sha256_of "${OUT_FILE}")"