#!/usr/bin/env bash
# tools/release/bump-version.sh — tag the next release version (R1-Q11/Q14).
#
# Usage:
#   tools/release/bump-version.sh <major|minor|patch>
#   make bump-version LEVEL=<major|minor|patch>
#
# Git tags are the source of truth for versioning. The script reads the latest
# reachable tag (defaulting to v0.0.0 when none exists), computes the next
# vX.Y.Z for the requested LEVEL, creates an annotated release tag, and pushes
# it to origin.
#
# Fail-fast and atomic:
#   - LEVEL is required and validated up front (major|minor|patch).
#   - the working tree must be clean — a tag must never describe uncommitted work.
#   - the current tag must parse as vX.Y.Z; the next tag must not already exist.
#   - the tag is created only after every check passes, and if the push fails
#     the local tag is rolled back, so no partial release state is left behind.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

LEVEL="${1:-}"
case "${LEVEL}" in
    major | minor | patch) ;;
    *)
        echo "gitd: bump-version: LEVEL must be major, minor, or patch (got '${LEVEL}'); usage: make bump-version LEVEL=<major|minor|patch>" >&2
        exit 1
        ;;
esac

# Fail fast: a dirty tree would tag a snapshot that matches no committed state.
if [[ -n "$(git status --porcelain)" ]]; then
    echo "gitd: bump-version: working tree is not clean; commit or stash before tagging" >&2
    exit 1
fi

# Current version = latest reachable tag, defaulting to v0.0.0.
current="$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)"
if [[ ! "${current}" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    echo "gitd: bump-version: current tag '${current}' is not vX.Y.Z; refusing to bump" >&2
    exit 1
fi
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"

case "${LEVEL}" in
    major) major=$((10#${major} + 1)) ; minor=0    ; patch=0 ;;
    minor)                          minor=$((10#${minor} + 1)) ; patch=0 ;;
    patch)                                                   patch=$((10#${patch} + 1)) ;;
esac

next="v${major}.${minor}.${patch}"

# Fail fast: the computed tag must not already exist.
if git rev-parse -q --verify "refs/tags/${next}" >/dev/null; then
    echo "gitd: bump-version: tag ${next} already exists; nothing to do" >&2
    exit 1
fi

git tag -a "${next}" -m "gitd ${next}"
echo "gitd: bump-version: tagged ${next}"

# Push the tag. On failure, roll back the local tag so no partial release
# state is left behind (fail-fast + atomic).
if ! git push origin "${next}"; then
    git tag -d "${next}" >/dev/null
    echo "gitd: bump-version: push of ${next} failed; local tag rolled back (git tag -d ${next}); no partial release" >&2
    exit 1
fi

echo "gitd: bump-version: ${next} pushed to origin"
