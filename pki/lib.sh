#!/usr/bin/env bash
# pki/lib.sh — shared fail-fast helpers for the gitd TLS PKI tooling.
#
# Mirrors tools/ssh-ca/lib.sh: set -euo pipefail everywhere, die() as the
# single fail-fast exit, prerequisites checked up front. TLS CA custody is
# local-only per R3-Q4/R12-Q7 — CA keys live under ~/.ssh/gitd-ca/ (0600,
# gitignored) and never leave this box.

set -euo pipefail

# TLS cert lifetimes (plan decision table): server 90d, client 30d, CA 10y.
TLS_SERVER_DAYS=90
TLS_CLIENT_DAYS=30
TLS_CA_DAYS=3650

# The hostname the server cert claims (CN + subjectAltName).
GITD_HOST="git.cmposer.cc"

# --- CA custody ---------------------------------------------------------------

# ca_dir returns the CA home directory (~/.ssh/gitd-ca). Fail-fast when HOME
# is unset.
ca_dir() {
    [[ -n "${HOME:-}" ]] || die "HOME is unset; cannot locate ~/.ssh/gitd-ca"
    printf '%s/.ssh/gitd-ca\n' "${HOME}"
}

# require_tls_ca fails fast unless the TLS CA key + cert already exist.
require_tls_ca() {
    local dir
    dir="$(ca_dir)"
    [[ -f "${dir}/tls-ca.key" && -f "${dir}/tls-ca.crt" ]] \
        || die "TLS CA not initialised; run pki/pki-new.sh first (expected ${dir}/tls-ca.{key,crt})"
}

# --- common helpers -----------------------------------------------------------

# die prints a fail-fast error to stderr and exits 1. Call as: die "reason".
die() {
    echo "gitd: pki: $*" >&2
    exit 1
}

# require_cmd fails fast when a required executable is missing.
require_cmd() {
    local cmd="$1"
    command -v "$cmd" >/dev/null 2>&1 \
        || die "required command '${cmd}' not found in PATH"
}
