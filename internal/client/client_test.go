package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/server"
	"github.com/scorsi/azukari/internal/svid"
	"github.com/scorsi/azukari/internal/testca"
)

const (
	serverID = "spiffe://example.org/store-host/azukari-server"
	nodeA    = "spiffe://example.org/node-a/azukari-backup"
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

func (s *syncBuffer) count(event string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.b.String(), `"event":"`+event+`"`)
}

type env struct {
	ca   *testca.CA
	addr string
	dir  string // the buckets' mount points: <dir>/<bucket>/b is the bucket
	logs *syncBuffer
}

// start serves "backup" and "down" (unavailable: its mount check fails) on loopback with a
// real internal/server, and shrinks the client's backoff.
func start(t *testing.T) *env {
	t.Helper()
	b := Backoff
	Backoff = time.Millisecond
	t.Cleanup(func() { Backoff = b })
	ca := testca.New(t)
	dir := t.TempDir()
	cfg := &config.Server{}
	for _, name := range []string{"backup", "down"} {
		mount := filepath.Join(dir, name)
		if err := os.Mkdir(mount, 0o700); err != nil {
			t.Fatal(err)
		}
		cfg.Buckets = append(cfg.Buckets, config.Bucket{Name: name, Type: "backup", Path: filepath.Join(mount, "b"),
			MountPoint: mount, MaxObjectBytes: 1024, DiskReserve: "0B", Nodes: []string{nodeA}})
	}
	src, err := svid.Load(ca.SVID(t, serverID))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	isMount := func(p string) (bool, error) { return p != filepath.Join(dir, "down"), nil }
	s, err := server.New(cfg, src, logging.New(logs, slog.LevelInfo), isMount)
	if err != nil {
		t.Fatal(err)
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
	for logs.count("server-started") == 0 {
		time.Sleep(time.Millisecond)
	}
	return &env{ca: ca, addr: ln.Addr().String(), dir: dir, logs: logs}
}

// client talks to bucket through addr (the store, or a proxy in front of it).
func (e *env) client(t *testing.T, addr, bucket string) *Client {
	t.Helper()
	files := e.ca.SVID(t, nodeA)
	src, err := svid.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Backup{Server: "https://" + addr, Bucket: bucket,
		TLS: config.BackupTLS{TLS: files, ServerID: serverID}}
	c := New(cfg, src)
	t.Cleanup(c.Close)
	return c
}

func wantReason(t *testing.T, err error, status int, reason string) {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) || se.Status != status || se.Reason != reason {
		t.Fatalf("err = %v, want %d %s", err, status, reason)
	}
}

func TestRoundTrip(t *testing.T) {
	e := start(t)
	c := e.client(t, e.addr, "backup")
	ctx := context.Background()
	body := []byte("hello")
	for i, want := range []bool{true, false} { // the second put is a retry of the same bytes
		created, err := c.Put(ctx, "node-a/chunks/x", body)
		if err != nil || created != want {
			t.Fatalf("put %d: created %v, err %v", i, created, err)
		}
	}
	if _, err := c.Put(ctx, "node-a/empty", nil); err != nil {
		t.Fatalf("empty put: %v", err)
	}
	got, err := c.Get(ctx, "node-a/chunks/x")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get: %q %v", got, err)
	}
	if got, err := c.Get(ctx, "node-a/empty"); err != nil || len(got) != 0 {
		t.Fatalf("empty get: %q %v", got, err)
	}
	if got := e.logs.count("request-refused"); got != 0 {
		t.Fatalf("%d refused requests", got)
	}
	r, err := c.http.Get(c.base + "/o/node-a/empty")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.ProtoMajor != 2 {
		t.Fatalf("spoke %s, want HTTP/2", r.Proto)
	}
}

func TestReasons(t *testing.T) {
	e := start(t)
	c := e.client(t, e.addr, "backup")
	ctx := context.Background()
	if _, err := c.Put(ctx, "node-a/x", []byte("one")); err != nil {
		t.Fatal(err)
	}
	_, err := c.Put(ctx, "node-a/x", []byte("two"))
	wantReason(t, err, 409, "object-exists")
	_, err = c.Put(ctx, "node-a/refs/s", make([]byte, 32))
	wantReason(t, err, 409, "refs-incomplete")
	_, err = c.Get(ctx, "node-a/nothing")
	wantReason(t, err, 404, "object-unknown")
	_, err = c.Missing(ctx, []string{"node-b/x"})
	wantReason(t, err, 403, "namespace-forbidden")
	_, err = e.client(t, e.addr, "nope").Get(ctx, "node-a/x")
	wantReason(t, err, 404, "bucket-unknown")
	// 4xx are never retried: one request logged per call above (404 is not a refusal).
	if got := e.logs.count("request") + e.logs.count("request-refused"); got != 6 {
		t.Fatalf("%d requests, want 6", got)
	}
}

func TestRetries(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	_, err := e.client(t, e.addr, "down").Put(ctx, "node-a/x", []byte("x"))
	wantReason(t, err, 503, "bucket-unavailable")
	if got := e.logs.count("request-refused"); got != Attempts {
		t.Fatalf("503: %d tries, want %d", got, Attempts)
	}

	// An object whose header is corrupt on disk answers 500 internal (ADR 0013).
	c := e.client(t, e.addr, "backup")
	if _, err := c.Put(ctx, "node-a/bad", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "backup", "b", "objects", "node-a", "bad"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = c.Get(ctx, "node-a/bad")
	wantReason(t, err, 500, "internal")
	if got := e.logs.count("request-refused"); got != 2*Attempts {
		t.Fatalf("500: %d tries, want %d", got-Attempts, Attempts)
	}
}

// proxy forwards to addr, closing the first drop connections at once as a store beyond its
// connection limit does (ADR 0012).
func proxy(t *testing.T, addr string, drop int32) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			if n.Add(1) <= drop {
				in.Close()
				continue
			}
			out, err := net.Dial("tcp", addr)
			if err != nil {
				in.Close()
				continue
			}
			go func() { io.Copy(out, in); out.Close() }()
			go func() { io.Copy(in, out); in.Close() }()
		}
	}()
	return ln.Addr().String(), &n
}

func TestRetryDroppedConnection(t *testing.T) {
	e := start(t)
	ctx := context.Background()
	addr, n := proxy(t, e.addr, int32(Attempts-1))
	created, err := e.client(t, addr, "backup").Put(ctx, "node-a/x", []byte("x"))
	if err != nil || !created {
		t.Fatalf("created %v, err %v", created, err)
	}
	if got := n.Load(); got != int32(Attempts) {
		t.Fatalf("%d connections, want %d", got, Attempts)
	}

	// Beyond Attempts the transport error comes back, not an *Error.
	addr, _ = proxy(t, e.addr, int32(Attempts))
	_, err = e.client(t, addr, "backup").Get(ctx, "node-a/x")
	var se *Error
	if err == nil || errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
}

func TestPaging(t *testing.T) {
	e := start(t)
	c := e.client(t, e.addr, "backup")
	ctx := context.Background()
	defer func(l uint32, m int) { ListLimit, MissingBatch = l, m }(ListLimit, MissingBatch)
	ListLimit, MissingBatch = 2, 2

	var keys []string
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		keys = append(keys, "node-a/s/"+k)
	}
	for _, k := range keys[:4] { // e is never put
		if _, err := c.Put(ctx, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	missing, err := c.Missing(ctx, append([]string{"node-a/s/z"}, keys...))
	if err != nil || !slices.Equal(missing, []string{"node-a/s/z", "node-a/s/e"}) {
		t.Fatalf("missing %v, err %v", missing, err)
	}
	var listed []string
	for o, err := range c.List(ctx, "node-a/s") {
		if err != nil {
			t.Fatal(err)
		}
		if o.Size != uint64(len(o.Key)) { // each object holds its key
			t.Fatalf("%s: size %d", o.Key, o.Size)
		}
		listed = append(listed, o.Key)
	}
	if !slices.Equal(listed, keys[:4]) {
		t.Fatalf("listed %v", listed)
	}
	if got := e.logs.count("request"); got != 4+3+2 { // puts, missing batches, list pages
		t.Fatalf("%d requests, want 9", got)
	}
}
