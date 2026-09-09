package browse

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testPKI is a throwaway CA + server/client certs used by the mTLS config
// tests. It is not real PKI — just enough to exercise tls.go.
type testPKI struct {
	dir          string
	clientSerial *big.Int
}

// makeCert builds a self-signed or CA-signed ECDSA P-256 cert and writes it
// with its key. signer is nil for a self-signed (CA) cert.
func makeCert(t *testing.T, dir, name string, signer *ecdsa.PrivateKey, ou string, serial *big.Int, curve elliptic.Curve) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyUsage := x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign
	if signer == nil {
		keyUsage |= x509.KeyUsageCRLSign // CA signs the revocation list
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name, OrganizationalUnit: []string{ou}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              keyUsage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:                  signer == nil,
		BasicConstraintsValid: true,
	}
	ca := tmpl
	if signer != nil {
		ca = signerCert(t, dir)
	}
	// For a self-signed CA (signer == nil) the private key signs the cert.
	priv := signer
	if priv == nil {
		priv = key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, name+".crt"), "CERTIFICATE", der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, name+".key"), "PRIVATE KEY", keyDER)
	return key, cert
}

// signerCert caches the CA cert for signing (reads from disk).
func signerCert(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM in ca.crt")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// newTestPKI generates ca, server, client, and an RSA (non-P256) client cert,
// plus a CRL revoking the client cert. Returns a temp dir with the files.
func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, caCert := makeCert(t, dir, "ca", nil, "ca", big.NewInt(1), elliptic.P256())
	_ = caKey
	_ = caCert
	_, _ = makeCert(t, dir, "server", loadKey(t, dir, "ca"), "server", big.NewInt(2), elliptic.P256())
	clientSerial := big.NewInt(3)
	_, _ = makeCert(t, dir, "client", loadKey(t, dir, "ca"), "device", clientSerial, elliptic.P256())
	// RSA client cert — wrong curve, must be rejected by requireP256.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4),
		Subject:      pkix.Name{CommonName: "rsa-client", OrganizationalUnit: []string{"device"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	rsaDER, err := x509.CreateCertificate(rand.Reader, rsaTmpl, signerCert(t, dir), &rsaKey.PublicKey, loadKey(t, dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, "rsa-client.crt"), "CERTIFICATE", rsaDER)

	// CRL revoking the client cert (serial 3).
	crlTmpl := &x509.RevocationList{
		Number:     big.NewInt(1),
		ThisUpdate: time.Now().Add(-time.Hour),
		NextUpdate: time.Now().Add(24 * time.Hour),
		RevokedCertificateEntries: []x509.RevocationListEntry{
			{SerialNumber: clientSerial, RevocationTime: time.Now()},
		},
	}
	crlDER, err := x509.CreateRevocationList(rand.Reader, crlTmpl, signerCert(t, dir), loadKey(t, dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, filepath.Join(dir, "revoked.crl"), "X509 CRL", crlDER)

	return &testPKI{dir: dir, clientSerial: clientSerial}
}

// paths returns the TLSPaths referencing this PKI's files.
func (p *testPKI) paths() TLSPaths {
	return TLSPaths{
		Cert:           filepath.Join(p.dir, "server.crt"),
		Key:            filepath.Join(p.dir, "server.key"),
		ClientCA:       filepath.Join(p.dir, "ca.crt"),
		RevocationList: filepath.Join(p.dir, "revoked.crl"),
	}
}

func loadKey(t *testing.T, dir, name string) *ecdsa.PrivateKey {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name+".key"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM in key file")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("not an ECDSA key")
	}
	return ec
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
