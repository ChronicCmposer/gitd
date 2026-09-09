#!/usr/bin/env bash
# tools/release/workspace-status.sh — pointer to the canonical root script.
#
# Bazel's --workspace_status_command normalizes a leading "./" away from
# relative paths (so "./workspace-status.sh" becomes the PATH lookup
# "workspace-status.sh" and is not found). A nested path without "." segments
# survives, so this thin pointer hands off to the canonical workspace-status.sh
# at the repo root — the single source of the STABLE_VERSION stamp (R1-Q11).

exec "$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/workspace-status.sh"