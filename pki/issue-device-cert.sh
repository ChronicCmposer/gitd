#!/usr/bin/env bash
# pki/issue-device-cert.sh — issue a repeatable per-device TLS client cert and
# bundle it into an Apple .mobileconfig profile.
#
# Produces, for a named device (e.g. "macbook"):
#   ~/.ssh/gitd-device-certs/<device>/<device>.key          ECDSA P-256 private key (0600)
#   ~/.ssh/gitd-device-certs/<device>/<device>.csr          certificate signing request
#   ~/.ssh/gitd-device-certs/<device>/<device>.crt          client cert (30d, OU=device)
#   ~/.ssh/gitd-device-certs/<device>/<device>.mobileconfig Apple profile: CA trust + device identity
#
# The .mobileconfig bundles the TLS CA (the client trust pool) and the device
# identity (a PKCS#12) into a single install, so Safari/Keychain stop prompting
# for the cert. The intermediate <device>.p12 is an input to that profile, not a
# deliverable: its password is auto-generated and never written to a sidecar
# file, and the p12 itself is shredded on exit. Only the .mobileconfig (itself a
# secret) plus the .key/.crt remain.
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
#   pki/issue-device-cert.sh <device-name>

set -euo pipefail

# shellcheck source=pki/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_cmd openssl

# --- boundary: parse + validate the CLI args up front (fail-fast) --------------
if [[ $# -ne 1 ]]; then
    die "usage: issue-device-cert.sh <device-name>"
fi
DEVICE="$1"

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
MOBILECONFIG="${OUT_DIR}/${DEVICE}.mobileconfig"

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

# --- 5. bundle the identity into an Apple .mobileconfig ------------------------
# The p12 password is the highest-value secret this script mints. It is never
# written to its own sidecar file (a plaintext <device>.p12.password would
# linger as a credential): it lives only in this shell and, because Apple's
# profile format requires it, inside the .mobileconfig. The intermediate .p12 is
# an input to the profile, not a deliverable, so it's shredded on exit — a
# normal EXIT after the profile is built, or an early one on error — leaving
# only the .mobileconfig (itself a secret) and the key/cert behind. shred is
# best-effort: fall back to rm where it isn't available (e.g. macOS).
shred_or_rm() {
    for f in "$@"; do
        [[ -e "$f" ]] || continue
        shred -u "$f" 2>/dev/null || rm -f "$f"
    done
}
trap 'shred_or_rm "${P12}"' EXIT

p12pass="$(openssl rand -base64 18)"
# Apple's profile installer parses this embedded p12 with the same
# SecKeychainItemImport machinery that rejects some OpenSSL 3.x default cipher
# suites — surfacing as "MAC verification failed" / "The certificate could not
# be verified". PBE-SHA1-3DES plus an SHA-1 MAC is the maximally-compatible
# combination for both LibreSSL (macOS's system openssl) and OpenSSL 3.x.
openssl pkcs12 -export -inkey "${KEY}" -in "${CERT}" -certfile "${TLS_CA}" \
    -keypbe PBE-SHA1-3DES -certpbe PBE-SHA1-3DES -macalg sha1 \
    -name "gitd ${DEVICE} client" -passout "pass:${p12pass}" -out "${P12}"
chmod 0600 "${P12}"

gen_uuid() {
    if command -v uuidgen >/dev/null 2>&1; then
        uuidgen
    elif [[ -r /proc/sys/kernel/random/uuid ]]; then
        cat /proc/sys/kernel/random/uuid
    else
        python3 -c "import uuid; print(uuid.uuid4())"
    fi
}

ca_der_b64="$(openssl x509 -in "${TLS_CA}" -outform DER | base64 | tr -d '\n')"
p12_b64="$(base64 < "${P12}" | tr -d '\n')"
root_uuid="$(gen_uuid)"
identity_uuid="$(gen_uuid)"
profile_uuid="$(gen_uuid)"

cat > "${MOBILECONFIG}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key><string>com.apple.security.root</string>
      <key>PayloadIdentifier</key><string>dev.cmposer.cc.gitd.ca</string>
      <key>PayloadUUID</key><string>${root_uuid}</string>
      <key>PayloadVersion</key><integer>1</integer>
      <key>PayloadDisplayName</key><string>gitd device: ${DEVICE}</string>
      <key>PayloadCertificateFileName</key><string>ca.cer</string>
      <key>PayloadContent</key><data>${ca_der_b64}</data>
    </dict>
    <dict>
      <key>PayloadType</key><string>com.apple.security.pkcs12</string>
      <key>PayloadIdentifier</key><string>dev.cmposer.cc.gitd.identity.${DEVICE}</string>
      <key>PayloadUUID</key><string>${identity_uuid}</string>
      <key>PayloadVersion</key><integer>1</integer>
      <key>PayloadDisplayName</key><string>gitd device: ${DEVICE}</string>
      <key>PayloadCertificateFileName</key><string>${DEVICE}.p12</string>
      <key>PayloadContent</key><data>${p12_b64}</data>
      <key>Password</key><string>${p12pass}</string>
    </dict>
  </array>
  <key>PayloadDisplayName</key><string>gitd: ${DEVICE}</string>
  <key>PayloadIdentifier</key><string>dev.cmposer.cc.gitd.profile.${DEVICE}</string>
  <key>PayloadUUID</key><string>${profile_uuid}</string>
  <key>PayloadVersion</key><integer>1</integer>
  <key>PayloadType</key><string>Configuration</string>
  <key>PayloadRemovalDisallowed</key><false/>
</dict>
</plist>
PLIST
chmod 0600 "${MOBILECONFIG}"

# --- summary --------------------------------------------------------------------
echo "gitd: pki: issued device cert '${DEVICE}' (${TLS_CLIENT_DAYS}d, OU=device):"
echo "gitd: pki:   key:          ${KEY}"
echo "gitd: pki:   cert:         ${CERT}"
echo "gitd: pki:   mobileconfig: ${MOBILECONFIG}"
echo "gitd: pki: AirDrop or otherwise transfer the .mobileconfig to the device and open it to install."
echo "gitd: pki: The profile embeds the p12 password in plaintext (Apple's format requires this)"
echo "gitd: pki: — treat the .mobileconfig itself as a secret and delete it after installing."
echo "gitd: pki: macOS: System Settings > General > VPN & Device Management > install (accept 'not signed'),"
echo "gitd: pki: then Keychain Access > set the gitd TLS CA to Always Trust."
echo "gitd: pki: iOS: Settings > General > VPN & Device Management > install the profile,"
echo "gitd: pki: then Settings > General > About > Certificate Trust Settings > enable full trust"
echo "gitd: pki: for the gitd TLS CA."
echo "gitd: pki: the server's client-CA trust pool is ${TLS_CA} — no server-side change is needed to accept this cert."
echo "gitd: pki: revocation: reissue the CRL and push /gitd/server/revoked.crl (see pki/README.md)."
