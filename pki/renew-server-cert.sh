#!/usr/bin/env bash
# pki/renew-server-cert.sh — client-side TLS server-cert renewal (R12-Q6).
#
# Re-issues the browse server cert (90d) with the existing local TLS CA and
# pushes the refreshed material to SSM /gitd/server/*. Phase 7's host-side
# gitd-cert-sync timer then pulls it to /etc/gitd/tls/ (zero-downtime via the
# per-handshake reads in R5-Q6/R7-Q5). The systemd user timer
# pki/systemd/gitd-tls-renew.{service,timer} drives this on a monthly cadence.
#
# This script runs on the CLIENT box (this host) where the CA lives; it needs
# aws credentials with ssm:PutParameter on /gitd/server/*.

set -euo pipefail

# shellcheck source=pki/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_cmd openssl

CA_DIR="$(ca_dir)"
TLS_CA="${CA_DIR}/tls-ca"
TLS_DB="${CA_DIR}/tls-db"
STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # this directory (pki/)
CONF="${TLS_DB}/openssl.cnf"

require_tls_ca
[[ -f "${CONF}" ]] || die "CA config not found (${CONF}); run pki-new.sh first"

# A fresh server key each renewal (best practice: rotate keys, don't reuse).
openssl ecparam -name P-256 -genkey -noout -out "${STAGE}/server.key"
chmod 0600 "${STAGE}/server.key"
openssl req -new -key "${STAGE}/server.key" \
    -subj "/CN=${GITD_HOST}/O=gitd/OU=server" \
    -addext "subjectAltName=DNS:${GITD_HOST}" \
    -out "${TLS_DB}/server.csr"
openssl ca -batch -config "${CONF}" -extensions server_ext \
    -days "${TLS_SERVER_DAYS}" -notext \
    -in "${TLS_DB}/server.csr" -out "${STAGE}/server.crt" >/dev/null 2>&1
chmod 0644 "${STAGE}/server.crt"

# Re-issue the CRL so any revocations recorded since the last run take effect
# immediately (the host pulls the new CRL on its next gitd-cert-sync run).
openssl ca -config "${CONF}" -gencrl -crlexts crl_ext \
    -out "${STAGE}/revoked.crl" >/dev/null 2>&1
chmod 0644 "${STAGE}/revoked.crl"

# Fail-fast verification before touching SSM (no partial state pushed).
openssl verify -CAfile "${TLS_CA}.crt" "${STAGE}/server.crt" >/dev/null 2>&1 \
    || die "renewed server cert does not verify against the CA"
openssl crl -in "${STAGE}/revoked.crl" -noout -text >/dev/null \
    || die "renewed CRL failed verification"

echo "gitd: pki: server cert renewed (${TLS_SERVER_DAYS}d, OU=server, CN=${GITD_HOST})"
echo "gitd: pki: pushing to SSM /gitd/server/* ..."
"${STAGE}/../cloudformation/upload-certs.sh" \
    || die "SSM upload failed; local certs renewed but not pushed (rerun upload-certs.sh)"
echo "gitd: pki: server cert renewal complete"
