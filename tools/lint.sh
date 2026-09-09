#!/usr/bin/env bash
# gitd: golangci-lint gate (Design Conventions — lint enforced in CI).
#
# golangci-lint is a standalone binary and is NOT a Go module dependency: it
# is pinned at a known-good v2 version and either (a) invoked from PATH when
# installed, or (b) fetched on demand via `go run ...@<pin>` (dev-only tool,
# never added to go.mod). The scan runs against the pinned rules_go SDK go
# binary so the analyzer matches the Go toolchain we ship.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Pinned golangci-lint v2 version (semver tag on github.com/golangci/golangci-lint).
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.13.2}"

OUTPUT_BASE="$(bazel info output_base)"
GO_BIN="$OUTPUT_BASE/external/rules_go++go_sdk+go_sdk/bin/go"
if [[ ! -x "$GO_BIN" ]]; then
    bazel build @io_bazel_rules_go//go >/dev/null 2>&1 || true
    GO_BIN="$OUTPUT_BASE/external/rules_go++go_sdk+go_sdk/bin/go"
fi

echo "gitd: golangci-lint $GOLANGCI_LINT_VERSION"
if command -v golangci-lint >/dev/null 2>&1 && golangci-lint version 2>/dev/null | grep -qi "v2"; then
    golangci-lint run ./...
else
    echo "gitd: golangci-lint not installed; fetching pinned $GOLANGCI_LINT_VERSION"
    "$GO_BIN" run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VERSION" run ./...
fi
