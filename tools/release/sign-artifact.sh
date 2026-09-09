#!/usr/bin/env bash
# tools/release/sign-artifact.sh — shared GPG detached-sign / verify helpers.
#
# Sourced (not executed) by release.sh, publish-dist.sh, deploy.sh, update.sh,
# and packaged into the deployment bundle for userdata.sh, so the whole artifact
# signing flow is ONE implementation. Two laws hold here:
#   - sign_artifact   (build / operator side): write <file>.asc with the
#     operator's signing key, resolved in precedence order:
#       1. GPG_KEY_ID (explicit env override wins)
#       2. git config user.signingkey (repo-local -> global -> system)
#       3. gpg's default key (safe fallback)
#   - verify_artifact (consumer / host side): accept ONLY a pinned public key,
#     imported into a throwaway GNUPGHOME, so provenance is proven against the
#     committed key and never against whatever keys happen to exist on the host.
#
# This is a hardening layer ON TOP of the pinned-sha256 integrity model (R3-Q2):
# the out-of-band sha256 pin remains the primary trust anchor; GPG adds artifact
# provenance. It never replaces the sha256 check.
#
# GPG behavior is pinned: --batch --yes --armor --detach-sign to sign, and
# --verify with a hermetic homedir to check. No interactive trust prompts, and a
# fixed-length 64-hex compare elsewhere guarantees the hash is never truncated.

set -euo pipefail

# die: fail-fast, one shared error printer. Idempotent under lib.sh / release.sh
# / userdata.sh, which may already define die with their own prefix.
if ! declare -F die >/dev/null 2>&1; then
    die() { echo "gitd: sign-artifact: $*" >&2; exit 1; }
fi

# require_gpg fails fast when gpg is absent. Verification (host-side) is a hard
# requirement, so callers that verify must ensure gpg is installed first (see
# userdata.sh / update.sh), then fail loudly here if it is still missing.
require_gpg() {
    command -v gpg >/dev/null 2>&1 \
        || die "gpg not found in PATH; GnuPG is required for artifact signing/verification"
}

# sign_artifact <file> — write <file>.asc (detached, ASCII-armored) using the
# operator's signing key, resolved in precedence order:
#   1. GPG_KEY_ID (explicit env override wins)
#   2. git config user.signingkey (repo-local -> global -> system, one call)
#   3. gpg's default key (safe fallback)
# Signing is atomic: it either produces a valid .asc or fails loudly, so an
# unsigned artifact can never be published by accident.
sign_artifact() {
    local file="$1" key=""
    [[ -f "${file}" ]] || die "cannot sign missing artifact: ${file}"
    require_gpg
    if [[ -n "${GPG_KEY_ID:-}" ]]; then
        key="${GPG_KEY_ID}"
    else
        # git config exits 1 when the key is unset; tolerate that so it does not
        # trip `set -euo pipefail` unexpectedly.
        key="$(git config user.signingkey 2>/dev/null || true)"
    fi
    if [[ -n "${key}" ]]; then
        if [[ -n "${GPG_KEY_ID:-}" ]]; then
            echo "gitd: signing ${file} with GPG_KEY_ID='${key}'"
        else
            echo "gitd: signing ${file} with git config user.signingkey='${key}'"
        fi
        gpg --batch --yes --armor --detach-sign \
            --local-user "${key}" \
            --output "${file}.asc" "${file}" \
            || die "gpg detached-sign failed for ${file} (is signing key '${key}' a usable signing key?)"
    else
        echo "gitd: signing ${file} with gpg default key (no GPG_KEY_ID or git config user.signingkey)"
        gpg --batch --yes --armor --detach-sign \
            --output "${file}.asc" "${file}" \
            || die "gpg detached-sign failed for ${file} (no usable default signing key? set GPG_KEY_ID or git config user.signingkey)"
    fi
    echo "gitd: signed ${file} -> ${file}.asc"
}

# verify_artifact <file> <pinned-public-key> — prove <file> against <file>.asc
# using ONLY <pinned-public-key>, imported into a throwaway GNUPGHOME. Any
# failure (missing artifact, missing signature, missing key, bad signature,
# absent gpg) aborts immediately — a provenance check that cannot be satisfied
# must never fall through to trust. The throwaway homedir is always cleaned up,
# including when die() exits the script.
verify_artifact() {
    local file="$1" pubkey="$2"
    local sig="${file}.asc" gpg_home
    [[ -f "${file}" ]] || die "cannot verify missing artifact: ${file}"
    [[ -f "${sig}" ]] || die "signature file ${sig} is missing; refusing to trust unsigned artifact ${file}"
    [[ -f "${pubkey}" ]] || die "pinned GPG public key not found: ${pubkey}"
    require_gpg
    gpg_home="$(mktemp -d)"
    trap 'rm -rf "${gpg_home}"' EXIT
    gpg --homedir "${gpg_home}" --batch --quiet --import "${pubkey}" \
        || die "failed to import pinned GPG public key ${pubkey} into the throwaway keyring"
    gpg --homedir "${gpg_home}" --batch --quiet --verify "${sig}" "${file}" \
        || die "GPG signature verification FAILED for ${file}; provenance not proven against the pinned key — refusing to trust it"
    rm -rf "${gpg_home}"
    trap - EXIT
    echo "gitd: GPG signature OK for ${file} (pinned key ${pubkey})"
}
