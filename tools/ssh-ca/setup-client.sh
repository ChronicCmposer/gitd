#!/usr/bin/env bash
# tools/ssh-ca/setup-client.sh — configure this box as a git.cmposer.cc client.
#
# 6.3 / R2-Q14 / R10-Q10 client setup:
#   - dedicated client keypair ~/.ssh/gitd_ed25519 (separate from the github
#     key so the gitd cert is never offered to github.com),
#   - ~/.ssh/config host entry for git.cmposer.cc (User git, that key),
#   - @cert-authority git.cmposer.cc line in ~/.ssh/known_hosts (no TOFU),
#   - an admin cert (principals git,admin) over the dedicated key.
#
# The script is idempotent and non-destructive: it backs up ~/.ssh/config and
# known_hosts before editing and only adds each block if absent. Run it from a
# shell with the CA present (tools/ssh-ca/ssh-ca init first). It edits the
# real ~/.ssh only when you run it — nothing here runs automatically.

set -euo pipefail

# shellcheck source=tools/ssh-ca/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_cmd ssh-keygen
require_cmd ssh

SSH_DIR="${HOME}/.ssh"
CLIENT_KEY="${SSH_DIR}/gitd_ed25519"
CONFIG="${SSH_DIR}/config"
KNOWN="${SSH_DIR}/known_hosts"
CA_DIR="$(ca_dir)"
CA_PUB="${CA_DIR}/ssh-ca.pub"

require_ca

[[ -d "${SSH_DIR}" ]] || mkdir -p "${SSH_DIR}"
chmod 0700 "${SSH_DIR}"

# --- 1. dedicated client keypair (Ed25519, no passphrase for unattended use) --
if [[ ! -f "${CLIENT_KEY}" ]]; then
    ssh-keygen -t ed25519 -f "${CLIENT_KEY}" -N "" -C "gitd client key (git.cmposer.cc)" >/dev/null
    echo "gitd: setup-client: created dedicated client key ${CLIENT_KEY}"
else
    echo "gitd: setup-client: client key already exists (${CLIENT_KEY}); keeping it"
fi
chmod 0600 "${CLIENT_KEY}"

# --- 2. ~/.ssh/config host entry (backup + atomic, idempotent) ----------------
if [[ -f "${CONFIG}" ]] && grep -q '^Host git\.cmposer\.cc$' "${CONFIG}"; then
    echo "gitd: setup-client: ~/.ssh/config already has a git.cmposer.cc block; leaving it"
else
    [[ -f "${CONFIG}" ]] && cp -a "${CONFIG}" "${CONFIG}.bak.$(date +%s)"
    cat >> "${CONFIG}" <<EOF

Host git.cmposer.cc
    HostName git.cmposer.cc
    User git
    IdentityFile ${CLIENT_KEY}
    IdentitiesOnly yes
    CertificateFile ${CLIENT_KEY}-cert.pub
    ServerAliveInterval 60
EOF
    chmod 0600 "${CONFIG}"
    echo "gitd: setup-client: added git.cmposer.cc host block to ~/.ssh/config"
fi

# --- 3. @cert-authority line in known_hosts (no TOFU, R10-Q10) ----------------
CA_LINE="@cert-authority git.cmposer.cc $(cat "${CA_PUB}")"
if [[ -f "${KNOWN}" ]] && grep -Fq "${CA_LINE}" "${KNOWN}"; then
    echo "gitd: setup-client: known_hosts already trusts the gitd CA; leaving it"
else
    [[ -f "${KNOWN}" ]] && cp -a "${KNOWN}" "${KNOWN}.bak.$(date +%s)"
    printf '%s\n' "${CA_LINE}" >> "${KNOWN}"
    chmod 0600 "${KNOWN}"
    echo "gitd: setup-client: added @cert-authority line to ~/.ssh/known_hosts"
fi

# --- 4. admin cert (principals git,admin, R2-Q14) ------------------------------
"${SSHCA_DIR}/ssh-ca" issue-user --admin "${CLIENT_KEY}.pub"
echo "gitd: setup-client: admin cert issued; verify with:"
echo "gitd: setup-client:   ssh-keygen -L -f ${CLIENT_KEY}-cert.pub"
echo "gitd: setup-client: then test: ssh git@git.cmposer.cc"
