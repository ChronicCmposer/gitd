#!/usr/bin/env bash
# gitd: statement coverage floor (Phase 9, R4-Q4).
#
# Runs `go test -cover` per internal package against the pinned rules_go SDK
# and enforces the >=80% statement-coverage floor. Thin-wrapper packages are
# listed in tools/coverage/exceptions.txt (one package path per line, # for
# comments) with a rationale; they are reported but not gated.
#
# Usage: coverage.sh            (checks the floor; exit 1 when any package
#                                drops below it)
#        coverage.sh --report   (print per-package coverage, no gate)
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

MODE="${1:-gate}"
EXCEPTIONS_FILE="${EXCEPTIONS_FILE:-$ROOT/tools/coverage/exceptions.txt}"
MIN_COVERAGE="${MIN_COVERAGE:-80}"
FLOOR="$MIN_COVERAGE" # percent, per R4-Q4

# --- load exceptions ----------------------------------------------------------
declare -A EXCEPTED=()
if [[ -f "$EXCEPTIONS_FILE" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
        # strip comments and blank lines
        line="${line%%#*}"
        line="$(echo "$line" | xargs)"
        [[ -z "$line" ]] && continue
        EXCEPTED["$line"]=1
    done < "$EXCEPTIONS_FILE"
fi

# --- pinned rules_go SDK go ---------------------------------------------------
OUTPUT_BASE="$(bazel info output_base)"
GO_BIN="$OUTPUT_BASE/external/rules_go++go_sdk+go_sdk/bin/go"
if [[ ! -x "$GO_BIN" ]]; then
    bazel build @io_bazel_rules_go//go >/dev/null 2>&1 || true
    GO_BIN="$OUTPUT_BASE/external/rules_go++go_sdk+go_sdk/bin/go"
fi

# --- measure ---------------------------------------------------------------
# `go test -cover ./internal/...` emits one "coverage: N% of statements" line
# per package that has tests, and a bare package line for test-less packages
# (0.0%). We parse those lines into pkg -> pct.
raw="$("$GO_BIN" test -cover ./internal/... 2>/dev/null)"

# Track failures. The pipe to grep -E can mask go's own exit code, so the
# per-line parse is the source of truth; a missing line for a package counts
# as 0.0%.
failures=0
printf '%-70s %8s  %s\n' "package" "coverage" "status"
while IFS= read -r line; do
    case "$line" in
        *"coverage: "*)
            # e.g. "ok  github.com/.../internal/event 0.003s  coverage: 94.4% of statements"
            pkg="${line#*github.com/ChronicCmposer/gitd/}"
            pkg="${pkg%%	*}"
            pct="${line##*coverage: }"
            pct="${pct%%%*}"
            pct="$(echo "$pct" | xargs)"
            ;;
        *)
            # e.g. "github.com/.../internal/gitenv  coverage: 0.0% of statements" (no tests)
            pkg="${line#*github.com/ChronicCmposer/gitd/}"
            pkg="${pkg%%	*}"
            pct="0.0"
            ;;
    esac
    [[ -z "$pkg" ]] && continue

    pkg_path="./$pkg"
    status="OK"
    if awk -v p="$pct" -v f="$FLOOR" 'BEGIN { exit !(p >= f) }'; then
        : # meets floor
    else
        if [[ -n "${EXCEPTED[$pkg_path]:-}" ]]; then
            status="EXCEPTED"
        else
            status="BELOW"
            failures=$((failures + 1))
        fi
    fi
    printf '%-70s %7.1f%%  %s\n' "$pkg_path" "$pct" "$status"
done <<< "$raw"

echo
echo "floor: >=${FLOOR}% statements per internal package (exceptions: $EXCEPTIONS_FILE)"

if [[ "$MODE" == "--report" ]]; then
    exit 0
fi

if (( failures > 0 )); then
    echo "gitd: coverage: $failures package(s) below ${FLOOR}% — add tests (strengthen, never weaken) or move a thin wrapper to exceptions.txt" >&2
    exit 1
fi
echo "gitd: coverage: all internal packages meet the ${FLOOR}% floor"