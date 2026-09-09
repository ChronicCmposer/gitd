#!/usr/bin/env bash
# cloudformation/upload-certs.sh — push pre-generated gitd certs to SSM.
#
# Secret-free by design (R3-Q3): it contains no private-key material or
# secrets — it reads the already-generated certs from the pki/ staging tree
# and the tools/ssh-ca host-key output dir, then pushes them to SSM
# SecureString parameters under /gitd/*. deploy.sh (Phase 7) runs this before
# create-stack; the certs are client-side crypto that predate the stack, so
# they must never appear in the CloudFormation template (which would put
# secrets in VCS).
#
# All params are SecureString encrypted with the DEFAULT aws/ssm managed key
# (R5-Q8); no customer-managed CMK. Requires aws credentials with
# ssm:PutParameter on /gitd/*.
#
# Usage:
#   cloudformation/upload-certs.sh
# Environment overrides:
#   AWS_REGION          region (default us-east-2)
#   GITD_PKI_DIR        pki/ staging dir (default <repo>/pki)
#   GITD_HOST_DIR       tools/ssh-ca issue-host output dir (default <repo>/host)

set -euo pipefail

die() {
    echo "gitd: upload-certs: $*" >&2
    exit 1
}
require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}
require_cmd aws

REGION="${AWS_REGION:-us-east-2}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PKI_DIR="${GITD_PKI_DIR:-${REPO_ROOT}/pki}"
HOST_DIR="${GITD_HOST_DIR:-${REPO_ROOT}/host}"
CA_DIR="${HOME}/.ssh/gitd-ca"

# put pushes one file to an SSM SecureString parameter under /gitd/<name>.
# Overwrite is allowed (renewal); the default aws/ssm key encrypts the value
# (R5-Q8). A missing source file fails fast — we never push a partial set.
put() {
    local name="$1" file="$2"
    [[ -f "${file}" ]] || die "missing source ${file} for /gitd/${name}"
    aws ssm put-parameter \
        --region "${REGION}" \
        --name "/gitd/${name}" \
        --type SecureString \
        --overwrite \
        --value "$(cat "${file}")" >/dev/null \
        || die "failed to put /gitd/${name}"
    echo "gitd: upload-certs: /gitd/${name} updated"
}

# --- fail-fast sanity checks before pushing anything ---------------------------
# TLS server cert must chain to the local TLS CA; host cert must be a valid
# OpenSSH cert. Broken material is never pushed (no silent partial success).
require_cmd openssl
require_cmd ssh-keygen
[[ -f "${PKI_DIR}/server.crt" ]] || die "missing ${PKI_DIR}/server.crt; run pki/pki-new.sh first"
[[ -f "${CA_DIR}/tls-ca.crt" ]] || die "missing ${CA_DIR}/tls-ca.crt; run pki/pki-new.sh first"
openssl verify -CAfile "${CA_DIR}/tls-ca.crt" "${PKI_DIR}/server.crt" >/dev/null 2>&1 \
    || die "server cert does not verify against the TLS CA; refusing to upload"
[[ -f "${HOST_DIR}/ssh_host_ed25519_key-cert.pub" ]] \
    || die "missing ${HOST_DIR}/ssh_host_ed25519_key-cert.pub; run tools/ssh-ca/ssh-ca issue-host ${HOST_DIR}"
ssh-keygen -L -f "${HOST_DIR}/ssh_host_ed25519_key-cert.pub" >/dev/null 2>&1 \
    || die "host cert is not a valid OpenSSH cert; refusing to upload"

# --- TLS server material (browse mTLS, R12-Q6) --------------------------------
put server/server.crt      "${PKI_DIR}/server.crt"
put server/server.key      "${PKI_DIR}/server.key"
put server/client-ca.crt   "${PKI_DIR}/client-ca.crt"
put server/revoked.crl     "${PKI_DIR}/revoked.crl"

# --- SSH host material (issue-host output, R3-Q3 / R13-Q3) --------------------
put host/ssh_host_ed25519_key          "${HOST_DIR}/ssh_host_ed25519_key"
put host/ssh_host_ed25519_key.pub      "${HOST_DIR}/ssh_host_ed25519_key.pub"
put host/ssh_host_ed25519_key-cert.pub "${HOST_DIR}/ssh_host_ed25519_key-cert.pub"

echo "gitd: upload-certs: all certs pushed to SSM /gitd/* (SecureString, default aws/ssm key)"
