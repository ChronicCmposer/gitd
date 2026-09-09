#!/usr/bin/env bash
# pki/pki-new.sh — generate the gitd TLS mTLS PKI from scratch.
#
# Produces (all passphrase-less per R12-Q7):
#   ~/.ssh/gitd-ca/tls-ca.key       TLS CA private key (0600, gitignored)
#   ~/.ssh/gitd-ca/tls-ca.crt       TLS CA certificate (10y, ECDSA P-256)
#   ~/.ssh/gitd-ca/tls-db/          CA index database for revocation tracking
#   pki/server.key                  server private key (0600, gitignored)
#   pki/server.crt                  server cert (90d, OU=server, CN=git.cmposer.cc)
#   pki/client.key                  client private key (0600, gitignored)
#   pki/client.crt                  client cert (30d, OU=device)
#   pki/client-ca.crt               = copy of tls-ca.crt (the client trust pool)
#   pki/revoked.crl                 CRL signed by the TLS CA (initially empty)
#
# The pki/ staging tree holds exactly the files Phase 7's userdata installs to
# /etc/gitd/tls/ (server.crt/key, client-ca.crt, revoked.crl) and that
# cloudformation/upload-certs.sh pushes to SSM /gitd/server/*. All keys are
# ECDSA P-256 only; OU roles server/device are embedded for audit.
#
# The TLS CA signs both server and client certs; the client-CA trust pool
# (client-ca.crt) is therefore the CA cert itself, and the CRL revokes any
# issued cert. Browse re-reads all of this per handshake (R7-Q5).

set -euo pipefail

# shellcheck source=pki/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_cmd openssl

CA_DIR="$(ca_dir)"
TLS_CA="${CA_DIR}/tls-ca"
TLS_DB="${CA_DIR}/tls-db"
STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # this directory (pki/)
CONF="${TLS_DB}/openssl.cnf"

# --- early exit: refuse to clobber an existing CA (fail-fast) -----------------
# A real CA is never silently overwritten; pki-new.sh is a one-shot bootstrap.
if [[ -f "${TLS_CA}.key" || -f "${TLS_CA}.crt" || -d "${TLS_DB}" ]]; then
    die "TLS CA already exists (${TLS_CA}.crt); refusing to overwrite (delete it manually to re-key)"
fi

mkdir -p "${CA_DIR}" "${TLS_DB}/certs" "${TLS_DB}/newcerts" "${STAGE}"
chmod 0700 "${CA_DIR}" "${TLS_DB}"
: > "${TLS_DB}/index.txt"
printf '1000\n' > "${TLS_DB}/serial"
printf '1000\n' > "${TLS_DB}/crlnumber"

# --- write the CA configuration (OpenSSL 3.x) ---------------------------------
cat > "${CONF}" <<EOF
[ ca ]
default_ca = gitd_ca

[ gitd_ca ]
dir              = ${TLS_DB}
database         = \$dir/index.txt
new_certs_dir    = \$dir/newcerts
certificate      = ${TLS_CA}.crt
private_key      = ${TLS_CA}.key
serial           = \$dir/serial
crlnumber        = \$dir/crlnumber
default_md       = sha256
default_days     = ${TLS_SERVER_DAYS}
default_crl_days = 30
policy           = gitd_policy
unique_subject   = no
copy_extensions  = copy

[ gitd_policy ]
commonName              = supplied
organizationName        = supplied
organizationalUnitName  = supplied

[ server_ext ]
basicConstraints       = critical,CA:FALSE
keyUsage               = critical,digitalSignature,keyEncipherment
extendedKeyUsage       = serverAuth
subjectKeyIdentifier   = hash
authorityKeyIdentifier = keyid:always

[ device_ext ]
basicConstraints       = critical,CA:FALSE
keyUsage               = critical,digitalSignature
extendedKeyUsage       = clientAuth
subjectKeyIdentifier   = hash
authorityKeyIdentifier = keyid:always

[ crl_ext ]
authorityKeyIdentifier = keyid:always
EOF

# --- 1. TLS CA keypair + self-signed cert (P-256, 10y) -------------------------
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
    -keyout "${TLS_CA}.key" -out "${TLS_CA}.crt" \
    -days "${TLS_CA_DAYS}" -nodes \
    -subj "/CN=gitd TLS CA/O=gitd/OU=ca" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" \
    -addext "subjectKeyIdentifier=hash" >/dev/null 2>&1
chmod 0600 "${TLS_CA}.key"
chmod 0644 "${TLS_CA}.crt"

# --- 2. server key + cert (OU=server, CN=git.cmposer.cc, 90d) ------------------
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

# --- 3. client key + cert (OU=device, 30d) -------------------------------------
openssl ecparam -name P-256 -genkey -noout -out "${STAGE}/client.key"
chmod 0600 "${STAGE}/client.key"
openssl req -new -key "${STAGE}/client.key" \
    -subj "/CN=gitd-device/O=gitd/OU=device" \
    -out "${TLS_DB}/client.csr"
openssl ca -batch -config "${CONF}" -extensions device_ext \
    -days "${TLS_CLIENT_DAYS}" -notext \
    -in "${TLS_DB}/client.csr" -out "${STAGE}/client.crt" >/dev/null 2>&1
chmod 0644 "${STAGE}/client.crt"

# --- 4. client-CA pool = the CA cert; CRL signed by the CA ---------------------
cp "${TLS_CA}.crt" "${STAGE}/client-ca.crt"
chmod 0644 "${STAGE}/client-ca.crt"

openssl ca -config "${CONF}" -gencrl -crlexts crl_ext \
    -out "${STAGE}/revoked.crl" >/dev/null 2>&1
chmod 0644 "${STAGE}/revoked.crl"

# --- 5. verify everything (fail-fast, no silent partial success) ---------------
openssl x509 -in "${TLS_CA}.crt" -noout -text >/dev/null \
    || die "TLS CA cert failed verification"
openssl x509 -in "${STAGE}/server.crt" -noout -text >/dev/null \
    || die "server cert failed verification"
openssl verify -CAfile "${TLS_CA}.crt" "${STAGE}/server.crt" >/dev/null 2>&1 \
    || die "server cert does not verify against the CA"
openssl verify -CAfile "${TLS_CA}.crt" "${STAGE}/client.crt" >/dev/null 2>&1 \
    || die "client cert does not verify against the CA"
openssl crl -in "${STAGE}/revoked.crl" -noout -text >/dev/null \
    || die "CRL failed verification"

echo "gitd: pki: TLS PKI generated:"
echo "gitd: pki:   CA:       ${TLS_CA}.crt (10y, P-256, OU=ca)"
echo "gitd: pki:   server:   ${STAGE}/server.crt (${TLS_SERVER_DAYS}d, OU=server, CN=${GITD_HOST})"
echo "gitd: pki:   client:   ${STAGE}/client.crt (${TLS_CLIENT_DAYS}d, OU=device)"
echo "gitd: pki:   clientCA: ${STAGE}/client-ca.crt (= CA cert, client trust pool)"
echo "gitd: pki:   CRL:      ${STAGE}/revoked.crl"
echo "gitd: pki: push to SSM with cloudformation/upload-certs.sh"
