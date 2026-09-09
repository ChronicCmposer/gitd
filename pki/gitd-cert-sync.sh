#!/usr/bin/env bash
# pki/gitd-cert-sync.sh — HOST-side browse TLS material refresh (R12-Q6).
#
# Installed by Phase 7's userdata and run hourly as root (systemd timer). It
# pulls the browse mTLS material (server cert/key, client-CA pool, revocation
# list) from SSM /gitd/server/* and writes it to /etc/gitd/tls/ root:root.
# Zero-downtime: browse re-reads every file per handshake (R5-Q6, R7-Q5), so
# this script never restarts the service — it just atomically replaces files.
#
# For a small atomicity window all files are staged + verified first, then
# renamed together; the server cert/key pair is checked for consistency before
# anything is installed (fail-fast, never install a broken pair).

set -euo pipefail

TLS_DIR="/etc/gitd/tls"
REGION="${AWS_REGION:-us-east-2}"

die() {
    echo "gitd: cert-sync: $*" >&2
    exit 1
}
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}
require_cmd aws
require_cmd openssl

mkdir -p "${TLS_DIR}"
chmod 0700 "${TLS_DIR}"

# fetch stages one SSM SecureString param into a temp file and returns its path.
# set -e aborts on any fetch failure (fail-fast; nothing is installed).
fetch() {
    local name="$1" mode="$2"
    local dest="${TLS_DIR}/.staged-$(basename "${name}").$$"
    aws ssm get-parameter --region "${REGION}" \
        --name "/gitd/${name}" --with-decryption \
        --query Parameter.Value --output text > "${dest}" \
        || { rm -f "${dest}"; die "failed to fetch /gitd/${name}"; }
    chmod "${mode}" "${dest}"
    printf '%s\n' "${dest}"
}

server_crt="$(fetch server/server.crt 0644)"
server_key="$(fetch server/server.key 0600)"
client_ca="$(fetch server/client-ca.crt 0644)"
revoked="$(fetch server/revoked.crl 0644)"

trap 'rm -f "${server_crt}" "${server_key}" "${client_ca}" "${revoked}"' EXIT

# --- consistency checks before install (fail-fast, no broken pair) ------------
# The server cert and key must be the same key pair, and the cert must chain
# to the client-CA pool. A mismatch aborts without touching /etc/gitd/tls.
[[ -s "${server_crt}" ]] || die "staged server.crt is empty"
[[ -s "${server_key}" ]] || die "staged server.key is empty"
cert_pub="$(openssl x509 -in "${server_crt}" -pubkey -noout 2>/dev/null)" \
    || die "cannot read public key from staged server.crt"
key_pub="$(openssl pkey -in "${server_key}" -pubout 2>/dev/null)" \
    || die "cannot read public key from staged server.key"
[[ "${cert_pub}" == "${key_pub}" ]] \
    || die "server cert and key are not the same key pair; refusing to install"
openssl verify -CAfile "${client_ca}" "${server_crt}" >/dev/null 2>&1 \
    || die "server cert does not chain to the client-CA pool; refusing to install"
openssl crl -in "${revoked}" -noout -text >/dev/null 2>&1 \
    || die "staged revocation list is not a valid CRL; refusing to install"

# --- atomic install (rename all staged files into place) -----------------------
mv -f "${server_crt}" "${TLS_DIR}/server.crt"
mv -f "${server_key}" "${TLS_DIR}/server.key"
mv -f "${client_ca}"  "${TLS_DIR}/client-ca.crt"
mv -f "${revoked}"    "${TLS_DIR}/revoked.crl"

echo "gitd: cert-sync: /etc/gitd/tls refreshed from SSM (zero-downtime)"
