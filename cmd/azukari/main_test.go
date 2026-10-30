package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/server"
)

// The command against a store's administration socket (no TLS listener needed).
func TestCommand(t *testing.T) {
	dir, err := os.MkdirTemp("", "azcli") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "b")
	cfg := &config.Server{Buckets: []config.Bucket{{Name: "backup", Type: "backup", Path: path, MountPoint: dir,
		MaxObjectBytes: 1024, DiskReserve: "0B", Retention: config.Retention{Keep: "10d", Grace: "2d", RunAt: "03:30"}}}}
	srv, err := server.New(cfg, nil, logging.New(&bytes.Buffer{}, slog.LevelInfo), func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	b := bucket.New(path, 1024, 0)
	for _, k := range []string{"node-a/snapshots/data/s1", "node-a/snapshots/data/s2"} {
		if _, err := b.Put(strings.Replace(k, "snapshots", "refs", 1), 0, sha256.Sum256(nil), strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Put(k, 4, sha256.Sum256([]byte("root")), strings.NewReader("root")); err != nil {
			t.Fatal(err)
		}
	}
	sock := filepath.Join(dir, server.AdminSocket)
	ln, err := server.ListenAdmin(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ServeAdmin(ctx, ln)

	cmd := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(append([]string{"--socket", sock}, args...), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := cmd("snapshots", "backup", "node-a", "data"); code != 0 || !strings.HasPrefix(out, "s1 ") ||
		len(strings.Split(strings.TrimSpace(out), "\n")) != 2 || !strings.HasSuffix(out, " 4\n") {
		t.Fatalf("snapshots = %d %q", code, out)
	}
	if code, out, _ := cmd("delete-snapshot", "backup", "node-a", "data", "s1"); code != 0 || out != "deleted node-a data s1\n" {
		t.Fatalf("delete = %d %q", code, out)
	}
	if code, out, _ := cmd("retention", "backup", "--dry-run"); code != 0 || out != "snapshots=0 refs=0 chunks=0 bytes=0 dry_run=true\n" {
		t.Fatalf("retention = %d %q", code, out)
	}
	if code, _, errOut := cmd("delete-snapshot", "backup", "node-a", "data", "s1"); code != 1 || !strings.Contains(errOut, `"reason":"snapshot-unknown"`) {
		t.Fatalf("delete again = %d %q", code, errOut)
	}
	for _, bad := range [][]string{{}, {"snapshots", "backup"}, {"snapshots", "backup", "n"}, {"delete-snapshot", "backup", "n", "s1"},
		{"retention", "backup", "--force"}, {"snapshots", "backup", "n", "data", "--dry-run"}} {
		if code, _, _ := cmd(bad...); code != 2 {
			t.Errorf("%q = %d, want 2", bad, code)
		}
	}
	if code := run([]string{"--socket=" + filepath.Join(dir, "none"), "retention", "backup"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 1 {
		t.Fatalf("no socket = %d", code)
	}
}
