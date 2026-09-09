# Quantum Threat Model (Phase 8.1 runbook, R5-Q5)

This note records the deliberate cryptographic posture of git.cmposer.cc and
when it must be revisited. Two very different claims are often conflated under
"post-quantum"; this server partitions them clearly (R5-Q5):

1. **Key exchange (confidentiality)** — post-quantum **today** via hybrid PQC.
2. **Signatures / authentication (integrity)** — **classical** today, because
   there is no standard PQC signature scheme usable in OpenSSH yet.

The guidance below is the standing decision (R5-Q5); it carries a hard
"revisit when ML-DSA lands" trigger.

## 1. Key exchange: hybrid-PQC defeats harvest-now-decrypt-later

The SSH transport negotiates hybrid PQC kex only:

- `mlkem768x25519-sha256`
- `sntrup761x25519-sha512@openssh.com`

The client (OpenSSH ≥10.x) negotiates `mlkem768x25519`. Because the kex is
hybrid (a classically-hard curve **and** a lattice mechanism in one handshake),
an attacker who records the ciphertext today cannot decrypt it later with a
(scaled) quantum computer: **harvest-now-decrypt-later fails against the hybrid
construction.** This is the property that matters for a personal git server's
transport — whatever you push now is not recoverable by a future cryptanalytic
break of the pure-classical part.

On the **browse** side, `:443` is `MinVersion TLS 1.3` **only** (R4-Q8). Go's
`crypto/tls` applies the hybrid `X25519MLKEM768` group on TLS 1.3 handshakes,
so every mTLS browse handshake is also post-quantum-confidential by
construction. (The mTLS client certs still work on 1.3.) The pairing — PQC kex
over both SSH :22 and HTTPS :443 — is what makes the *entire* transport
harvest-safe, not just git.

## 2. Authentication signatures: still classical Ed25519

Cryptographic **signatures** are used for authentication everywhere:

- SSH **user + host certs** (and CA) — Ed25519
- TLS mTLS **certs/CA/CRL** — ECDSA P-256
- Webhook HMAC — HMAC-SHA256 (symmetric, not signature-based)

None of these are PQC, and that is by design with reasons attached:

- **No PQC signature algorithms exist in OpenSSH as of 10.x.** It negotiates
  PQC **kex** but there is no PQC *signature* algorithm available, so a
  post-quantum auth-signature claim is not even possible in the current
  ecosystem.
- **RSA-4096 is strictly worse, not better.** Under a sufficiently large
  quantum computer, Shor's algorithm breaks RSA (and ECDSA/EdDSA) factoring /
  discrete-log classes outright. But RSA-4096 does **not** gain anything
  quantum-wise while being meaningless to pick over Ed25519 for
  post-quantum-safety — and Ed25519 is the smaller, faster, safer classical
  scheme. So the "bigger RSA = more quantum proof" intuition is exactly
  wrong (R5-Q5, R13-Q3).
- **Ed25519 is the deliberate classical baseline** — a deliberate, uniform,
  well-audited scheme that is the best *today* given no adopted PQC
  signatures, with the caveat documented so nobody mistakes "no PQC
  signatures" for "secure against a future quantum adversary".

This asymmetry matters for the *model*: **your authentication is downgradable
if the harshest future assumed.** The posture is therefore: confidentiality is
quantum-safe now (hybrid kex everywhere); authentication is classical until
ML-DSA lands and is then to be re-evaluated.

## 3. Decisions that follow (traceable R-numbers)

- **R5-Q5** — hybrid-PQC kex defeats harvest-now-decrypt-later; auth signatures
  remain classical Ed25519 (no PQC signatures in OpenSSH 10.x); RSA-4096
  strictly worse (Shor); accepted + documented; revisit when ML-DSA lands.
- **R4-Q8** — browse `:443` TLS 1.3 only → hybrid `X25519MLKEM768` on every
  handshake; mTLS certs work on 1.3.
- **R13-Q3** — host key pinned Ed25519 only; no RSA/ECDSA/DSA host keys in the
  image.
- **R2-Q17** — custom static OpenSSH build linked WITH OpenSSL to get the PQC
  kex AL2023's 8.7p1 lacks (the reason we build at all).

## 4. Revisit triggers

Revisit **all** of the above when:

- **ML-DSA (and/or ML-KEM signature hybrids) land in OpenSSH.** This is the
  named condition in R5-Q5. Plan to test a kex/`HostKeyAlgorithm`/`PubkeyAcceptedKeyTypes`
  extension, build it into the `tools/dist` pipeline (R2-Q17 rebuild), and
  decide whether user/host certs gain ML-DSA layers. Until then the classical
  Ed25519 position stands.
- Any **new PQC kex** becomes the OpenSSH default and you want to pin it in
  the hardened `sshd_config` rather than rely on negotiation.
- The TLS stack changes in a way that no longer guarantees the 1.3/hybrid
  handshake (e.g. a forced 1.2 downgrade, which is exactly what R4-Q8's
  `MinVersion TLS 1.3 only` forbids).

The standing answer, absent those triggers: **hybrid PQC key exchange + TLS
1.3 hybrid handshakes everywhere (confidentiality quantum-safe,
harvest-now-decrypt-later defeated); classical Ed25519/ECDSA P-256
authentication (integrity), documented and acceptable until ML-DSA lands.**
