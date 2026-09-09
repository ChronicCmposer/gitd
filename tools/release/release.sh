#!/usr/bin/env bash
# tools/release/release.sh — build a gitd release from an exact-tagged HEAD
# (R1-Q11/Q14, R6-Q3), and optionally publish the image to gitd-dist.
#
# Usage:
#   make release
#   tools/release/release.sh
#
# Core contract (always runs, always safe to re-run):
#   - HEAD must be exactly a release tag (git describe --tags --exact-match
#     succeeds) so the stamped binary embeds the exact tag. Run
#     `make bump-version LEVEL=<major|minor|patch>` first otherwise.
#   - build the stamped gitd binary,
#   - produce tools/dist/out/gitd-container.tar via the existing image flow,
#   - print the sha256 the operator uses as the out-of-band update pin (R6-Q3).
#
# Optional publish (only when GH_TOKEN is set): upload gitd-container.tar to
# the gitd-container release on ChronicCmposer/gitd-dist. A skipped or failed
# publish NEVER fails the build — the sha256 pin above is the deliverable, and
# publishing is belt-and-braces for the artifact channel.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

# Fail fast: HEAD must be exactly a release tag, else the binary would embed
# the dev fallback instead of a real version.
tag="$(git describe --tags --exact-match 2>/dev/null || true)"
if [[ -z "${tag}" ]]; then
    echo "gitd: release: HEAD is not exactly a release tag; run 'make bump-version LEVEL=<major|minor|patch>' first so the binary embeds the exact tag" >&2
    exit 1
fi
echo "gitd: release: HEAD is exactly ${tag}"

# Build the stamped gitd binary. stamp=True on the target + build --stamp
# (in .bazelrc; passed explicitly so the build is robust to rc overrides)
# makes rules_go substitute {STABLE_VERSION} from workspace-status.sh.
bazel build --stamp //cmd/gitd:gitd

# Produce gitd-container.tar (the existing image-container flow).
make image-container

tar="tools/dist/out/gitd-container.tar"
[[ -f "${tar}" ]] || { echo "gitd: release: image tarball not found at ${tar}; image-container did not produce it" >&2; exit 1; }
sha256="$(sha256sum "${tar}" | awk '{print $1}')"
echo "gitd: release: ${tag}: build OK"
echo "gitd: release: update pin (out-of-band, R6-Q3): ${sha256}"

# Optional publish. Never fail the build over a skipped or failed publish.
if [[ -z "${GH_TOKEN:-}" ]]; then
    echo "gitd: release: GH_TOKEN not set; skipped publish (build + sha256 are the core). Set GH_TOKEN to publish gitd-container.tar to gitd-dist."
elif ! command -v gh >/dev/null 2>&1; then
    echo "gitd: release: gh not found in PATH; skipped publish (sha256 above is still the update pin)" >&2
else
    if gh release view gitd-container --repo ChronicCmposer/gitd-dist >/dev/null 2>&1; then
        gh release upload gitd-container "${tar}" --repo ChronicCmposer/gitd-dist --clobber \
            || echo "gitd: release: WARNING: publish upload failed; sha256 above is still the update pin" >&2
    else
        gh release create gitd-container "${tar}" --repo ChronicCmposer/gitd-dist \
            --title "gitd ${tag}" \
            --notes "gitd-container.tar built from ${tag}. Update pin sha256: ${sha256}" \
            || echo "gitd: release: WARNING: publish create failed; sha256 above is still the update pin" >&2
    fi
fi

echo "gitd: release: ${tag} done"
