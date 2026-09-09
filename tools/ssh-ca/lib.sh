#!/usr/bin/env bash
# tools/ssh-ca/lib.sh — shared fail-fast helpers for the gitd SSH CA tool.
#
# The laws that hold here (code-philosophy): set -euo pipefail everywhere;
# die() is the single fail-fast, fail-loud exit; prerequisites are checked up
# front (early exit); cert lifetimes and key types are constants so an invalid
# invocation is impossible rather than merely discouraged (parse, don't
# validate).
#
# CA custody is local-only per R3-Q4/R12-Q7: the CA keypair lives under
# ~/.ssh/gitd-ca/ (0600, gitignored) and never leaves this box.

set -euo pipefail

# Where this script tree lives; used to locate sibling files.
SSHCA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Hostname whose identity every host cert must claim (R10-Q10: principals =
# hostname only, EIP explicitly rejected).
GITD_HOST="git.cmposer.cc"

# Cert lifetimes (plan decision table): SSH user 90d, host 1y. These are the
# relative-time strings OpenSSH's -V accepts; the human labels below are for
# messages only (1y is spelled 365d because -V rejects +1y).
USER_CERT_VALIDITY="90d"
USER_CERT_LABEL="90d"
HOST_CERT_VALIDITY="365d"
HOST_CERT_LABEL="1y"

# Slight backdate so a just-issued cert is valid despite client/server clock
# skew. Validity windows are expressed relative to this backdate.
VALIDITY_BACKDATE="-5m"

# --- CA custody ---------------------------------------------------------------

# ca_dir returns the CA home directory (~/.ssh/gitd-ca). Fail-fast when HOME
# is unset.
ca_dir() {
    [[ -n "${HOME:-}" ]] || die "HOME is unset; cannot locate ~/.ssh/gitd-ca"
    printf '%s/.ssh/gitd-ca\n' "${HOME}"
}

# require_ca fails fast unless the SSH CA keypair already exists (init creates
# it; everything else consumes it).
require_ca() {
    local dir ca ca_pub
    dir="$(ca_dir)"
    ca="${dir}/ssh-ca"
    ca_pub="${ca}.pub"
    [[ -f "${ca}" && -f "${ca_pub}" ]] \
        || die "SSH CA not initialised; run 'ssh-ca init' first (expected ${ca})"
}

# enforce_keytype fails fast unless the given public-key file is an Ed25519
# key (auth keys and host keys are Ed25519 only, R13-Q3 / R5-Q5).
# ssh-keygen -lf reports the type in parentheses at the end of the fingerprint
# line, e.g. "... comment (ED25519)".
enforce_keytype() {
    local pub="$1"
    require_cmd ssh-keygen
    local fingerprint
    fingerprint="$(ssh-keygen -lf "${pub}")"
    case "${fingerprint}" in
        *"(ED25519)") return 0 ;;
        *) die "key '${pub}' is not Ed25519 (got: $(keytype_name "${fingerprint}")); Ed25519 is the only permitted key type" ;;
    esac
}

# keytype_name extracts the key type from a "ssh-keygen -lf" fingerprint line,
# e.g. "... comment (ED25519)" -> "ED25519".
keytype_name() {
    local fp="$1"
    fp="${fp##* (}"
    fp="${fp%)}"
    printf '%s' "${fp}"
}

# --- common helpers -----------------------------------------------------------

# die prints a fail-fast error to stderr and exits 1. Call as: die "reason".
die() {
    echo "gitd: ssh-ca: $*" >&2
    exit 1
}

# require_cmd fails fast when a required executable is missing.
require_cmd() {
    local cmd="$1"
    command -v "$cmd" >/dev/null 2>&1 \
        || die "required command '${cmd}' not found in PATH"
}

# chmod_key enforces 0600 on a private key so no other user can read it.
chmod_key() {
    local path="$1"
    chmod 0600 "${path}"
}
