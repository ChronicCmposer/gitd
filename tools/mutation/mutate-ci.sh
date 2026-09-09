#!/usr/bin/env bash
# gitd: CI mutation gate — git-diff mode, zero NEW survivors (Phase 9, R4-Q3).
#
# Runs the pinned go-mutesting binary in --git-diff-lines mode: only lines
# changed since the merge-base of <base> and HEAD are mutated (the tool diffs
# against the merge-base, so a PR reports exactly the branch's changes). Scope
# is the changed internal/ packages only (cmd/ excluded, R4-Q2).
#
# Gate policy (R4-Q3): --fail-on-escaped exits 4 when a survivor is NEW
# relative to the committed baseline. Baseline-known survivors (equivalent
# mutants after triage) pass. --ignore-msi-with-no-mutations exits 0 when no
# mutations were generated (e.g. an internal/ change that only touched
# comments or a package with no tests) — a clean exit for a clean PR.
#
# Usage: mutate-ci.sh [BASE]   (BASE defaults to origin/main)
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

BASE="${1:-${MUTATE_CI_BASE:-origin/main}}"

if ! git rev-parse --verify -q "$BASE" >/dev/null 2>&1; then
    echo "gitd: mutate-ci: base ref '$BASE' not found" >&2
    exit 1
fi

# --- locate the hermetic binary + pinned SDK go ------------------------------
MUTESTING="$(bazel cquery --output=files //tools/mutesting 2>/dev/null | head -1)"
if [[ -z "$MUTESTING" || ! -f "$MUTESTING" ]]; then
    bazel build //tools/mutesting >&2
    MUTESTING="$(bazel cquery --output=files //tools/mutesting 2>/dev/null | head -1)"
fi
if [[ -z "$MUTESTING" || ! -f "$MUTESTING" ]]; then
    echo "gitd: mutate-ci: could not locate the go-mutesting binary" >&2
    exit 1
fi

OUTPUT_BASE="$(bazel info output_base)"
SDK_GO_DIR="$OUTPUT_BASE/external/rules_go++go_sdk+go_sdk/bin"
export PATH="$SDK_GO_DIR:$PATH"

# --- changed internal/ packages since BASE (cmd/ excluded, R4-Q2) ------------
# Map changed files under internal/ to their package directories, dedupe.
changed_pkgs="$(
    git diff --name-only "$(git merge-base "$BASE" HEAD 2>/dev/null || echo "$BASE")" HEAD \
        | sed -n 's#^internal/\([^/]*\)/.*#./internal/\1#p' \
        | sort -u
)"
if [[ -z "$changed_pkgs" ]]; then
    echo "gitd: mutate-ci: no internal/ packages changed since $BASE; nothing to gate" >&2
    exit 0
fi

BASELINE="${BASELINE:-$ROOT/tools/mutation/baseline.json}"
EXTRA_FLAGS=(
    --git-diff-lines
    --git-diff-base="$BASE"
    --coverage
    --quiet
    --no-diffs
    --baseline="$BASELINE"
    --fail-on-escaped
    --ignore-msi-with-no-mutations
    --logger-summary-json
)
# shellcheck disable=SC2206
EXTRA_FLAGS+=(${MUTATE_FLAGS:-})

echo "gitd: mutate-ci: base=$BASE changed packages:" >&2
echo "$changed_pkgs" >&2
# shellcheck disable=SC2086
"$MUTESTING" "${EXTRA_FLAGS[@]}" $changed_pkgs
tool_code=$?

REPORTS_DIR="${MUTATE_REPORTS_DIR:-$ROOT/gitd-mutation-reports}"
mkdir -p "$REPORTS_DIR"
for f in go-mutesting-report.json go-mutesting-summary.json go-mutesting-agentic.json; do
    if [[ -f "$f" ]]; then
        mv "$f" "$REPORTS_DIR/"
    fi
done
echo "gitd: mutate-ci: reports in $REPORTS_DIR" >&2

exit "$tool_code"