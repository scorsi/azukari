// Package testca issues test SVIDs with the internal CA's profile (hanko ADR-0004): P-256, a
// single URI SAN, no DNS SAN, both EKUs. Imported by tests only (ADR 0005, 0010).
package testca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/config"
)

// TrustDomain is the trust domain of the IDs tests issue.
const TrustDomain = "example.org"

// CA is a test certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte // the bundle
}

func newKey(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// New returns a fresh CA.
func New(t testing.TB) *CA {
	t.Helper()
	k := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: "azukari test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{Cert: cert, Key: k, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Issue returns an SVID for id, PEM cert and key, after mod adjusts the template.
func (c *CA) Issue(t testing.TB, id string, mod func(*x509.Certificate)) (certPEM, keyPEM []byte) {
	t.Helper()
	u, err := url.Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	k := newKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(23 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{u},
	}
	if mod != nil {
		mod(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &k.PublicKey, c.Key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
}

// Write writes data to path, mode 0600.
func Write(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Files writes the three files into a new directory and returns their [tls] table.
func Files(t testing.TB, cert, key, bundle []byte) config.TLS {
	t.Helper()
	dir := t.TempDir()
	f := config.TLS{
		TrustDomain: TrustDomain,
		SVIDFile:    filepath.Join(dir, "svid.pem"),
		KeyFile:     filepath.Join(dir, "key.pem"),
		BundleFile:  filepath.Join(dir, "bundle.pem"),
	}
	Write(t, f.SVIDFile, cert)
	Write(t, f.KeyFile, key)
	Write(t, f.BundleFile, bundle)
	return f
}

// SVID issues an SVID for id and writes its files with this CA as the bundle.
func (c *CA) SVID(t testing.TB, id string) config.TLS {
	t.Helper()
	cert, key := c.Issue(t, id, nil)
	return Files(t, cert, key, c.PEM)
}
