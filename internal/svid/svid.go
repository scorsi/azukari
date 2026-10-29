// Package svid loads a node's X.509-SVID files and verifies SPIFFE peers (ADR 0005, 0011,
// 0016): written once, used by azukari-server and azukari-backup.
package svid

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/scorsi/azukari/internal/config"
)

// Source holds the current SVID, key and bundle; Reload swaps them atomically.
type Source struct {
	files config.TLS
	cur   atomic.Pointer[material]

	// OnReload, when set, is told of every load after Load: nil after a swap, the error when
	// the files are kept (svid-reloaded / svid-reload-failed). A handshake reports a failure
	// once per change of the files, Reload every time. Set it before serving.
	OnReload func(error)

	mu     sync.Mutex
	loaded stamps // files of the material in use
	failed stamps // files whose load last failed and was reported
}

// stamps identify the three files' contents by inode and modification time (ADR 0016).
type stamps [3]struct {
	ino   uint64
	mtime time.Time
}

func (s *Source) stat() stamps {
	var st stamps
	for i, p := range []string{s.files.SVIDFile, s.files.KeyFile, s.files.BundleFile} {
		// A missing file keeps the zero stamp: it differs from any loaded one, and its load fails.
		if fi, err := os.Stat(p); err == nil {
			st[i].ino = uint64(fi.Sys().(*syscall.Stat_t).Ino)
			st[i].mtime = fi.ModTime()
		}
	}
	return st
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

// Reload re-reads the files (SIGHUP). On error the previous material stays in use; open
// connections are never touched, new handshakes use the new material.
func (s *Source) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reload(s.stat(), true)
}

// refresh reloads the files when they changed since the material in use; it runs before
// each server handshake (ADR 0016).
func (s *Source) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.stat(); st != s.loaded {
		s.reload(st, false)
	}
}

// reload loads the files stamped st (taken before reading: a write after it is seen by the
// next check). Called with mu held.
func (s *Source) reload(st stamps, force bool) error {
	m, err := load(s.files)
	if err != nil {
		if force || st != s.failed {
			s.failed = st
			s.report(err)
		}
		return err
	}
	s.cur.Store(m)
	s.loaded = st
	s.report(nil)
	return nil
}

func (s *Source) report(err error) {
	if s.OnReload != nil {
		s.OnReload(err)
	}
}

// NotAfter is the current SVID's expiry (svid-reloaded, azukari_svid_expiry_timestamp_seconds).
func (s *Source) NotAfter() time.Time {
	return s.cur.Load().cert.Leaf.NotAfter
}

// ID is the current SVID's own SPIFFE ID and its <host> segment, the node's namespace in a
// backup bucket (ADR 0003, 0011).
func (s *Source) ID() (id, host string, err error) {
	leaf := s.cur.Load().cert.Leaf
	if len(leaf.URIs) != 1 {
		return "", "", fmt.Errorf("own SVID has %d URI SANs, want 1", len(leaf.URIs))
	}
	id = leaf.URIs[0].String()
	host, err = config.CheckID(id, s.files.TrustDomain)
	if err != nil {
		return "", "", fmt.Errorf("own ID %q %w", id, err)
	}
	return id, host, nil
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
// bundle with client auth; the certificate served is the current one, renewed files being
// picked up before each handshake.
func (s *Source) ServerConfig() *tls.Config {
	return &tls.Config{
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			s.refresh()
			return nil, nil // this config, now reading the refreshed material
		},
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
