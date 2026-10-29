package svid

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/config"
)

const (
	trustDomain = "example.org"
	serverID    = "spiffe://example.org/store-host/azukari-server"
	nodeID      = "spiffe://example.org/node-a/azukari-backup"
)

// testCA issues SVIDs with the internal CA's profile (hanko ADR-0004): P-256, a single URI
// SAN, no DNS SAN, both EKUs. Copied pattern from hanko's internal/testutil.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func newCA(t *testing.T) *testCA {
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
	return &testCA{cert: cert, key: k, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns an SVID for id, PEM cert and key, after mod adjusts the template.
func (c *testCA) issue(t *testing.T, id string, mod func(*x509.Certificate)) (certPEM, keyPEM []byte) {
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &k.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// files writes the three files into a new directory and returns their [tls] table.
func files(t *testing.T, cert, key, bundle []byte) config.TLS {
	t.Helper()
	dir := t.TempDir()
	f := config.TLS{
		TrustDomain: trustDomain,
		SVIDFile:    filepath.Join(dir, "svid.pem"),
		KeyFile:     filepath.Join(dir, "key.pem"),
		BundleFile:  filepath.Join(dir, "bundle.pem"),
	}
	write(t, f.SVIDFile, cert)
	write(t, f.KeyFile, key)
	write(t, f.BundleFile, bundle)
	return f
}

func source(t *testing.T, ca *testCA, id string) *Source {
	t.Helper()
	cert, key := ca.issue(t, id, nil)
	s, err := Load(files(t, cert, key, ca.pem))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerify(t *testing.T) {
	ca := newCA(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	other, _ := url.Parse("spiffe://example.org/node-b/azukari-backup")

	cases := []struct {
		name string
		ca   *testCA
		id   string
		mod  func(*x509.Certificate)
		eku  x509.ExtKeyUsage
		want string // error substring, "" for success
	}{
		{name: "valid client", id: nodeID, eku: x509.ExtKeyUsageClientAuth},
		{name: "valid server", id: serverID, eku: x509.ExtKeyUsageServerAuth},
		{name: "wrong trust domain", id: "spiffe://other.example.org/node-a/azukari-backup", eku: x509.ExtKeyUsageClientAuth, want: "outside trust domain"},
		{name: "not a SPIFFE ID", id: "spiffe://example.org/node-a", eku: x509.ExtKeyUsageClientAuth, want: "is not spiffe://"},
		{name: "two URI SANs", id: nodeID, eku: x509.ExtKeyUsageClientAuth, want: "2 URI SANs",
			mod: func(c *x509.Certificate) { c.URIs = append(c.URIs, other) }},
		{name: "CA leaf", id: nodeID, eku: x509.ExtKeyUsageClientAuth, want: "is a CA",
			mod: func(c *x509.Certificate) { c.IsCA = true; c.KeyUsage |= x509.KeyUsageCertSign }},
		{name: "expired", id: nodeID, eku: x509.ExtKeyUsageClientAuth, want: "expired",
			mod: func(c *x509.Certificate) {
				c.NotBefore = time.Now().Add(-48 * time.Hour)
				c.NotAfter = time.Now().Add(-time.Hour)
			}},
		{name: "another CA", ca: newCA(t), id: nodeID, eku: x509.ExtKeyUsageClientAuth, want: "unknown authority"},
		{name: "server auth only as client", id: nodeID, eku: x509.ExtKeyUsageClientAuth, want: "key usage",
			mod: func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }},
		{name: "client auth only as server", id: serverID, eku: x509.ExtKeyUsageServerAuth, want: "key usage",
			mod: func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issuer := ca
			if tc.ca != nil {
				issuer = tc.ca
			}
			cert, _ := issuer.issue(t, tc.id, tc.mod)
			id, host, err := Verify([]*x509.Certificate{parse(t, cert)}, roots, tc.eku, trustDomain)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if id != tc.id || !strings.HasPrefix(tc.id, "spiffe://example.org/"+host+"/") {
					t.Fatalf("got %q %q", id, host)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, _, err := Verify(nil, roots, x509.ExtKeyUsageClientAuth, trustDomain); err == nil {
		t.Fatal("empty chain accepted")
	}
}

func TestLoadRefusesMismatchedKey(t *testing.T) {
	ca := newCA(t)
	cert, _ := ca.issue(t, nodeID, nil)
	_, key := ca.issue(t, nodeID, nil)
	if _, err := Load(files(t, cert, key, ca.pem)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRefusesEmptyBundle(t *testing.T) {
	ca := newCA(t)
	cert, key := ca.issue(t, nodeID, nil)
	if _, err := Load(files(t, cert, key, []byte("garbage"))); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("err = %v", err)
	}
}

// handshake runs one TLS handshake over loopback and returns both sides' errors and the
// server's view of the connection.
func handshake(t *testing.T, srv, cli *tls.Config) (srvErr, cliErr error, state tls.ConnectionState) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	type result struct {
		err   error
		state tls.ConnectionState
	}
	done := make(chan result, 1)
	go func() {
		c, err := lis.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer c.Close()
		tc := tls.Server(c, srv)
		err = tc.Handshake()
		if err == nil {
			// TLS 1.3: the client's certificate is checked after the client's Handshake
			// returns; a read round trip surfaces a refusal on both sides.
			_, err = tc.Write([]byte{1})
		}
		done <- result{err: err, state: tc.ConnectionState()}
	}()
	c, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tc := tls.Client(c, cli)
	cliErr = tc.Handshake()
	if cliErr == nil {
		_, cliErr = tc.Read(make([]byte, 1))
	}
	tc.Close()
	r := <-done
	return r.err, cliErr, r.state
}

func TestHandshake(t *testing.T) {
	ca := newCA(t)
	store := source(t, ca, serverID)
	node := source(t, ca, nodeID)

	cli := node.ClientConfig(serverID)
	cli.NextProtos = []string{"h2"}
	srvErr, cliErr, state := handshake(t, store.ServerConfig(), cli)
	if srvErr != nil || cliErr != nil {
		t.Fatalf("server %v, client %v", srvErr, cliErr)
	}
	if state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != "h2" {
		t.Fatalf("version %x, ALPN %q", state.Version, state.NegotiatedProtocol)
	}
	if id, host, err := store.Peer(&state); err != nil || id != nodeID || host != "node-a" {
		t.Fatalf("peer %q %q %v", id, host, err)
	}

	t.Run("server ID mismatch", func(t *testing.T) {
		_, cliErr, _ := handshake(t, store.ServerConfig(), node.ClientConfig("spiffe://example.org/other-host/azukari-server"))
		if cliErr == nil || !strings.Contains(cliErr.Error(), "want spiffe://example.org/other-host/azukari-server") {
			t.Fatalf("client err = %v", cliErr)
		}
	})
	t.Run("client from another CA", func(t *testing.T) {
		stranger := newCA(t)
		cert, key := stranger.issue(t, nodeID, nil)
		s, err := Load(files(t, cert, key, ca.pem))
		if err != nil {
			t.Fatal(err)
		}
		srvErr, _, _ := handshake(t, store.ServerConfig(), s.ClientConfig(serverID))
		if srvErr == nil || !strings.Contains(srvErr.Error(), "unknown authority") {
			t.Fatalf("server err = %v", srvErr)
		}
	})
	t.Run("client without certificate", func(t *testing.T) {
		cli := node.ClientConfig(serverID)
		cli.Certificates = nil
		if srvErr, _, _ := handshake(t, store.ServerConfig(), cli); srvErr == nil {
			t.Fatal("server accepted a client without certificate")
		}
	})
}

func TestReload(t *testing.T) {
	oldCA := newCA(t)
	cert, key := oldCA.issue(t, serverID, nil)
	f := files(t, cert, key, oldCA.pem)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := store.ServerConfig() // built once, as the server does
	first := store.NotAfter()

	// The CA rolls: a new SVID and a bundle trusting both CAs.
	nextCA := newCA(t)
	cert, key = nextCA.issue(t, serverID, func(c *x509.Certificate) { c.NotAfter = first.Add(time.Hour) })
	write(t, f.SVIDFile, cert)
	write(t, f.KeyFile, key)
	write(t, f.BundleFile, append(append([]byte{}, oldCA.pem...), nextCA.pem...))
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if !store.NotAfter().Equal(first.Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("NotAfter %v, want %v", store.NotAfter(), first.Add(time.Hour))
	}
	// A client of the new CA, trusting only it, sees the new certificate and is accepted.
	nodeCert, nodeKey := nextCA.issue(t, nodeID, nil)
	node, err := Load(files(t, nodeCert, nodeKey, nextCA.pem))
	if err != nil {
		t.Fatal(err)
	}
	if srvErr, cliErr, _ := handshake(t, srvCfg, node.ClientConfig(serverID)); srvErr != nil || cliErr != nil {
		t.Fatalf("after reload: server %v, client %v", srvErr, cliErr)
	}

	// A failing reload (key not matching) keeps the material.
	_, otherKey := nextCA.issue(t, serverID, nil)
	write(t, f.KeyFile, otherKey)
	if err := store.Reload(); err == nil {
		t.Fatal("reload with a mismatched key succeeded")
	}
	if err := os.Remove(f.BundleFile); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reload without bundle: %v", err)
	}
	if !store.NotAfter().Equal(first.Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("NotAfter changed after failed reloads: %v", store.NotAfter())
	}
	if srvErr, cliErr, _ := handshake(t, srvCfg, node.ClientConfig(serverID)); srvErr != nil || cliErr != nil {
		t.Fatalf("after failed reload: server %v, client %v", srvErr, cliErr)
	}
}
