#!/usr/bin/env bash
# gitd: full baseline-aware mutation run (Phase 9, R4-Q1/R4-Q3).
#
# Runs the pinned go-mutesting v2.7.9 binary (hermetically built by Bazel at
# //tools/mutesting) over every internal/ package with:
#   --coverage       covered-code MSI (uncovered mutants reported separately)
#   --baseline       known-surviving mutants; survivors in the baseline do not
#                    fail the run, NEW survivors do (R4-Q3: strengthen tests
#                    first, never weaken; equivalent mutants -> baseline)
#   --fail-on-escaped  exit 4 when a NEW mutant escapes
#
# The tool execs `go test` subprocesses, so the pinned rules_go SDK go binary
# is put on PATH first (R3-Q8: everything runs against the pinned toolchain).
# Reports (go-mutesting-*.json) are written to the repo root by the tool, then
# moved to $MUTATE_REPORTS_DIR (default gitd-mutation-reports/).
#
# Exit codes: 0 = pass, 4 = MSI/new-survivor gate failed, other = tool error.
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# --- locate the hermetic binary + pinned SDK go ------------------------------
BAZEL="${BAZEL:-bazel}"
MUTESTING="$(bazel cquery --output=files //tools/mutesting 2>/dev/null | head -1)"
if [[ -z "$MUTESTING" || ! -f "$MUTESTING" ]]; then
    "$BAZEL" build //tools/mutesting >&2
    MUTESTING="$(bazel cquery --output=files //tools/mutesting 2>/dev/null | head -1)"
fi
if [[ -z "$MUTESTING" || ! -f "$MUTESTING" ]]; then
    echo "gitd: mutate: could not locate the go-mutesting binary" >&2
    exit 1
fi

OUTPUT_BASE="$(bazel info output_base)"
SDK_GO_DIR="$OUTPUT_BASE/external/rules_go++go_sdk+go_sdk/bin"
export PATH="$SDK_GO_DIR:$PATH"

# --- scope: every internal package, cmd/ excluded (R4-Q2) --------------------
SCOPE="${SCOPE:-./internal/...}"

# --- baseline + flags --------------------------------------------------------
BASELINE="${BASELINE:-$ROOT/tools/mutation/baseline.json}"
EXTRA_FLAGS=(
    --coverage
    --quiet
    --no-diffs
    --baseline="$BASELINE"
    --fail-on-escaped
    --logger-summary-json
)
# shellcheck disable=SC2206
EXTRA_FLAGS+=(${MUTATE_FLAGS:-})

echo "gitd: mutate: running go-mutesting over $SCOPE (baseline: $BASELINE)" >&2
# shellcheck disable=SC2086
"$MUTESTING" "${EXTRA_FLAGS[@]}" $SCOPE
tool_code=$?

# --- collect artifacts (run even when the gate fails) ------------------------
REPORTS_DIR="${MUTATE_REPORTS_DIR:-$ROOT/gitd-mutation-reports}"
mkdir -p "$REPORTS_DIR"
for f in go-mutesting-report.json go-mutesting-summary.json go-mutesting-agentic.json; do
    if [[ -f "$f" ]]; then
        mv "$f" "$REPORTS_DIR/"
    fi
done
echo "gitd: mutate: reports in $REPORTS_DIR" >&2
if [[ -f "$REPORTS_DIR/go-mutesting-summary.json" ]]; then
    echo "gitd: mutate: MSI summary:" >&2
    cat "$REPORTS_DIR/go-mutesting-summary.json" >&2
    echo >&2
fi

exit "$tool_code"