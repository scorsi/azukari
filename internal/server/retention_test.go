package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/bucket"
)

const day = 24 * time.Hour

// store puts body at k in the backup bucket, received now (real clock).
func store(t *testing.T, e *env, k, body string) {
	t.Helper()
	if _, err := e.srv.buckets["backup"].b.Put(k, int64(len(body)), sha256.Sum256([]byte(body)), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
}

func TestRunRetention(t *testing.T) {
	e := start(t)
	id := sha256.Sum256([]byte("c1"))
	x := fmt.Sprintf("%x", id)
	store(t, e, "node-a/chunks/"+x[:2]+"/"+x, "c1")
	for _, s := range []string{"s1", "s2"} {
		store(t, e, "node-a/refs/data/"+s, string(id[:]))
		store(t, e, "node-a/snapshots/data/"+s, "root "+s)
	}
	later := time.Now().Add(30 * day)
	e.srv.now = func() time.Time { return later }
	want := bucket.Pruned{Snapshots: 1, Refs: 1, Bytes: int64(len("root s1") + 32)}
	sample := func(m string) string { return fmt.Sprintf("azukari_%s{bucket=\"backup\"} ", m) }

	// enabled = false: dry whatever is asked; then enabled with --dry-run: dry too.
	for i, dryRun := range []bool{false, true} {
		if i == 1 {
			e.srv.buckets["backup"].ret.Enabled = true
		}
		if p, err := e.srv.RunRetention("backup", dryRun); err != nil || p != want {
			t.Fatalf("dry run %d = %+v, %v", i, p, err)
		}
		if l, err := e.srv.Snapshots("backup", "node-a", "data"); err != nil || len(l) != 2 {
			t.Fatalf("Snapshots after dry run = %v, %v", l, err)
		}
	}
	if n := strings.Count(e.logs.String(), `"dry_run":true`); n != 2 {
		t.Fatalf("%d dry prune-completed lines:\n%s", n, e.logs)
	}
	if m := scrape(t, e.srv); !strings.Contains(m, sample("pruned_bytes_total")+"0\n") ||
		!strings.Contains(m, sample("prune_last_success_timestamp_seconds")+"0\n") {
		t.Fatalf("metrics after dry runs:\n%s", m)
	}

	if p, err := e.srv.RunRetention("backup", false); err != nil || p != want {
		t.Fatalf("run = %+v, %v", p, err)
	}
	if !strings.Contains(e.logs.String(), `"dry_run":false`) {
		t.Fatalf("no real prune-completed:\n%s", e.logs)
	}
	if m := scrape(t, e.srv); !strings.Contains(m, sample("pruned_bytes_total")+fmt.Sprint(want.Bytes)+"\n") ||
		!strings.Contains(m, sample("prune_last_success_timestamp_seconds")+fmt.Sprint(later.Unix())+"\n") {
		t.Fatalf("metrics after the run:\n%s", m)
	}
	l, err := e.srv.Snapshots("backup", "node-a", "data")
	if err != nil || len(l) != 1 || l[0].Name != "s2" || l[0].Bytes != int64(len("root s2")+32) {
		t.Fatalf("Snapshots = %+v, %v", l, err)
	}
	if err := e.srv.DeleteSnapshot("backup", "node-a", "data", "s2"); err != nil {
		t.Fatal(err)
	}
	if l, err := e.srv.Snapshots("backup", "node-a", "data"); err != nil || len(l) != 0 {
		t.Fatalf("Snapshots after delete = %v, %v", l, err)
	}

	if _, err := e.srv.RunRetention("down", true); !errors.Is(err, bucket.ErrUnavailable) ||
		!strings.Contains(e.logs.String(), `"prune-failed"`) {
		t.Fatalf("unavailable bucket: %v\n%s", err, e.logs)
	}
	for _, err := range []error{
		func() error { _, err := e.srv.RunRetention("nope", true); return err }(),
		e.srv.DeleteSnapshot("nope", "node-a", "data", "s1"),
		func() error { _, err := e.srv.Snapshots("nope", "node-a", "data"); return err }(),
	} {
		if !errors.Is(err, ErrUnknownBucket) {
			t.Fatalf("unknown bucket: %v", err)
		}
	}
}

func TestNextRun(t *testing.T) {
	loc := time.FixedZone("x", 3600)
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 10, 30, 1, 0, 0, 0, loc), time.Date(2026, 10, 30, 3, 30, 0, 0, loc)},
		{time.Date(2026, 10, 30, 3, 30, 0, 0, loc), time.Date(2026, 10, 31, 3, 30, 0, 0, loc)},
		{time.Date(2026, 12, 31, 23, 0, 0, 0, loc), time.Date(2027, 1, 1, 3, 30, 0, 0, loc)},
	} {
		if got := nextRun(tc.now, 3, 30); !got.Equal(tc.want) {
			t.Errorf("nextRun(%v) = %v, want %v", tc.now, got, tc.want)
		}
	}
}

// The schedule waits for run_at on the injected clock, runs, then waits for the next day.
func TestSchedule(t *testing.T) {
	e := start(t)
	var mu sync.Mutex
	now := time.Date(2026, 10, 30, 1, 0, 0, 0, time.Local)
	e.srv.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	waits, fire := make(chan time.Duration), make(chan time.Time)
	e.srv.after = func(d time.Duration) <-chan time.Time { waits <- d; return fire }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.srv.schedule(ctx, "backup", "03:30"); close(done) }()

	if d := <-waits; d != 150*time.Minute {
		t.Fatalf("first wait %v", d)
	}
	if strings.Contains(e.logs.String(), `"prune-completed"`) {
		t.Fatal("ran before run_at")
	}
	mu.Lock()
	now = time.Date(2026, 10, 30, 3, 30, 1, 0, time.Local)
	mu.Unlock()
	fire <- now
	if d := <-waits; d != 24*time.Hour-time.Second {
		t.Fatalf("second wait %v", d)
	}
	if n := strings.Count(e.logs.String(), `"prune-completed"`); n != 1 {
		t.Fatalf("%d runs:\n%s", n, e.logs)
	}
	cancel()
	<-done
}
