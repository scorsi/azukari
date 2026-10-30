package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	adminv1 "github.com/scorsi/azukari/gen/azukari/admin/v1"
	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
)

// startAdmin serves e's store on a fresh administration socket and returns its path. Short
// directory: a socket path is limited to about 100 bytes.
func startAdmin(t *testing.T, e *env) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "azadm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, AdminSocket)
	if err := os.WriteFile(path, nil, 0o600); err != nil { // a stale file from a previous run
		t.Fatal(err)
	}
	ln, err := ListenAdmin(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- e.srv.ServeAdmin(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return path
}

// adminPost posts req (raw bytes when it is a []byte) and decodes a 200 into resp; it returns
// the status and the Error's reason.
func adminPost(t *testing.T, sock, method, path string, req any, resp proto.Message) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	defer c.CloseIdleConnections()
	var body []byte
	switch r := req.(type) {
	case []byte:
		body = r
	case proto.Message:
		var err error
		if body, err = proto.Marshal(r); err != nil {
			t.Fatal(err)
		}
	}
	hr, err := http.NewRequest(method, "http://azukari"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	out, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode >= 400 {
		var e azukariv1.Error
		if err := proto.Unmarshal(out, &e); err != nil {
			t.Fatalf("%s %s: %d with no Error: %v", method, path, r.StatusCode, err)
		}
		return r.StatusCode, e.Reason
	}
	if err := proto.Unmarshal(out, resp); err != nil {
		t.Fatal(err)
	}
	return r.StatusCode, ""
}

func TestAdminSocket(t *testing.T) {
	e := start(t)
	sock := startAdmin(t, e)
	if fi, err := os.Lstat(sock); err != nil || fi.Mode().Type() != os.ModeSocket || fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket %v, %v; want a socket, mode 0660", fi.Mode(), err)
	}

	store(t, e, "node-a/snapshots/s1", "root s1")
	store(t, e, "node-a/refs/s1", "")
	time.Sleep(time.Millisecond) // distinct receive times
	store(t, e, "node-a/snapshots/s2", "root s2")
	var sr adminv1.SnapshotsResponse
	if code, reason := adminPost(t, sock, "POST", "/v1/admin/b/backup/snapshots", &adminv1.SnapshotsRequest{Namespace: "node-a"}, &sr); code != 200 {
		t.Fatalf("snapshots = %d %s", code, reason)
	}
	if len(sr.Snapshots) != 2 || sr.Snapshots[0].Name != "s1" || sr.Snapshots[1].Name != "s2" ||
		sr.Snapshots[0].Size != uint64(len("root s1")) || sr.Snapshots[0].ReceiveTime > sr.Snapshots[1].ReceiveTime ||
		time.Since(time.Unix(0, sr.Snapshots[1].ReceiveTime)) > time.Minute {
		t.Fatalf("snapshots = %v", sr.Snapshots)
	}
	// An unknown namespace lists no snapshot (ADR 0024).
	sr.Reset()
	if code, _ := adminPost(t, sock, "POST", "/v1/admin/b/backup/snapshots", &adminv1.SnapshotsRequest{Namespace: "node-x"}, &sr); code != 200 || len(sr.Snapshots) != 0 {
		t.Fatalf("unknown namespace = %d %v", code, sr.Snapshots)
	}

	del := &adminv1.DeleteSnapshotRequest{Namespace: "node-a", Name: "s1"}
	if code, reason := adminPost(t, sock, "POST", "/v1/admin/b/backup/delete-snapshot", del, &adminv1.DeleteSnapshotResponse{}); code != 200 {
		t.Fatalf("delete = %d %s", code, reason)
	}
	if l, err := e.srv.Snapshots("backup", "node-a"); err != nil || len(l) != 1 || l[0].Name != "s2" {
		t.Fatalf("after delete: %v, %v", l, err)
	}

	var rr adminv1.RetentionResponse
	if code, reason := adminPost(t, sock, "POST", "/v1/admin/b/backup/retention", &adminv1.RetentionRequest{DryRun: true}, &rr); code != 200 || !rr.DryRun {
		t.Fatalf("retention = %d %s %v", code, reason, &rr)
	}

	refusals := []struct {
		method, path string
		req          any
		code         int
		reason       string
	}{
		{"POST", "/v1/admin/b/backup/delete-snapshot", del, 404, "snapshot-unknown"},
		{"POST", "/v1/admin/b/nope/snapshots", &adminv1.SnapshotsRequest{Namespace: "node-a"}, 404, "bucket-unknown"},
		{"POST", "/v1/admin/b/nope/retention", &adminv1.RetentionRequest{}, 404, "bucket-unknown"},
		{"POST", "/v1/admin/b/nope/delete-snapshot", del, 404, "bucket-unknown"},
		{"POST", "/v1/admin/b/down/snapshots", &adminv1.SnapshotsRequest{Namespace: "node-a"}, 503, "bucket-unavailable"},
		{"POST", "/v1/admin/b/down/retention", &adminv1.RetentionRequest{}, 503, "bucket-unavailable"},
		{"POST", "/v1/admin/b/down/delete-snapshot", del, 503, "bucket-unavailable"},
		{"POST", "/v1/admin/b/backup/snapshots", &adminv1.SnapshotsRequest{Namespace: "../x"}, 400, "key-invalid"},
		{"POST", "/v1/admin/b/backup/delete-snapshot", &adminv1.DeleteSnapshotRequest{Namespace: "node-a", Name: ".."}, 400, "key-invalid"},
		{"POST", "/v1/admin/b/backup/snapshots", []byte{0xff}, 400, "request-invalid"},
		{"GET", "/v1/admin/b/backup/snapshots", nil, 400, "request-invalid"},
		{"POST", "/v1/admin/b/backup/prune", nil, 400, "request-invalid"},
		{"POST", "/v1/b/backup/list", nil, 400, "request-invalid"},
		{"POST", "/v1/admin/b/backup/snapshots", make([]byte, maxControlBytes+1), 400, "request-invalid"},
	}
	for _, c := range refusals {
		if code, reason := adminPost(t, sock, c.method, c.path, c.req, nil); code != c.code || reason != c.reason {
			t.Errorf("%s %s = %d %s, want %d %s", c.method, c.path, code, reason, c.code, c.reason)
		}
	}

	// One admin-call per call, with the caller's uid (SO_PEERCRED) and the arguments.
	var calls []map[string]any
	for _, l := range e.logs.lines(t) {
		if l["event"] == "admin-call" {
			calls = append(calls, l)
		}
	}
	if len(calls) != 4+len(refusals) {
		t.Fatalf("%d admin-call lines, want %d:\n%s", len(calls), 4+len(refusals), e.logs)
	}
	want := fmt.Sprint(os.Getuid())
	for _, c := range calls {
		if fmt.Sprint(c["uid"]) != want {
			t.Fatalf("uid %v, want %s: %v", c["uid"], want, c)
		}
	}
	if d := calls[2]; d["call"] != "delete-snapshot" || d["bucket"] != "backup" || d["namespace"] != "node-a" ||
		d["snapshot"] != "s1" || d["code"] != float64(200) {
		t.Fatalf("delete line %v", d)
	}
	if r := calls[4]; r["code"] != float64(404) || r["reason"] != "snapshot-unknown" {
		t.Fatalf("refusal line %v", r)
	}
}

// A retention call waits for a run of that bucket already in progress, past the body
// deadline: no response deadline (ADR 0024).
func TestAdminRetentionWaits(t *testing.T) {
	e := start(t)
	sock := startAdmin(t, e)
	old := BodyTimeout
	BodyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { BodyTimeout = old })
	id := sha256.Sum256([]byte("c1"))
	x := fmt.Sprintf("%x", id)
	store(t, e, "node-a/chunks/"+x[:2]+"/"+x, "c1")
	for _, s := range []string{"s1", "s2"} {
		store(t, e, "node-a/refs/"+s, string(id[:]))
		store(t, e, "node-a/snapshots/"+s, "root "+s)
	}
	later := time.Now().Add(30 * day)
	e.srv.now = func() time.Time { return later }
	e.srv.buckets["backup"].ret.Enabled = true

	sb := e.srv.buckets["backup"]
	sb.run.Lock()
	type result struct {
		code int
		rr   adminv1.RetentionResponse
	}
	res := make(chan *result)
	go func() {
		r := &result{}
		r.code, _ = adminPost(t, sock, "POST", "/v1/admin/b/backup/retention", &adminv1.RetentionRequest{}, &r.rr)
		res <- r
	}()
	time.Sleep(4 * BodyTimeout)
	select {
	case <-res:
		t.Fatal("answered while a run was in progress")
	default:
	}
	sb.run.Unlock()
	r := <-res
	if r.code != 200 || r.rr.DryRun || r.rr.Snapshots != 1 || r.rr.Refs != 1 || r.rr.Bytes != uint64(len("root s1")+32) {
		t.Fatalf("retention = %d %v", r.code, &r.rr)
	}
	if !strings.Contains(e.logs.String(), `"dry_run":false`) {
		t.Fatalf("no real prune-completed:\n%s", e.logs)
	}
}
