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
	"sync"
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

func TestID(t *testing.T) {
	ca := testca.New(t)
	id, host, err := source(t, ca, nodeID).ID()
	if err != nil || id != nodeID || host != "node-a" {
		t.Fatalf("got %q %q %v", id, host, err)
	}
	cert, key := ca.Issue(t, "spiffe://other.example.org/node-a/azukari-backup", nil)
	s, err := Load(testca.Files(t, cert, key, ca.PEM))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ID(); err == nil || !strings.Contains(err.Error(), "outside trust domain") {
		t.Fatalf("err = %v", err)
	}
}

// rotate replaces path as the internal CA does: a staged file renamed over it (new inode).
func rotate(t *testing.T, path string, data []byte) {
	t.Helper()
	testca.Write(t, path+".new", data)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

func TestWatch(t *testing.T) {
	ca := testca.New(t)
	cert, key := ca.Issue(t, serverID, nil)
	f := testca.Files(t, cert, key, ca.PEM)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	var reports []error
	store.OnReload = func(err error) { reports = append(reports, err) }
	srvCfg := store.ServerConfig()
	node := source(t, ca, nodeID)
	first := store.NotAfter()

	served := func() time.Time {
		t.Helper()
		var got time.Time
		cli := node.ClientConfig(serverID)
		cli.VerifyConnection = func(cs tls.ConnectionState) error {
			got = cs.PeerCertificates[0].NotAfter
			return nil
		}
		if srvErr, cliErr, _ := handshake(t, srvCfg, cli); srvErr != nil || cliErr != nil {
			t.Fatalf("server %v, client %v", srvErr, cliErr)
		}
		return got
	}

	if got := served(); !got.Equal(first) || len(reports) != 0 {
		t.Fatalf("unchanged files: served %v, reports %v", got, reports)
	}

	// Renewal caught between the two renames: new certificate, old key.
	second := first.Add(time.Hour).Truncate(time.Second)
	cert, key = ca.Issue(t, serverID, func(c *x509.Certificate) { c.NotAfter = second })
	rotate(t, f.SVIDFile, cert)
	for range 2 {
		if got := served(); !got.Equal(first) {
			t.Fatalf("half-rotated: served %v, want the previous %v", got, first)
		}
	}
	if len(reports) != 1 || reports[0] == nil || !strings.Contains(reports[0].Error(), "does not match") {
		t.Fatalf("half-rotated: reports %v, want one mismatch", reports)
	}

	// key.pem lands last: the next handshake serves the new SVID, without Reload.
	rotate(t, f.KeyFile, key)
	if got := served(); !got.Equal(second) || !store.NotAfter().Equal(second) {
		t.Fatalf("rotated: served %v, NotAfter %v, want %v", got, store.NotAfter(), second)
	}
	if len(reports) != 2 || reports[1] != nil {
		t.Fatalf("rotated: reports %v, want one success", reports)
	}
	served()
	if len(reports) != 2 {
		t.Fatalf("unchanged files reloaded again: %v", reports)
	}
}

// node loads a client SVID issued by ca, trusting bundle.
func node(t *testing.T, ca *testca.CA, bundle []byte) *Source {
	t.Helper()
	cert, key := ca.Issue(t, nodeID, nil)
	s, err := Load(testca.Files(t, cert, key, bundle))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func concat(bundles ...[]byte) []byte {
	var b []byte
	for _, x := range bundles {
		b = append(b, x...)
	}
	return b
}

// TestWatchBundle replaces the CA as the internal CA does, with real verification on both
// sides: a new bundle verifies new handshakes (ADR 0005, 0016).
func TestWatchBundle(t *testing.T) {
	oldCA, newCA := testca.New(t), testca.New(t)
	cert, key := oldCA.Issue(t, serverID, nil)
	f := testca.Files(t, cert, key, oldCA.PEM)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	var reports []error
	store.OnReload = func(err error) { reports = append(reports, err) }
	srvCfg := store.ServerConfig()

	// These two trust both CAs: a refusal is the server's.
	oldNode, newNode := node(t, oldCA, concat(oldCA.PEM, newCA.PEM)), node(t, newCA, concat(oldCA.PEM, newCA.PEM))
	// This one trusts only the new CA: accepted means the server serves the new SVID.
	newOnly := node(t, newCA, newCA.PEM)
	want := func(step string, n *Source, accepted bool, refusedBy string) {
		t.Helper()
		srvErr, cliErr, _ := handshake(t, srvCfg, n.ClientConfig(serverID))
		switch {
		case accepted && (srvErr != nil || cliErr != nil):
			t.Fatalf("%s: server %v, client %v", step, srvErr, cliErr)
		case !accepted && refusedBy == "server" && (srvErr == nil || !strings.Contains(srvErr.Error(), "unknown authority")):
			t.Fatalf("%s: server err = %v, want unknown authority", step, srvErr)
		case !accepted && refusedBy == "client" && (cliErr == nil || !strings.Contains(cliErr.Error(), "unknown authority")):
			t.Fatalf("%s: client err = %v, want unknown authority", step, cliErr)
		}
	}

	want("start", oldNode, true, "")
	want("start", newNode, false, "server")
	want("start", newOnly, false, "client")

	// The bundle alone grows to trust both CAs: clients of the new CA are accepted.
	rotate(t, f.BundleFile, concat(oldCA.PEM, newCA.PEM))
	want("bundle grown", newNode, true, "")
	want("bundle grown", oldNode, true, "")
	if len(reports) != 1 || reports[0] != nil {
		t.Fatalf("bundle grown: reports %v", reports)
	}

	// The CA rolls: SVID, bundle (new CA only), key.pem last.
	cert, key = newCA.Issue(t, serverID, nil)
	rotate(t, f.SVIDFile, cert)
	rotate(t, f.BundleFile, newCA.PEM)
	// Caught between the renames: the previous SVID and bundle stay whole.
	want("half-rotated", oldNode, true, "")
	want("half-rotated", newNode, true, "")
	want("half-rotated", newOnly, false, "client")
	if len(reports) != 2 || reports[1] == nil {
		t.Fatalf("half-rotated: reports %v, want one failure", reports)
	}
	rotate(t, f.KeyFile, key)
	want("rotated", newOnly, true, "")
	want("rotated", newNode, true, "")
	want("rotated", oldNode, false, "server")
	if len(reports) != 3 || reports[2] != nil {
		t.Fatalf("rotated: reports %v, want one success", reports)
	}
}

// TestWatchMalformedBundle: a bundle that does not parse keeps the previous material and is
// reported once per change of the files.
func TestWatchMalformedBundle(t *testing.T) {
	ca := testca.New(t)
	f := ca.SVID(t, serverID)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	var reports []error
	store.OnReload = func(err error) { reports = append(reports, err) }
	srvCfg := store.ServerConfig()
	n := node(t, ca, ca.PEM)
	ok := func(step string) {
		t.Helper()
		if srvErr, cliErr, _ := handshake(t, srvCfg, n.ClientConfig(serverID)); srvErr != nil || cliErr != nil {
			t.Fatalf("%s: server %v, client %v", step, srvErr, cliErr)
		}
	}
	b, _ := pem.Decode(ca.PEM)
	truncated := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Bytes[:len(b.Bytes)/2]})

	for i, bad := range [][]byte{truncated, []byte("garbage")} {
		rotate(t, f.BundleFile, bad)
		for range 3 {
			ok("malformed bundle")
		}
		if len(reports) != i+1 || reports[i] == nil || !strings.Contains(reports[i].Error(), "no certificate") {
			t.Fatalf("malformed bundle %d: reports %v, want one more failure", i, reports)
		}
	}
	if err := store.Reload(); err == nil || len(reports) != 3 {
		t.Fatalf("Reload: %v, reports %v (SIGHUP reports every time)", err, reports)
	}
	rotate(t, f.BundleFile, ca.PEM)
	ok("bundle restored")
	if len(reports) != 4 || reports[3] != nil {
		t.Fatalf("restored: reports %v", reports)
	}
}

// TestSwapDuringHandshake: a swap landing between the certificate the server sent and the
// client's certificate does not change the material of that handshake (ADR 0016: open
// connections keep their handshake, a new bundle verifies new handshakes).
func TestSwapDuringHandshake(t *testing.T) {
	oldCA, newCA := testca.New(t), testca.New(t)
	f := oldCA.SVID(t, serverID)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := store.ServerConfig()
	n := node(t, oldCA, oldCA.PEM)
	cli := n.ClientConfig(serverID)
	cli.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		cert, key := newCA.Issue(t, serverID, nil)
		rotate(t, f.SVIDFile, cert)
		rotate(t, f.BundleFile, newCA.PEM)
		rotate(t, f.KeyFile, key)
		if err := store.Reload(); err != nil {
			t.Error(err)
		}
		return &cli.Certificates[0], nil
	}
	if srvErr, cliErr, _ := handshake(t, srvCfg, cli); srvErr != nil || cliErr != nil {
		t.Fatalf("handshake begun on the old material: server %v, client %v", srvErr, cliErr)
	}
	// The next handshake uses the new material.
	if srvErr, _, _ := handshake(t, srvCfg, n.ClientConfig(serverID)); srvErr == nil {
		t.Fatal("old CA's client accepted after the swap")
	}
}

// TestConcurrentRenewal runs handshakes while the files are replaced (staged, key.pem last)
// and Reload is called; run with -race. Both CAs stay trusted, so every handshake succeeds
// whatever material it lands on, half-rotated files included.
func TestConcurrentRenewal(t *testing.T) {
	cas := []*testca.CA{testca.New(t), testca.New(t)}
	bundle := concat(cas[0].PEM, cas[1].PEM)
	var pairs [2][2][]byte
	for i, ca := range cas {
		pairs[i][0], pairs[i][1] = ca.Issue(t, serverID, nil)
	}
	f := testca.Files(t, pairs[0][0], pairs[0][1], bundle)
	store, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reloads, failures int
	store.OnReload = func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if reloads++; err != nil {
			failures++
		}
	}
	srvCfg := store.ServerConfig()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				tc := tls.Server(c, srvCfg)
				err := tc.Handshake()
				if err == nil {
					_, err = tc.Write([]byte{1})
				}
				if err != nil {
					t.Errorf("server: %v", err)
				}
			}()
		}
	}()

	clients := []*Source{node(t, cas[0], bundle), node(t, cas[1], bundle)}
	stop := make(chan struct{})
	var workers sync.WaitGroup
	for w := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			cli := clients[w%2].ClientConfig(serverID)
			for {
				select {
				case <-stop:
					return
				default:
				}
				c, err := net.Dial("tcp", lis.Addr().String())
				if err != nil {
					t.Error(err)
					return
				}
				tc := tls.Client(c, cli)
				err = tc.Handshake()
				if err == nil {
					_, err = tc.Read(make([]byte, 1))
				}
				c.Close()
				if err != nil {
					t.Errorf("client: %v", err)
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				store.Reload() // fails while half-rotated: the previous material stays
				store.NotAfter()
				store.ID()
			}
		}
	}()
	for i := range 40 {
		p := pairs[(i+1)%2]
		rotate(t, f.SVIDFile, p[0])
		time.Sleep(time.Millisecond)
		rotate(t, f.KeyFile, p[1])
		time.Sleep(time.Millisecond)
	}
	close(stop)
	workers.Wait()
	lis.Close()
	wg.Wait()

	// The last pair is pairs[0]: the next handshake serves it.
	only := node(t, cas[0], cas[0].PEM)
	if srvErr, cliErr, _ := handshake(t, srvCfg, only.ClientConfig(serverID)); srvErr != nil || cliErr != nil {
		t.Fatalf("after renewals: server %v, client %v", srvErr, cliErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if reloads <= failures {
		t.Fatalf("%d reloads, %d failed", reloads, failures)
	}
}
