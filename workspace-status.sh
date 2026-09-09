#!/usr/bin/env bash
# workspace-status.sh — Bazel link-time version stamping source (R1-Q11/Q14).
#
# Emits the stable workspace-status keys Bazel injects into stamped binaries
# via x_defs. STABLE_VERSION is the exact release tag at HEAD when one exists
# (git describe --tags --exact-match); otherwise the v0.0.0-devel dev fallback.
#
# Git tags are the source of truth for versioning, and the version is resolved
# HERE at link time — never generated into source. This is the single place the
# embedded version string is computed, so the stamped gitd binary always
# reports exactly what was checked out (a release tag, or the dev fallback).

set -euo pipefail

echo "STABLE_VERSION $(git describe --tags --exact-match 2>/dev/null || echo v0.0.0-devel)"
