// Package svid loads a node's X.509-SVID files and verifies SPIFFE peers (ADR 0005, 0011):
// written once, used by azukari-server and azukari-backup.
package svid

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/scorsi/azukari/internal/config"
)

// Source holds the current SVID, key and bundle; Reload swaps them atomically.
type Source struct {
	files config.TLS
	cur   atomic.Pointer[material]
}

type material struct {
	cert  tls.Certificate // Leaf is set
	roots *x509.CertPool
}

// Load reads svid_file, key_file and bundle_file; a key not matching the certificate is an error.
func Load(files config.TLS) (*Source, error) {
	s := &Source{files: files}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the files. On error the previous material stays in use; open connections
// are never touched, new handshakes use the new material.
func (s *Source) Reload() error {
	m, err := load(s.files)
	if err != nil {
		return err
	}
	s.cur.Store(m)
	return nil
}

// NotAfter is the current SVID's expiry (svid-reloaded, azukari_svid_expiry_timestamp_seconds).
func (s *Source) NotAfter() time.Time {
	return s.cur.Load().cert.Leaf.NotAfter
}

func load(f config.TLS) (*material, error) {
	certPEM, err := os.ReadFile(f.SVIDFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(f.KeyFile)
	if err != nil {
		return nil, err
	}
	bundlePEM, err := os.ReadFile(f.BundleFile)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s, %s: %w", f.SVIDFile, f.KeyFile, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundlePEM) {
		return nil, fmt.Errorf("%s: no certificate", f.BundleFile)
	}
	return &material{cert: cert, roots: roots}, nil
}

// ServerConfig requires a client SVID from the trust domain, verified against the current
// bundle with client auth; the certificate served is the current one.
func (s *Source) ServerConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
		ClientAuth: tls.RequireAnyClientCert, // verified in VerifyConnection, against the current bundle
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return &s.cur.Load().cert, nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, _, err := Verify(cs.PeerCertificates, s.cur.Load().roots, x509.ExtKeyUsageClientAuth, s.files.TrustDomain)
			return err
		},
	}
}

// ClientConfig presents the current SVID and requires the server's ID to equal serverID;
// no DNS name is checked.
func (s *Source) ClientConfig(serverID string) *tls.Config {
	m := s.cur.Load()
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{m.cert},
		// The default verification checks a DNS name; VerifyConnection replaces it.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			id, _, err := Verify(cs.PeerCertificates, m.roots, x509.ExtKeyUsageServerAuth, s.files.TrustDomain)
			if err != nil {
				return err
			}
			if id != serverID {
				return fmt.Errorf("server is %s, want %s", id, serverID)
			}
			return nil
		},
	}
}

// Verify checks a peer's chain (leaf first): it verifies against roots with eku, the leaf is
// not a CA and holds exactly one URI SAN, a SPIFFE ID in trustDomain. It returns the ID and
// its <host> segment.
func Verify(chain []*x509.Certificate, roots *x509.CertPool, eku x509.ExtKeyUsage, trustDomain string) (id, host string, err error) {
	if len(chain) == 0 {
		return "", "", errors.New("no peer certificate")
	}
	leaf := chain[0]
	if leaf.IsCA {
		return "", "", errors.New("peer certificate is a CA")
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{eku}}); err != nil {
		return "", "", err
	}
	if len(leaf.URIs) != 1 {
		return "", "", fmt.Errorf("peer certificate has %d URI SANs, want 1", len(leaf.URIs))
	}
	id = leaf.URIs[0].String()
	host, err = config.CheckID(id, trustDomain)
	if err != nil {
		return "", "", fmt.Errorf("peer ID %q %w", id, err)
	}
	return id, host, nil
}

// Peer returns the ID and <host> segment of a connection verified by ServerConfig.
func (s *Source) Peer(cs *tls.ConnectionState) (id, host string, err error) {
	if len(cs.PeerCertificates) == 0 || len(cs.PeerCertificates[0].URIs) != 1 {
		return "", "", errors.New("connection has no verified peer")
	}
	id = cs.PeerCertificates[0].URIs[0].String()
	host, err = config.CheckID(id, s.files.TrustDomain)
	return id, host, err
}
