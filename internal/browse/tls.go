package browse

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

// TLSPaths are the file-path references for the mTLS material (R1-Q4,
// R6-Q5): server cert/key, client-CA pool, and the file-backed revocation
// list.
type TLSPaths struct {
	Cert           string
	Key            string
	ClientCA       string
	RevocationList string
}

// NewTLSConfig builds the mTLS *tls.Config (R4-Q8, R5-Q6, R7-Q5, R9-Q8):
//
//   - MinVersion TLS 1.3 only — guarantees the hybrid PQC X25519MLKEM768
//     key exchange on every handshake (R4-Q8).
//   - RequireAndVerifyClientCert — mTLS is the gate; no cert-less surface
//     (R9-Q9).
//   - GetCertificate re-reads the server cert/key on every handshake for
//     zero-downtime rotation (R5-Q6).
//   - GetConfigForClient re-reads the client-CA pool and revocation list on
//     every handshake for zero-downtime revocation (R7-Q5).
//   - ECDSA P-256 only for both the server and accepted client keys.
//   - Client certs: any valid CA-signed cert passes (no CN allowlist; the
//     per-handshake revocation check covers loss, R9-Q8).
//
// Material is loaded once at startup to fail fast on missing/invalid files
// (R2-Q12); handshakes re-read it.
func NewTLSConfig(p TLSPaths) (*tls.Config, error) {
	if _, err := loadServerCert(p.Cert, p.Key); err != nil {
		return nil, fmt.Errorf("browse tls: %w", err)
	}
	if _, err := loadClientCAs(p.ClientCA); err != nil {
		return nil, fmt.Errorf("browse tls: %w", err)
	}
	if _, err := loadRevoked(p.RevocationList); err != nil {
		return nil, fmt.Errorf("browse tls: %w", err)
	}

	base := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			clientCAs, err := loadClientCAs(p.ClientCA)
			if err != nil {
				return nil, err
			}
			revoked, err := loadRevoked(p.RevocationList)
			if err != nil {
				return nil, err
			}
			return &tls.Config{
				MinVersion: tls.VersionTLS13,
				ClientAuth: tls.RequireAndVerifyClientCert,
				ClientCAs:  clientCAs,
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					return verifyClientCert(rawCerts, revoked)
				},
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return loadServerCert(p.Cert, p.Key)
				},
			}, nil
		},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return loadServerCert(p.Cert, p.Key)
		},
	}
	return base, nil
}

// loadServerCert reads and parses the server cert/key, verifying the leaf is
// ECDSA P-256 (R5-Q6 per-handshake rotation + P-256-only requirement).
func loadServerCert(certPath, keyPath string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load server cert %s: %w", certPath, err)
	}
	leaf, err := parseLeaf(cert.Certificate)
	if err != nil {
		return nil, fmt.Errorf("load server cert %s: %w", certPath, err)
	}
	if err := requireP256(leaf.PublicKey); err != nil {
		return nil, fmt.Errorf("server cert %s: %w", certPath, err)
	}
	return &cert, nil
}

// loadClientCAs reads the client-CA pool (R7-Q5 per-handshake reload).
func loadClientCAs(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client CA %s: %w", path, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("client CA %s: no certificates parsed", path)
	}
	return pool, nil
}

// revokedSet is the set of revoked serial numbers from the CRL (R7-Q5).
type revokedSet map[string]bool

// loadRevoked reads the file-backed revocation list and extracts the revoked
// serials. A missing file is an error (fail-fast, R2-Q12); an empty/absent CRL
// revokes nothing. Both DER and PEM CRLs are accepted.
func loadRevoked(path string) (revokedSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read revocation list %s: %w", path, err)
	}
	if len(data) == 0 {
		return revokedSet{}, nil
	}
	crl, err := x509.ParseRevocationList(data)
	if err != nil {
		// Try PEM-decoding a DER CRL block before giving up.
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("parse revocation list %s: %w", path, err)
		}
		crl, err = x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse revocation list %s: %w", path, err)
		}
	}
	revoked := make(revokedSet, len(crl.RevokedCertificateEntries))
	for _, entry := range crl.RevokedCertificateEntries {
		revoked[entry.SerialNumber.String()] = true
	}
	return revoked, nil
}

// verifyClientCert validates the presented client leaf: ECDSA P-256 only and
// serial not revoked (R7-Q5, R9-Q8). The standard chain verification against
// the per-handshake ClientCAs pool already ran; this adds the P-256 and
// revocation checks.
func verifyClientCert(rawCerts [][]byte, revoked revokedSet) error {
	if len(rawCerts) == 0 {
		return errors.New("browse tls: no client certificate presented")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("browse tls: parse client cert: %w", err)
	}
	if err := requireP256(leaf.PublicKey); err != nil {
		return err
	}
	if revoked[leaf.SerialNumber.String()] {
		return fmt.Errorf("browse tls: client certificate revoked (serial %s)", leaf.SerialNumber)
	}
	return nil
}

// requireP256 fails unless the public key is an ECDSA key on the P-256 curve.
func requireP256(pub any) error {
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("public key is not ECDSA")
	}
	if ec.Curve != elliptic.P256() {
		return errors.New("public key is not on curve P-256")
	}
	return nil
}

// parseLeaf parses the first certificate in a DER bundle.
func parseLeaf(der [][]byte) (*x509.Certificate, error) {
	if len(der) == 0 {
		return nil, errors.New("no certificate in bundle")
	}
	return x509.ParseCertificate(der[0])
}
