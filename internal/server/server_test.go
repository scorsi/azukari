package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/svid"
	"github.com/scorsi/azukari/internal/testca"
)

const (
	serverID = "spiffe://example.org/store-host/azukari-server"
	nodeA    = "spiffe://example.org/node-a/azukari-backup"
	stranger = "spiffe://example.org/node-z/azukari-backup"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

type env struct {
	ca        *testca.CA
	addr      string
	logs      *syncBuffer
	snapshots *syncBuffer // one "<bucket> <namespace>" line per OnSnapshot call
	srv       *Server
	cfg       *config.Server
	tls       config.TLS
}

// start serves three buckets on loopback: "backup" (max 1024 bytes), "down" (its mount is
// missing: unavailable) and "full" (reserve above any free space).
func start(t *testing.T) *env {
	t.Helper()
	ca := testca.New(t)
	dir := t.TempDir()
	bkt := func(name string, reserve string) config.Bucket {
		mount := filepath.Join(dir, name)
		if err := os.Mkdir(mount, 0o700); err != nil {
			t.Fatal(err)
		}
		return config.Bucket{Name: name, Type: "backup", Path: filepath.Join(mount, "b"), MountPoint: mount,
			MaxObjectBytes: 1024, DiskReserve: reserve, Nodes: []string{nodeA},
			Retention: config.Retention{Keep: "10d", Grace: "2d", RunAt: "03:30"}}
	}
	cfg := &config.Server{Buckets: []config.Bucket{bkt("backup", "0B"), bkt("down", "0B"), bkt("full", "1000TiB")}}
	files := ca.SVID(t, serverID)
	src, err := svid.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	isMount := func(p string) (bool, error) { return p != filepath.Join(dir, "down"), nil }
	s, err := New(cfg, src, logging.New(logs, slog.LevelInfo), isMount)
	if err != nil {
		t.Fatal(err)
	}
	snapshots := &syncBuffer{}
	record := s.OnSnapshot
	s.OnSnapshot = func(bucket, namespace string, at time.Time) {
		fmt.Fprintln(snapshots, bucket, namespace)
		record(bucket, namespace, at)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, []net.Listener{ln}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	for !strings.Contains(logs.String(), `"server-started"`) {
		time.Sleep(time.Millisecond)
	}
	return &env{ca: ca, addr: ln.Addr().String(), logs: logs, snapshots: snapshots, srv: s, cfg: cfg, tls: files}
}

func (e *env) tlsConfig(t *testing.T, id string, alpn ...string) *tls.Config {
	t.Helper()
	src, err := svid.Load(e.ca.SVID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	c := src.ClientConfig(serverID)
	c.NextProtos = alpn
	return c
}

// client speaks HTTP/2 when h2, else HTTP/1.1 only.
func (e *env) client(t *testing.T, id string, h2 bool) *http.Client {
	t.Helper()
	tr := &http.Transport{TLSClientConfig: e.tlsConfig(t, id), ForceAttemptHTTP2: h2}
	if !h2 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

type answer struct {
	code   int
	reason string
	body   []byte
	resp   *http.Response
}

func (e *env) do(t *testing.T, c *http.Client, method, path string, body io.Reader, sum string) answer {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+e.addr+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "" {
		req.Header.Set("Azukari-Sha256", sum)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	a := answer{code: resp.StatusCode, resp: resp}
	if a.body, err = io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 400 && resp.Header.Get("Content-Type") == "application/x-protobuf" {
		var m azukariv1.Error
		if err := proto.Unmarshal(a.body, &m); err != nil {
			t.Fatal(err)
		}
		a.reason = m.Reason
	}
	return a
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestPutGet(t *testing.T) {
	e := start(t)
	for _, h2 := range []bool{false, true} {
		c := e.client(t, nodeA, h2)
		wantProto := map[bool]string{false: "HTTP/1.1", true: "HTTP/2.0"}[h2]
		path := "/v1/b/backup/o/node-a/chunks/" + map[bool]string{false: "h1", true: "h2"}[h2]
		data := []byte("chunk bytes " + wantProto)

		a := e.do(t, c, "PUT", path, bytes.NewReader(data), hexSum(data))
		if a.code != http.StatusCreated || a.resp.Proto != wantProto {
			t.Fatalf("put: %d %q over %s, want 201 over %s", a.code, a.reason, a.resp.Proto, wantProto)
		}
		if a := e.do(t, c, "PUT", path, bytes.NewReader(data), hexSum(data)); a.code != http.StatusOK {
			t.Fatalf("retry: %d %q", a.code, a.reason)
		}
		other := []byte("other bytes")
		if a := e.do(t, c, "PUT", path, bytes.NewReader(other), hexSum(other)); a.code != http.StatusConflict || a.reason != "object-exists" {
			t.Fatalf("conflict: %d %q", a.code, a.reason)
		}
		a = e.do(t, c, "GET", path, nil, "")
		if a.code != http.StatusOK || !bytes.Equal(a.body, data) || a.resp.ContentLength != int64(len(data)) ||
			a.resp.Header.Get("Azukari-Sha256") != hexSum(data) || a.resp.Proto != wantProto {
			t.Fatalf("get: %d %q %q cl=%d sum=%q", a.code, a.reason, a.body, a.resp.ContentLength, a.resp.Header.Get("Azukari-Sha256"))
		}
		// An empty object is an object.
		if a := e.do(t, c, "PUT", path+"-empty", bytes.NewReader(nil), hexSum(nil)); a.code != http.StatusCreated {
			t.Fatalf("empty put: %d %q", a.code, a.reason)
		}
	}

	var request, refused map[string]any
	for _, l := range e.logs.lines(t) {
		switch {
		case l["event"] == "request" && l["call"] == "put" && l["code"] == 201.0 && request == nil:
			request = l
		case l["event"] == "request-refused" && refused == nil:
			refused = l
		}
	}
	for _, l := range []map[string]any{request, refused} {
		if l == nil {
			t.Fatal("request or request-refused line missing")
		}
		for _, f := range []string{"bucket", "peer_id", "call", "key", "code", "bytes", "dur_us"} {
			if _, ok := l[f]; !ok {
				t.Errorf("%s line lacks %s: %v", l["event"], f, l)
			}
		}
	}
	if request["peer_id"] != nodeA || request["bucket"] != "backup" || request["bytes"] == 0.0 || request["level"] != "info" {
		t.Errorf("request line %v", request)
	}
	if refused["reason"] != "object-exists" || refused["level"] != "warn" || refused["code"] != 409.0 {
		t.Errorf("request-refused line %v", refused)
	}
}

func TestRefusals(t *testing.T) {
	e := start(t)
	c := e.client(t, nodeA, true)
	data := []byte("data")
	big := make([]byte, 1025)
	cases := []struct {
		name, method, path string
		body               []byte
		sum                string
		code               int
		reason             string
	}{
		{"sha256 missing", "PUT", "/v1/b/backup/o/node-a/x", data, "", 400, "sha256-missing"},
		{"sha256 not hex", "PUT", "/v1/b/backup/o/node-a/x", data, "zz", 400, "request-invalid"},
		{"sha256 too long", "PUT", "/v1/b/backup/o/node-a/x", data, hexSum(data) + "00", 400, "request-invalid"},
		{"sha256 too short", "PUT", "/v1/b/backup/o/node-a/x", data, hexSum(data)[2:], 400, "request-invalid"},
		{"sha256 mismatch", "PUT", "/v1/b/backup/o/node-a/x", data, hexSum([]byte("else")), 400, "sha256-mismatch"},
		{"too large", "PUT", "/v1/b/backup/o/node-a/x", big, hexSum(big), 413, "object-too-large"},
		{"key invalid", "PUT", "/v1/b/backup/o/node-a/b@d", data, hexSum(data), 400, "key-invalid"},
		{"foreign namespace", "PUT", "/v1/b/backup/o/node-b/x", data, hexSum(data), 403, "namespace-forbidden"},
		{"foreign namespace get", "GET", "/v1/b/backup/o/node-b/x", nil, "", 403, "namespace-forbidden"},
		{"unknown bucket", "PUT", "/v1/b/nope/o/node-a/x", data, hexSum(data), 404, "bucket-unknown"},
		{"unknown object", "GET", "/v1/b/backup/o/node-a/nope", nil, "", 404, "object-unknown"},
		{"unavailable bucket", "PUT", "/v1/b/down/o/node-a/x", data, hexSum(data), 503, "bucket-unavailable"},
		{"unavailable bucket get", "GET", "/v1/b/down/o/node-a/x", nil, "", 503, "bucket-unavailable"},
		{"disk reserve", "PUT", "/v1/b/full/o/node-a/x", data, hexSum(data), 507, "disk-reserve"},
		{"no delete", "DELETE", "/v1/b/backup/o/node-a/x", nil, "", 400, "request-invalid"},
		{"unknown path", "GET", "/v1/other", nil, "", 400, "request-invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			}
			if a := e.do(t, c, tc.method, tc.path, body, tc.sum); a.code != tc.code || a.reason != tc.reason {
				t.Fatalf("got %d %q, want %d %q", a.code, a.reason, tc.code, tc.reason)
			}
		})
	}

	t.Run("unknown ID", func(t *testing.T) {
		for _, h2 := range []bool{false, true} {
			if a := e.do(t, e.client(t, stranger, h2), "GET", "/v1/b/backup/o/node-z/x", nil, ""); a.code != 403 || a.reason != "forbidden" {
				t.Fatalf("got %d %q", a.code, a.reason)
			}
		}
	})
	t.Run("length required", func(t *testing.T) {
		for _, h2 := range []bool{false, true} {
			// A reader of unknown length: chunked on HTTP/1.1, no content-length on HTTP/2.
			a := e.do(t, e.client(t, nodeA, h2), "PUT", "/v1/b/backup/o/node-a/x", io.MultiReader(bytes.NewReader(data)), hexSum(data))
			if a.code != 411 || a.reason != "length-required" {
				t.Fatalf("h2=%v: got %d %q", h2, a.code, a.reason)
			}
		}
	})
	if a := e.do(t, c, "GET", "/v1/b/backup/o/node-a/x", nil, ""); a.code != 404 {
		t.Fatalf("a refused put stored something: %d", a.code)
	}

	for _, l := range e.logs.lines(t) {
		if l["event"] == "request-refused" && (l["level"] != "warn" || l["reason"] == nil) {
			t.Errorf("refused line %v", l)
		}
		if l["event"] == "request" && l["reason"] != nil {
			t.Errorf("request line with a reason %v", l)
		}
	}
}

func dialTLS(t *testing.T, e *env, alpn string) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", e.addr, e.tlsConfig(t, nodeA, alpn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Refusals that need no body are answered while the body is still being sent (ADR 0003).
func TestRefusedBeforeBody(t *testing.T) {
	e := start(t)
	data := []byte("stored")
	if a := e.do(t, e.client(t, nodeA, false), "PUT", "/v1/b/backup/o/node-a/stored", bytes.NewReader(data), hexSum(data)); a.code != 201 {
		t.Fatalf("put: %d", a.code)
	}
	for _, tc := range []struct{ method, path, status string }{
		{"PUT", "/v1/b/backup/o/node-b/x", "403"},
		{"PUT", "/v1/b/backup/o/node-a/x", "413"},
		{"PUT", "/v1/b/nope/o/node-a/x", "404"},
		// A GET with a withheld body: net/http would wait for it before sending the object.
		{"GET", "/v1/b/backup/o/node-a/stored", "400"},
	} {
		c := dialTLS(t, e, "http/1.1")
		req := tc.method + " " + tc.path + " HTTP/1.1\r\nHost: store\r\nContent-Length: 100000\r\nAzukari-Sha256: " +
			strings.Repeat("0", 64) + "\r\n\r\n" + strings.Repeat("x", 100)
		if _, err := io.WriteString(c, req); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		if !strings.HasPrefix(resp.Status, tc.status) || !resp.Close {
			t.Fatalf("%s: %s close=%v", tc.path, resp.Status, resp.Close)
		}
	}
}

const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// h2frame encodes one HTTP/2 frame (RFC 9113 4.1).
func h2frame(typ, flags byte, stream uint32, payload []byte) []byte {
	b := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), typ, flags,
		byte(stream >> 24), byte(stream >> 16), byte(stream >> 8), byte(stream)}
	return append(b, payload...)
}

// hpack encodes name/value pairs as literals without indexing, new name (RFC 7541 6.2.2);
// every string under 128 bytes.
func hpack(kv ...string) []byte {
	var b []byte
	for i, s := range kv {
		if i%2 == 0 {
			b = append(b, 0)
		}
		b = append(append(b, byte(len(s))), s...)
	}
	return b
}

// dialH2 opens an HTTP/2 connection as node-a and sends the preface and settings.
func dialH2(t *testing.T, e *env, settings []byte) *tls.Conn {
	t.Helper()
	c := dialTLS(t, e, "h2")
	if p := c.ConnectionState().NegotiatedProtocol; p != "h2" {
		t.Fatalf("ALPN %q", p)
	}
	if _, err := c.Write(append([]byte(h2Preface), h2frame(0x4, 0, 0, settings)...)); err != nil {
		t.Fatal(err)
	}
	return c
}

// An HTTP/2 connection held after its preface, or in the middle of a HEADERS block while
// another stream is open, is closed like held HTTP/1 headers (ADR 0003).
func TestHeldPrefaceClosed(t *testing.T) {
	old := HeaderTimeout
	HeaderTimeout = 300 * time.Millisecond
	t.Cleanup(func() { HeaderTimeout = old })
	e := start(t)
	put := hpack(":method", "PUT", ":scheme", "https", ":authority", "store", ":path", "/v1/b/backup/o/node-a/held",
		"content-length", "10", "azukari-sha256", strings.Repeat("0", 64))
	for name, frames := range map[string][]byte{
		"preface": nil,
		// Stream 1 waits for its body (BodyTimeout, 60 s); stream 3's HEADERS lacks END_HEADERS.
		"headers beside an active stream": append(h2frame(0x1, 0x4, 1, put), h2frame(0x1, 0, 3, hpack(":method", "GET"))...),
	} {
		t.Run(name, func(t *testing.T) {
			c := dialH2(t, e, nil)
			if _, err := c.Write(frames); err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			c.SetReadDeadline(began.Add(5 * time.Second))
			_, err := io.Copy(io.Discard, c)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatal("held connection still open after 5 s")
			}
			if d := time.Since(began); d > 3*time.Second {
				t.Fatalf("closed after %v", d)
			}
			// Let stream 1's handler finish before another test changes the timeouts it reads.
			for frames != nil && !strings.Contains(e.logs.String(), "node-a/held") {
				time.Sleep(time.Millisecond)
			}
		})
	}
}

// A control response the client never reads is bounded by the write deadline (ADR 0003): with
// an HTTP/2 window of 0 the store cannot send it, and resets the stream once BodyTimeout passes.
func TestControlWriteDeadline(t *testing.T) {
	old := BodyTimeout
	BodyTimeout = 300 * time.Millisecond
	t.Cleanup(func() { BodyTimeout = old })
	e := start(t)
	body, err := proto.Marshal(&azukariv1.MissingRequest{Keys: []string{"node-a/x"}})
	if err != nil {
		t.Fatal(err)
	}
	c := dialH2(t, e, []byte{0, 0x4, 0, 0, 0, 0}) // SETTINGS_INITIAL_WINDOW_SIZE = 0
	req := h2frame(0x1, 0x4, 1, hpack(":method", "POST", ":scheme", "https", ":authority", "store",
		":path", "/v1/b/backup/missing", "content-length", strconv.Itoa(len(body))))
	if _, err := c.Write(append(req, h2frame(0x0, 0x1, 1, body)...)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(c)
	for {
		var h [9]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			t.Fatalf("no RST_STREAM on stream 1: %v", err)
		}
		if _, err := br.Discard(int(h[0])<<16 | int(h[1])<<8 | int(h[2])); err != nil {
			t.Fatal(err)
		}
		if h[3] == 0x3 && h[8] == 1 {
			return
		}
	}
}

func TestConnectionLimit(t *testing.T) {
	e := start(t)
	var held []net.Conn
	for range MaxConns {
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	extra, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection beyond the cap: %v, want EOF", err)
	}
	for _, c := range held {
		c.Close()
	}
	// The slots come back once the server sees the connections closed.
	c := e.client(t, nodeA, true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequest("GET", "https://"+e.addr+"/v1/b/backup/o/node-a/x", nil)
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != 404 {
				t.Fatalf("got %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no slot freed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStartLogs(t *testing.T) {
	e := start(t)
	seen := map[string]map[string]any{}
	for _, l := range e.logs.lines(t) {
		seen[l["event"].(string)+"/"+func() string { s, _ := l["bucket"].(string); return s }()] = l
	}
	for _, k := range []string{"bucket-created/backup", "bucket-created/full", "bucket-unavailable/down", "server-started/"} {
		if seen[k] == nil {
			t.Errorf("no %s line in %v", k, seen)
		}
	}
	if l := seen["bucket-unavailable/down"]; l != nil && (l["level"] != "error" || l["err"] == nil) {
		t.Errorf("bucket-unavailable %v", l)
	}
	if l := seen["server-started/"]; l != nil && (len(l["buckets"].([]any)) != 3 || l["listen"].([]any)[0] != e.addr) {
		t.Errorf("server-started %v", l)
	}
}

// post sends a control message and decodes the answer into resp when it is 200.
func (e *env) post(t *testing.T, c *http.Client, path string, req, resp proto.Message) answer {
	t.Helper()
	in, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	a := e.do(t, c, "POST", path, bytes.NewReader(in), "")
	if a.code == http.StatusOK {
		if ct := a.resp.Header.Get("Content-Type"); ct != "application/x-protobuf" {
			t.Fatalf("content type %q", ct)
		}
		if err := proto.Unmarshal(a.body, resp); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func TestMissingList(t *testing.T) {
	e := start(t)
	for _, h2 := range []bool{false, true} {
		e := start(t)
		c := e.client(t, nodeA, h2)
		for _, k := range []string{"node-a/chunks/b", "node-a/chunks/a", "node-a/x/1"} {
			data := []byte(k)
			if a := e.do(t, c, "PUT", "/v1/b/backup/o/"+k, bytes.NewReader(data), hexSum(data)); a.code != http.StatusCreated {
				t.Fatalf("put %s: %d %q", k, a.code, a.reason)
			}
		}

		var mr azukariv1.MissingResponse
		ask := []string{"node-a/chunks/a", "node-a/chunks/c", "node-a/chunks/b", "node-a/x/y"}
		if a := e.post(t, c, "/v1/b/backup/missing", &azukariv1.MissingRequest{Keys: ask}, &mr); a.code != 200 {
			t.Fatalf("missing: %d %q", a.code, a.reason)
		}
		if got := strings.Join(mr.Keys, ","); got != "node-a/chunks/c,node-a/x/y" {
			t.Fatalf("missing answered %q", got)
		}

		var lr azukariv1.ListResponse
		if a := e.post(t, c, "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-a", Limit: 2}, &lr); a.code != 200 {
			t.Fatalf("list: %d %q", a.code, a.reason)
		}
		if len(lr.Objects) != 2 || lr.Objects[0].Key != "node-a/chunks/a" || lr.Objects[1].Key != "node-a/chunks/b" || lr.Next != "node-a/chunks/b" {
			t.Fatalf("first page %v", &lr)
		}
		if o := lr.Objects[0]; o.Size != uint64(len(o.Key)) || hex.EncodeToString(o.Sha256) != hexSum([]byte(o.Key)) {
			t.Fatalf("object %v", o)
		}
		if a := e.post(t, c, "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-a/", StartAfter: lr.Next}, &lr); a.code != 200 {
			t.Fatalf("list page 2: %d %q", a.code, a.reason)
		}
		if len(lr.Objects) != 1 || lr.Objects[0].Key != "node-a/x/1" || lr.Next != "" {
			t.Fatalf("last page %v", &lr)
		}
	}

	c := e.client(t, nodeA, true)
	data := []byte("x")
	if a := e.do(t, c, "PUT", "/v1/b/backup/o/node-a/k", bytes.NewReader(data), hexSum(data)); a.code != http.StatusCreated {
		t.Fatalf("put: %d", a.code)
	}
	var mr azukariv1.MissingResponse
	e.post(t, c, "/v1/b/backup/missing", &azukariv1.MissingRequest{Keys: []string{"node-a/k", "node-a/l"}}, &mr)
	var line map[string]any
	for _, l := range e.logs.lines(t) {
		if l["event"] == "request" && l["call"] == "missing" {
			line = l
		}
	}
	if line == nil || line["count"] != 2.0 || line["code"] != 200.0 || line["bucket"] != "backup" || line["peer_id"] != nodeA {
		t.Fatalf("missing request line %v", line)
	}
	if _, ok := line["key"]; ok {
		t.Errorf("missing line carries a key: %v", line)
	}
}

func TestControlRefusals(t *testing.T) {
	e := start(t)
	c := e.client(t, nodeA, true)
	many := make([]string, 10001)
	for i := range many {
		many[i] = "node-a/k"
	}
	cases := []struct {
		name, path string
		req        proto.Message
		code       int
		reason     string
	}{
		{"too many keys", "/v1/b/backup/missing", &azukariv1.MissingRequest{Keys: many}, 413, "too-many-keys"},
		{"key invalid", "/v1/b/backup/missing", &azukariv1.MissingRequest{Keys: []string{"node-a/k", "node-a/../x"}}, 400, "key-invalid"},
		{"foreign key", "/v1/b/backup/missing", &azukariv1.MissingRequest{Keys: []string{"node-a/k", "node-b/k"}}, 403, "namespace-forbidden"},
		{"foreign key on a down bucket", "/v1/b/down/missing", &azukariv1.MissingRequest{Keys: []string{"node-b/k"}}, 403, "namespace-forbidden"},
		{"missing on a down bucket", "/v1/b/down/missing", &azukariv1.MissingRequest{Keys: []string{"node-a/k"}}, 503, "bucket-unavailable"},
		{"missing unknown bucket", "/v1/b/nope/missing", &azukariv1.MissingRequest{}, 404, "bucket-unknown"},
		{"limit too high", "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-a/", Limit: 10001}, 400, "request-invalid"},
		{"empty prefix", "/v1/b/backup/list", &azukariv1.ListRequest{}, 403, "namespace-forbidden"},
		{"foreign prefix", "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-b/"}, 403, "namespace-forbidden"},
		{"sibling prefix", "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-ab"}, 403, "namespace-forbidden"},
		{"partial node prefix", "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-"}, 403, "namespace-forbidden"},
		{"list on a down bucket", "/v1/b/down/list", &azukariv1.ListRequest{Prefix: "node-a/"}, 503, "bucket-unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if a := e.post(t, c, tc.path, tc.req, nil); a.code != tc.code || a.reason != tc.reason {
				t.Fatalf("got %d %q, want %d %q", a.code, a.reason, tc.code, tc.reason)
			}
		})
	}

	raw := []struct {
		name, method, path string
		body               []byte
		code               int
		reason             string
	}{
		{"malformed missing", "POST", "/v1/b/backup/missing", []byte{0xff, 0xff}, 400, "request-invalid"},
		{"malformed list", "POST", "/v1/b/backup/list", []byte{0xff, 0xff}, 400, "request-invalid"},
		{"missing over 4 MiB", "POST", "/v1/b/backup/missing", make([]byte, 4<<20+1), 413, "too-many-keys"},
		{"list over 4 MiB", "POST", "/v1/b/backup/list", make([]byte, 4<<20+1), 400, "request-invalid"},
		{"get on missing", "GET", "/v1/b/backup/missing", nil, 400, "request-invalid"},
		{"unknown control call", "POST", "/v1/b/backup/delete", nil, 400, "request-invalid"},
	}
	for _, tc := range raw {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != nil {
				body = bytes.NewReader(tc.body)
			}
			if a := e.do(t, c, tc.method, tc.path, body, ""); a.code != tc.code || a.reason != tc.reason {
				t.Fatalf("got %d %q, want %d %q", a.code, a.reason, tc.code, tc.reason)
			}
		})
	}
	t.Run("over 4 MiB without length", func(t *testing.T) {
		for _, h2 := range []bool{false, true} {
			body := io.MultiReader(bytes.NewReader(make([]byte, 4<<20+1)))
			if a := e.do(t, e.client(t, nodeA, h2), "POST", "/v1/b/backup/list", body, ""); a.code != 400 || a.reason != "request-invalid" {
				t.Fatalf("h2=%v: got %d %q", h2, a.code, a.reason)
			}
		}
	})
	t.Run("unknown ID", func(t *testing.T) {
		if a := e.post(t, e.client(t, stranger, true), "/v1/b/backup/list", &azukariv1.ListRequest{Prefix: "node-z/"}, nil); a.code != 403 || a.reason != "forbidden" {
			t.Fatalf("got %d %q", a.code, a.reason)
		}
	})

	for _, l := range e.logs.lines(t) {
		if l["event"] == "request-refused" && (l["call"] == "missing" || l["call"] == "list") {
			if _, ok := l["count"]; !ok {
				t.Errorf("refused control line lacks count: %v", l)
			}
		}
	}
}

func TestRefsAndSnapshot(t *testing.T) {
	e := start(t)
	c := e.client(t, nodeA, true)
	put := func(k string, data []byte) answer {
		return e.do(t, c, "PUT", "/v1/b/backup/o/node-a/"+k, bytes.NewReader(data), hexSum(data))
	}
	id := sha256.Sum256([]byte("chunk"))
	h := hex.EncodeToString(id[:])
	other := [32]byte{}
	for name, body := range map[string][]byte{
		"unsorted":  append(id[:], other[:]...),
		"duplicate": append(id[:], id[:]...),
	} {
		if a := put("refs/data/s", body); a.code != http.StatusBadRequest || a.reason != "refs-invalid" {
			t.Errorf("%s: %d %q", name, a.code, a.reason)
		}
	}
	if a := put("refs/data/s", id[:]); a.code != http.StatusConflict || a.reason != "refs-incomplete" {
		t.Fatalf("incomplete: %d %q", a.code, a.reason)
	}
	if a := put("chunks/"+h[:2]+"/"+h, []byte("chunk")); a.code != http.StatusCreated {
		t.Fatalf("chunk: %d %q", a.code, a.reason)
	}
	if a := put("refs/data/s", id[:]); a.code != http.StatusCreated {
		t.Fatalf("complete: %d %q", a.code, a.reason)
	}
	for range 2 { // the retry (200) is not a second snapshot
		if a := put("snapshots/data/s", []byte("root")); a.code >= 300 {
			t.Fatalf("snapshot: %d %q", a.code, a.reason)
		}
	}
	var got []map[string]any
	for _, l := range e.logs.lines(t) {
		if l["event"] == "snapshot-received" {
			got = append(got, l)
		}
	}
	if len(got) != 1 || got[0]["bucket"] != "backup" || got[0]["namespace"] != "node-a" || got[0]["snapshot"] != "data/s" || got[0]["level"] != "info" {
		t.Fatalf("snapshot-received lines %v", got)
	}
	if s := e.snapshots.String(); s != "backup node-a\n" {
		t.Fatalf("OnSnapshot calls %q", s)
	}
}
