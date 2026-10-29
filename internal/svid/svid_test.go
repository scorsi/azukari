package svid

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/testca"
)

const (
	trustDomain = testca.TrustDomain
	serverID    = "spiffe://example.org/store-host/azukari-server"
	nodeID      = "spiffe://example.org/node-a/azukari-backup"
)

func source(t *testing.T, ca *testca.CA, id string) *Source {
	t.Helper()
	s, err := Load(ca.SVID(t, id))
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
	ca := testca.New(t)
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	other, _ := url.Parse("spiffe://example.org/node-b/azukari-backup")

	cases := []struct {
		name string
		ca   *testca.CA
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
		{name: "another CA", ca: testca.New(t), id: nodeID, eku: x509.ExtKeyUsageClientAuth, want: "unknown authority"},
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
			cert, _ := issuer.Issue(t, tc.id, tc.mod)
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
	ca := testca.New(t)
	cert, _ := ca.Issue(t, nodeID, nil)
	_, key := ca.Issue(t, nodeID, nil)
	if _, err := Load(testca.Files(t, cert, key, ca.PEM)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRefusesEmptyBundle(t *testing.T) {
	ca := testca.New(t)
	cert, key := ca.Issue(t, nodeID, nil)
	if _, err := Load(testca.Files(t, cert, key, []byte("garbage"))); err == nil || !strings.Contains(err.Error(), "no certificate") {
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
	ca := testca.New(t)
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
		stranger := testca.New(t)
		cert, key := stranger.Issue(t, nodeID, nil)
		s, err := Load(testca.Files(t, cert, key, ca.PEM))
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
	oldCA := testca.New(t)
	cert, key := oldCA.Issue(t, serverID, nil)
	f := testca.Files(t, cert, key, oldCA.PEM)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := store.ServerConfig() // built once, as the server does
	first := store.NotAfter()

	// The CA rolls: a new SVID and a bundle trusting both CAs.
	nextCA := testca.New(t)
	cert, key = nextCA.Issue(t, serverID, func(c *x509.Certificate) { c.NotAfter = first.Add(time.Hour) })
	testca.Write(t, f.SVIDFile, cert)
	testca.Write(t, f.KeyFile, key)
	testca.Write(t, f.BundleFile, append(append([]byte{}, oldCA.PEM...), nextCA.PEM...))
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if !store.NotAfter().Equal(first.Add(time.Hour).Truncate(time.Second)) {
		t.Fatalf("NotAfter %v, want %v", store.NotAfter(), first.Add(time.Hour))
	}
	// A client of the new CA, trusting only it, sees the new certificate and is accepted.
	nodeCert, nodeKey := nextCA.Issue(t, nodeID, nil)
	node, err := Load(testca.Files(t, nodeCert, nodeKey, nextCA.PEM))
	if err != nil {
		t.Fatal(err)
	}
	if srvErr, cliErr, _ := handshake(t, srvCfg, node.ClientConfig(serverID)); srvErr != nil || cliErr != nil {
		t.Fatalf("after reload: server %v, client %v", srvErr, cliErr)
	}

	// A failing reload (key not matching) keeps the material.
	_, otherKey := nextCA.Issue(t, serverID, nil)
	testca.Write(t, f.KeyFile, otherKey)
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
