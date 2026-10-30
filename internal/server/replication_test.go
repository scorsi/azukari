package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/config"
)

// These tests run the real rsync (the package's check inputs) against dir replicas.

// localReplica is a dir replica in a fresh directory, mounted as far as the env's isMount says.
func localReplica(t *testing.T) config.Replica {
	mount := t.TempDir()
	dest := filepath.Join(mount, "copy")
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	return config.Replica{Name: "local", Kind: "dir", Destination: dest, MountPoint: mount, Every: "1h"}
}

// snapshot stores a chunk, the refs naming it and the root, in ADR 0006's order; it returns
// their keys.
func snapshot(t *testing.T, e *env, name, chunk string) (c, refs, root string) {
	id := sha256.Sum256([]byte(chunk))
	x := fmt.Sprintf("%x", id)
	c, refs, root = "node-a/chunks/"+x[:2]+"/"+x, "node-a/refs/data/"+name, "node-a/snapshots/data/"+name
	store(t, e, c, chunk)
	store(t, e, refs, string(id[:]))
	store(t, e, root, "root "+name)
	return c, refs, root
}

// tree reads every file under dir but tmp/, by path relative to dir.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "tmp" {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			files[rel] = string(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func has(t *testing.T, dir, key string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, "objects", key))
	return err == nil
}

// events returns the log lines of event ev.
func events(t *testing.T, e *env, ev string) []map[string]any {
	var out []map[string]any
	for _, l := range e.logs.lines(t) {
		if l["event"] == ev {
			out = append(out, l)
		}
	}
	return out
}

func sameTree(t *testing.T, a, b string) {
	t.Helper()
	ta, tb := tree(t, a), tree(t, b)
	if len(ta) != len(tb) {
		t.Fatalf("%d files in the bucket, %d in the copy:\n%v\n%v", len(ta), len(tb), ta, tb)
	}
	for k, v := range ta {
		if tb[k] != v {
			t.Fatalf("%s differs: %q, copy %q", k, v, tb[k])
		}
	}
}

func TestReplicate(t *testing.T) {
	e := start(t)
	r := localReplica(t)
	ctx := context.Background()
	bkt, cp := e.cfg.Buckets[0].Path, filepath.Join(r.Destination, "backup")
	c1, refs1, root1 := snapshot(t, e, "s1", "c1")

	// Roots go after their objects; a root stored mid-run waits for the next run; retention
	// waits for the run.
	var root2 string
	pruned := make(chan struct{})
	e.srv.onPhase = func(phase string) {
		switch phase {
		case "objects":
			go func() { e.srv.RunRetention("backup", true); close(pruned) }()
		case "roots":
			if !has(t, cp, c1) || !has(t, cp, refs1) || has(t, cp, root1) {
				t.Errorf("before roots: chunk %v refs %v root %v", has(t, cp, c1), has(t, cp, refs1), has(t, cp, root1))
			}
			_, _, root2 = snapshot(t, e, "s2", "c2")
		case "delete-chunks":
			select {
			case <-pruned:
				t.Error("retention ran during the replication")
			default:
			}
		}
	}
	e.srv.Replicate(ctx, "backup", r)
	<-pruned
	if !has(t, cp, root1) || has(t, cp, root2) {
		t.Fatalf("after run 1: root s1 %v, root s2 %v\n%s", has(t, cp, root1), has(t, cp, root2), e.logs)
	}
	done := events(t, e, "replication-completed")
	if len(done) != 1 || done[0]["files"] != 4.0 || done[0]["bytes"] != float64(len("1\n")+len("c1")+32+len("root s1"))+3*64 ||
		done[0]["deletions"] != 0.0 || done[0]["replica"] != "local" {
		t.Fatalf("replication-completed: %v\n%s", done, e.logs)
	}

	e.srv.onPhase = nil
	e.srv.Replicate(ctx, "backup", r)
	sameTree(t, bkt, cp)

	// Pruning: s1 goes (root, refs, then its chunk); the copy follows in that order.
	later := time.Now().Add(30 * day)
	e.srv.now = func() time.Time { return later }
	e.srv.buckets["backup"].ret.Enabled = true
	if p, err := e.srv.RunRetention("backup", false); err != nil || p.Snapshots != 1 || p.Chunks != 1 {
		t.Fatalf("prune: %+v, %v", p, err)
	}
	e.srv.onPhase = func(phase string) {
		switch phase {
		case "delete-refs":
			if has(t, cp, root1) || !has(t, cp, refs1) || !has(t, cp, c1) {
				t.Errorf("before refs: root %v refs %v chunk %v", has(t, cp, root1), has(t, cp, refs1), has(t, cp, c1))
			}
		case "delete-chunks":
			if has(t, cp, refs1) || !has(t, cp, c1) {
				t.Errorf("before chunks: refs %v chunk %v", has(t, cp, refs1), has(t, cp, c1))
			}
		}
	}
	e.srv.Replicate(ctx, "backup", r)
	sameTree(t, bkt, cp)
	if done := events(t, e, "replication-completed"); len(done) != 3 || done[2]["deletions"] != 3.0 {
		t.Fatalf("replication-completed: %v", done)
	}
	m := scrape(t, e.srv)
	for _, want := range []string{
		fmt.Sprintf("azukari_replication_last_success_timestamp_seconds{bucket=\"backup\",replica=\"local\"} %d\n", later.Unix()),
		"azukari_replication_runs_total{bucket=\"backup\",replica=\"local\",result=\"completed\"} 3\n",
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("no %q in\n%s", want, m)
		}
	}
}

// A chunk pruned while the replica was unreachable and stored again (same size, new header)
// is copied again (ADR 0026).
func TestReplicateRecreated(t *testing.T) {
	e := start(t)
	r := localReplica(t)
	ctx := context.Background()
	bkt, cp := e.cfg.Buckets[0].Path, filepath.Join(r.Destination, "backup")
	c, _, _ := snapshot(t, e, "s1", "c1")
	snapshot(t, e, "s2", "c2") // retention keeps the newest snapshot
	// Received long before its recreation, as retention's grace makes it in practice: rsync
	// compares times to the second.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(bkt, "objects", c), old, old); err != nil {
		t.Fatal(err)
	}
	e.srv.Replicate(ctx, "backup", r)
	sameTree(t, bkt, cp)
	before := tree(t, cp)["objects/"+c]

	// No run while s1 and its chunk are pruned and the chunk stored again.
	e.srv.now = func() time.Time { return time.Now().Add(30 * day) }
	e.srv.buckets["backup"].ret.Enabled = true
	if p, err := e.srv.RunRetention("backup", false); err != nil || p.Chunks != 1 {
		t.Fatalf("prune: %+v, %v", p, err)
	}
	store(t, e, c, "c1")
	if after := tree(t, bkt)["objects/"+c]; len(after) != len(before) || after == before {
		t.Fatalf("recreated chunk: %d bytes, same as before %v", len(after), after == before)
	}

	e.srv.Replicate(ctx, "backup", r)
	sameTree(t, bkt, cp)
	if done := events(t, e, "replication-completed"); len(done) != 2 || done[1]["files"] != 1.0 {
		t.Fatalf("replication-completed: %v\n%s", done, e.logs)
	}
}

// Guards skip a run and touch nothing; a FORMAT mismatch or an rsync failure fails it.
func TestReplicateRefused(t *testing.T) {
	e := start(t)
	r := localReplica(t)
	ctx := context.Background()
	cp := filepath.Join(r.Destination, "backup")
	_, _, root := snapshot(t, e, "s1", "c1")
	extra := filepath.Join(cp, "objects", "node-a", "chunks", "zz", "extra")
	if err := os.MkdirAll(filepath.Dir(extra), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{extra, filepath.Join(cp, "FORMAT")} {
		if err := os.WriteFile(f, []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	e.srv.onPhase = func(string) { calls++ }

	unmounted := r
	unmounted.MountPoint = e.cfg.Buckets[1].MountPoint // the env's only path that is not a mount
	sb := e.srv.buckets["backup"]
	for _, tc := range []struct {
		bucket string
		r      config.Replica
		lock   bool
		reason string
	}{
		{"down", r, false, "bucket unavailable"},
		{"backup", unmounted, false, "is not a mount point"},
		{"backup", r, true, "retention running"},
	} {
		if tc.lock {
			sb.run.Lock()
		}
		e.srv.Replicate(ctx, tc.bucket, tc.r)
		if tc.lock {
			sb.run.Unlock()
		}
		l := events(t, e, "replication-skipped")
		if last := l[len(l)-1]; !strings.Contains(last["reason"].(string), tc.reason) || last["bucket"] != tc.bucket {
			t.Fatalf("skip: %v, want %q", last, tc.reason)
		}
	}
	if calls != 0 {
		t.Fatalf("%d rsync calls on skipped runs", calls)
	}

	if err := os.WriteFile(filepath.Join(cp, "FORMAT"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.Replicate(ctx, "backup", r)
	missing := r
	missing.Destination = filepath.Join(r.MountPoint, "absent")
	e.srv.Replicate(ctx, "backup", missing)

	failed := events(t, e, "replication-failed")
	if len(failed) != 2 || !strings.Contains(failed[0]["err"].(string), `destination FORMAT holds "1\n"`) || failed[0]["exit_code"] != nil {
		t.Fatalf("FORMAT mismatch: %v", failed)
	}
	if failed[1]["exit_code"] != 23.0 || !strings.Contains(failed[1]["err"].(string), "rsync check") {
		t.Fatalf("rsync failure: %v", failed[1])
	}
	if _, err := os.Stat(extra); err != nil || has(t, cp, root) || len(tree(t, cp)) != 2 {
		t.Fatalf("the copy changed: %v", tree(t, cp))
	}
	m := scrape(t, e.srv)
	for _, want := range []string{
		"azukari_replication_runs_total{bucket=\"backup\",replica=\"local\",result=\"failed\"} 2\n",
		"azukari_replication_runs_total{bucket=\"backup\",replica=\"local\",result=\"skipped\"} 2\n",
		"azukari_replication_runs_total{bucket=\"down\",replica=\"local\",result=\"skipped\"} 1\n",
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("no %q in\n%s", want, m)
		}
	}
}

func TestSSHCommand(t *testing.T) {
	got := sshCommand(config.Replica{Kind: "ssh", Port: 2222, SSHKeyFile: "/k", KnownHostsFile: "/kh"})
	want := "ssh -F /dev/null -p 2222 -i '/k' -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes" +
		" -o StrictHostKeyChecking=yes -o UserKnownHostsFile='/kh' -o GlobalKnownHostsFile=/dev/null -o UpdateHostKeys=no" +
		" -o ConnectTimeout=30 -o ServerAliveInterval=30 -o ServerAliveCountMax=4"
	if got != want {
		t.Fatalf("sshCommand = %q", got)
	}
}

// A dir replica that symlinks lead into a bucket, or around one, is skipped untouched.
func TestReplicateOverlap(t *testing.T) {
	e := start(t)
	r := localReplica(t)
	bkt := e.cfg.Buckets[0].Path
	around := r
	around.Destination = filepath.Join(r.MountPoint, "link")
	for _, l := range [][2]string{
		{bkt, filepath.Join(r.Destination, "backup")},             // the copy is the bucket
		{filepath.Dir(e.cfg.Buckets[2].Path), around.Destination}, // the destination holds bucket full
	} {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	snapshot(t, e, "s1", "c1")
	e.srv.onPhase = func(p string) { t.Errorf("rsync %s ran", p) }
	e.srv.Replicate(context.Background(), "backup", r)
	e.srv.Replicate(context.Background(), "backup", around)
	l := events(t, e, "replication-skipped")
	if len(l) != 2 || !strings.Contains(l[0]["reason"].(string), "overlapping bucket backup's path") ||
		!strings.Contains(l[1]["reason"].(string), "overlapping bucket full's path") {
		t.Fatalf("replication-skipped: %v", l)
	}
}

// Cancelling a run kills rsync and whatever it started, reaps it and releases the bucket.
func TestReplicateCancel(t *testing.T) {
	e := start(t)
	r := localReplica(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	pid := filepath.Join(bin, "pid")
	// The child sleep keeps rsync's stdout open: only killing the group ends it before WaitDelay.
	// The pid file is written once sleep is forked: a group kill racing the shell's fork could
	// miss the new child (seen on darwin), which then held stdout past the 5 s bound below.
	script := fmt.Sprintf("#!/bin/sh\n%s 600 &\necho $$ > %s\nwait\n", sleep, pid)
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.srv.Replicate(ctx, "backup", r); close(done) }()
	var child int
	for i := 0; ; i++ {
		b, _ := os.ReadFile(pid)
		if _, err := fmt.Sscan(string(b), &child); err == nil {
			break
		}
		if i == 500 {
			t.Fatal("the fake rsync never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run outlived its cancellation")
	}
	if err := syscall.Kill(child, 0); err != syscall.ESRCH {
		t.Fatalf("rsync %d not reaped: %v", child, err)
	}
	sb := e.srv.buckets["backup"]
	if !sb.run.TryLock() {
		t.Fatal("bucket lock still held")
	}
	sb.run.Unlock()
	if f := events(t, e, "replication-failed"); len(f) != 1 || !strings.Contains(f[0]["err"].(string), "rsync check: context canceled") {
		t.Fatalf("replication-failed: %v", f)
	}
}

// A run cut in its object phase or in any deletion phase stops there: later phases do not
// run, every root on the copy keeps its refs and chunks, the next run converges and retention
// takes the lock afterwards. Real rsync fails: on an unreadable object, or after its first
// deletion through a wrapper adding --max-delete=1 to the phase under test.
func TestReplicateInterrupted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	real, err := exec.LookPath("rsync")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rsync"), []byte("#!/bin/sh\nexec "+real+" $AZUKARI_TEST_RSYNC \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AZUKARI_TEST_RSYNC", "")

	e := start(t)
	r := localReplica(t)
	ctx := context.Background()
	bkt, cp := e.cfg.Buckets[0].Path, filepath.Join(r.Destination, "backup")
	type snap struct{ c, refs, root string }
	var snaps []snap
	for _, n := range []string{"s1", "s2", "s3"} {
		c, refs, root := snapshot(t, e, n, "c"+n)
		snaps = append(snaps, snap{c, refs, root})
	}
	var phases []string
	sb := e.srv.buckets["backup"]
	// cut runs one replication failing in phase at, and checks what the copy kept.
	cut := func(at string) {
		t.Helper()
		phases = nil
		e.srv.Replicate(ctx, "backup", r)
		f := events(t, e, "replication-failed")
		if len(f) == 0 || !strings.Contains(f[len(f)-1]["err"].(string), "rsync "+at+": ") || phases[len(phases)-1] != at {
			t.Fatalf("cut in %s: phases %v, failed %v", at, phases, f)
		}
		for _, s := range snaps {
			if has(t, cp, s.root) && (!has(t, cp, s.refs) || !has(t, cp, s.c)) {
				t.Fatalf("cut in %s: root %s without its refs or chunk: %v", at, s.root, tree(t, cp))
			}
		}
		if !sb.run.TryLock() {
			t.Fatalf("cut in %s: bucket lock still held", at)
		}
		sb.run.Unlock()
	}

	// Object phase: s1's chunk unreadable, other objects copied, no root.
	e.srv.onPhase = func(p string) { phases = append(phases, p) }
	chunk := filepath.Join(bkt, "objects", snaps[0].c)
	if err := os.Chmod(chunk, 0); err != nil {
		t.Fatal(err)
	}
	cut("objects")
	if err := os.Chmod(chunk, 0o600); err != nil {
		t.Fatal(err)
	}
	if has(t, cp, snaps[0].c) || !has(t, cp, snaps[1].c) || has(t, cp, snaps[2].root) {
		t.Fatalf("after the cut object phase: %v", tree(t, cp))
	}
	e.srv.Replicate(ctx, "backup", r)
	sameTree(t, bkt, cp)

	// s1 and s2 pruned (s3 is the newest): each deletion phase in turn stops after one deletion.
	later := time.Now().Add(30 * day)
	e.srv.now = func() time.Time { return later }
	sb.ret.Enabled = true
	if p, err := e.srv.RunRetention("backup", false); err != nil || p.Snapshots != 2 || p.Chunks != 2 {
		t.Fatalf("prune: %+v, %v", p, err)
	}
	for _, at := range []string{"delete-snapshots", "delete-refs", "delete-chunks"} {
		e.srv.onPhase = func(p string) {
			phases = append(phases, p)
			v := ""
			if p == at {
				v = "--max-delete=1"
			}
			os.Setenv("AZUKARI_TEST_RSYNC", v)
		}
		cut(at)
	}
	if has(t, cp, snaps[0].root) || has(t, cp, snaps[1].root) || !has(t, cp, snaps[0].c) && !has(t, cp, snaps[1].c) {
		t.Fatalf("after the cut runs: %v", tree(t, cp))
	}
	e.srv.Replicate(ctx, "backup", r)
	sameTree(t, bkt, cp)
	if p, err := e.srv.RunRetention("backup", false); err != nil || p.Snapshots != 0 {
		t.Fatalf("retention after the runs: %+v, %v", p, err)
	}
}
