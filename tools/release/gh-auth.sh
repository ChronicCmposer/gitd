# tools/release/gh-auth.sh — shared gh CLI authentication guards.
#
# Publishing to GitHub Releases is authenticated through the `gh` CLI, which
# reads its own stored credentials (gh auth login) rather than requiring a
# GH_TOKEN env var. If GH_TOKEN is set, `gh` naturally uses it as an override,
# so the primary documented path is `gh auth login`.
#
# gh_auth() is a pure predicate (exit 0 = gh installed and authenticated);
# require_gh_auth() fails fast for callers that must not proceed without auth.

# gh_auth: exit 0 iff gh is installed and authenticated (token valid).
gh_auth() {
    command -v gh >/dev/null 2>&1 || return 1
    gh auth status >/dev/null 2>&1
}

# require_gh_auth: fail fast (via the caller's die) when gh is missing or not
# authenticated. A publish with no auth is a loud failure, never a silent skip.
# The optional first arg names the die function (defaults to "die", which every
# sourcing script defines).
require_gh_auth() {
    local die_fn="${1:-die}"
    gh_auth || "$die_fn" "gh is required and must be authenticated (run 'gh auth login') to publish to GitHub"
}
