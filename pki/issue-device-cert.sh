#!/usr/bin/env bash
# pki/issue-device-cert.sh — issue a repeatable per-device TLS client cert.
#
# Produces, for a named device (e.g. "macbook"):
#   ~/.ssh/gitd-device-certs/<device>/<device>.key   ECDSA P-256 private key (0600)
#   ~/.ssh/gitd-device-certs/<device>/<device>.csr   certificate signing request
#   ~/.ssh/gitd-device-certs/<device>/<device>.crt   client cert (30d, OU=device)
#   ~/.ssh/gitd-device-certs/<device>/<device>.p12   [optional] PKCS#12 for macOS Keychain
#
# Certs are stored under ~/.ssh/gitd-device-certs/<device>/ (NOT the committed
# pki/ staging tree, which is the server upload set) and are gitignored by the
# root .gitignore alongside the CA.
#
# The browse server's client-CA trust pool is tls-ca.crt itself, so a freshly
# issued device cert is accepted with NO server-side change; revocation is by
# reissuing the CRL and pushing /gitd/server/revoked.crl (see pki/README.md).
#
# Usage:
#   pki/issue-device-cert.sh <device-name> [-p12-pass <password>]

set -euo pipefail

# shellcheck source=pki/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_cmd openssl

# --- boundary: parse + validate the CLI args up front (fail-fast) --------------
if [[ $# -lt 1 ]]; then
    die "usage: issue-device-cert.sh <device-name> [-p12-pass <password>]"
fi
DEVICE="$1"
shift

P12_PASS=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -p12-pass|--p12-pass)
            if [[ $# -lt 2 || -z "$2" ]]; then
                die "$1 requires a non-empty password"
            fi
            P12_PASS="$2"
            shift 2
            ;;
        *)
            die "unknown argument '$1'; usage: issue-device-cert.sh <device-name> [-p12-pass <password>]"
            ;;
    esac
done

# Device label must be a reasonable identifier: no spaces or slashes.
[[ "$DEVICE" =~ ^[A-Za-z0-9._-]+$ ]] \
    || die "invalid device name '${DEVICE}': use only letters, digits, dots, underscores, hyphens"

require_tls_ca

CA_DIR="$(ca_dir)"
TLS_CA="${CA_DIR}/tls-ca.crt"
TLS_DB="${CA_DIR}/tls-db"
CONF="${TLS_DB}/openssl.cnf"

OUT_DIR="${HOME}/.ssh/gitd-device-certs/${DEVICE}"
KEY="${OUT_DIR}/${DEVICE}.key"
CSR="${OUT_DIR}/${DEVICE}.csr"
CERT="${OUT_DIR}/${DEVICE}.crt"
P12="${OUT_DIR}/${DEVICE}.p12"

# --- early exit: refuse to clobber an existing cert for this device ------------
if [[ -f "${KEY}" || -f "${CERT}" ]]; then
    die "device cert '${DEVICE}' already exists (${KEY}); refusing to overwrite (reissue via a different device name)"
fi

# --- build the device directory (0700) -----------------------------------------
mkdir -p "${OUT_DIR}"
chmod 0700 "${OUT_DIR}"

# --- 1. key (ECDSA P-256, 0600) -------------------------------------------------
openssl ecparam -name P-256 -genkey -noout -out "${KEY}"
chmod 0600 "${KEY}"

# --- 2. CSR (CN=<device>/O=gitd/OU=device) --------------------------------------
openssl req -new -key "${KEY}" \
    -subj "/CN=${DEVICE}/O=gitd/OU=device" \
    -out "${CSR}"

# --- 3. cert (sign with the TLS CA, device_ext, TLS_CLIENT_DAYS) ---------------
openssl ca -batch -config "${CONF}" -extensions device_ext \
    -days "${TLS_CLIENT_DAYS}" -notext \
    -in "${CSR}" -out "${CERT}" >/dev/null 2>&1
chmod 0644 "${CERT}"

# --- 4. verify (fail-fast: must chain to the CA and carry clientAuth EKU) ------
openssl verify -CAfile "${TLS_CA}" "${CERT}" >/dev/null 2>&1 \
    || die "device cert '${DEVICE}' does not verify against ${TLS_CA}"
# OpenSSL renders clientAuth either as the human label "TLS Web Client
# Authentication" or as its OID 1.3.6.1.5.5.7.3.2 depending on the version;
# match either so the check is robust across OpenSSL releases.
openssl x509 -in "${CERT}" -noout -ext extendedKeyUsage \
    | grep -Eq "TLS Web Client Authentication|1\.3\.6\.1\.5\.5\.7\.3\.2" \
    || die "device cert '${DEVICE}' is missing extendedKeyUsage=clientAuth"

# --- 5. optional PKCS#12 for macOS Keychain import ------------------------------
if [[ -n "${P12_PASS}" ]]; then
    openssl pkcs12 -export -out "${P12}" -inkey "${KEY}" -in "${CERT}" \
        -certfile "${TLS_CA}" -name "gitd ${DEVICE} client" \
        -passout "pass:${P12_PASS}"
    chmod 0600 "${P12}"
fi

# --- summary --------------------------------------------------------------------
echo "gitd: pki: issued device cert '${DEVICE}' (${TLS_CLIENT_DAYS}d, OU=device):"
echo "gitd: pki:   key:  ${KEY}"
echo "gitd: pki:   cert: ${CERT}"
if [[ -n "${P12_PASS}" ]]; then
    echo "gitd: pki:   p12:  ${P12} (macOS Keychain import; -name 'gitd ${DEVICE} client')"
fi
echo "gitd: pki: the server's client-CA trust pool is ${TLS_CA} — no server-side change is needed to accept this cert."
echo "gitd: pki: revocation: reissue the CRL and push /gitd/server/revoked.crl (see pki/README.md)."
