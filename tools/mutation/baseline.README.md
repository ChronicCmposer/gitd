# gitd mutation baseline (Phase 9, R4-Q3)

This directory holds the committed **baseline of known-surviving mutants** for
the pinned `jonbaldie/go-mutesting/v2` v2.7.9 binary (`//tools/mutesting`).

`baseline.json` currently contains an **empty** mutant set:

```json
{ "version": 1, "mutants": [] }
```

An empty baseline is the *correct starting state* for a fresh repo: it means
**every** escaped mutant is treated as NEW and fails the PR gate
(`make mutate-ci` / `--fail-on-escaped`). Nothing has been triaged yet, so
there is nothing to excuse. As the full nightly run (`make mutate`) surfaces
survivors, you triage them and promote the acceptable ones here.

## Schema (from `internal/baseline/baseline.go` v2.7.9)

```json
{
  "version": 1,
  "mutants": [
    {
      "id":      "<md5 hex, see MutantID>",
      "file":    "internal/event/event.go",
      "mutator": "statement/return",
      "line":    42
    }
  ]
}
```

- `id` is the stable `MutantID(relFile, mutatorName, diff)` MD5 — it deliberately
  excludes line numbers so it survives refactors that only shift surrounding code.
- `file` is the repo-relative path (slash-normalized).
- `mutator` is the mutator name (e.g. `conditional/negated`, `statement/return`,
  `branch/case`, `numbers/incrementer`, `expression/...`).
- `line` is the original start line (informational; the `id` is what matters).

## How to populate it (triage discipline, R4-Q3)

The rule is **strengthen tests first, never weaken**; the baseline only records
mutants that are *equivalent* (semantically identical to the original) or
otherwise genuinely unavoidable after you have added every reasonable test.

1. **Run the full mutation suite** and read the report:

   ```sh
   make mutate            # writes gitd-mutation-reports/go-mutesting-report.json
   ```

2. **Read each survivor** in `go-mutesting-report.json` (or the human-readable
   `go-mutesting-agentic.json`, which is designed for LLM/manual review). For
   each escaped mutant ask: *can I add a test that kills this?*
   - If yes → **add the test** (strengthen, never weaken). Do NOT put it in the
     baseline.
   - If the mutant is **equivalent** (e.g. `numbers/incrementer` on a loop that
     is dead by construction, a `conditional/negated` where both branches are
     provably identical) → it belongs in the baseline.
   - If the code is genuinely untestable as written (no injection seam) → fix
     the seam rather than excusing it; only as a last resort baseline it.

3. **Write only the accepted set.** The tool can generate the file for you:

   ```sh
   bazel run --@io_bazel_rules_go//go/config:pure //tools/mutesting \
     -- --update-baseline --baseline=$PWD/tools/mutation/baseline.json \
     --coverage ./internal/...
   ```

   This rewrites `baseline.json` to contain **all** current survivors. **Do NOT
   blindly commit that output** — edit it down to only the mutants you triaged
   as equivalent, then re-run `make mutate` to confirm the gate now passes with
   `--fail-on-escaped`.

4. **Commit** the edited baseline with a message explaining each accepted
   survivor, e.g.:

   ```
   mutation: baseline equivalent mutants in internal/event (dead loop incrementer)
   ```

5. **Keep it small and reviewed.** Every baseline entry is a test you chose not
   to write; treat additions like a coverage exception — they need a rationale.

## Why not a comment in the JSON?

JSON does not allow comments, so the triage guidance lives here. The scripts
(`mutate.sh` / `mutate-ci.sh`) also carry the invariant in their header comments:
**survivors in the baseline pass; NEW survivors fail the run** (`--fail-on-escaped`,
exit 4).
