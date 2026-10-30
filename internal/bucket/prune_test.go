package bucket

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	day   = 24 * time.Hour
	keep  = 10 * day
	grace = 2 * day
)

func nsChunk(ns, c string) string {
	x := hex.EncodeToString(chunkID(c))
	return ns + "/chunks/" + x[:2] + "/" + x
}

func chunkID(c string) []byte { s := sha256.Sum256([]byte(c)); return s[:] }

// putAt stores k with body as received at at.
func putAt(t *testing.T, b *Bucket, at time.Time, k, body string) error {
	t.Helper()
	b.now = func() time.Time { return at }
	_, err := put(b, k, body)
	return err
}

// refsBody is the refs list of chunks (ADR 0007): sorted ids, concatenated.
func refsBody(chunks ...string) string {
	var ids [][]byte
	for _, c := range chunks {
		ids = append(ids, chunkID(c))
	}
	slices.SortFunc(ids, bytes.Compare)
	return string(bytes.Join(ids, nil))
}

// backup stores a complete snapshot of chunks at at: chunks, refs, root.
func backup(t *testing.T, b *Bucket, at time.Time, ns, name string, chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		must(t, putAt(t, b, at, nsChunk(ns, c), c))
	}
	must(t, putAt(t, b, at, ns+"/refs/"+name, refsBody(chunks...)))
	must(t, putAt(t, b, at, ns+"/snapshots/"+name, "root "+name))
}

func objects(t *testing.T, b *Bucket) []string {
	t.Helper()
	var got []string
	must(t, filepath.WalkDir(filepath.Join(b.path, "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(b.path, "objects"), p)
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	}))
	slices.Sort(got)
	return got
}

func assertObjects(t *testing.T, b *Bucket, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := objects(t, b); !slices.Equal(got, want) {
		t.Fatalf("objects =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func prune(t *testing.T, b *Bucket, now time.Time, dry bool) Pruned {
	t.Helper()
	p, err := b.Prune(now, keep, grace, dry)
	must(t, err)
	return p
}

// setupHistory: snapshots s1..s4 of namespace a, s1 and s2 older than keep, s3 within it,
// s4 the newest; c3 is first stored by s2 (old) but referenced by s3 and s4.
func setupHistory(t *testing.T) (*Bucket, time.Time) {
	b := newBucket(t)
	t0 := testNow
	must(t, putAt(t, b, t0, "a/config", "{}"))
	backup(t, b, t0, "a", "s1", "c1", "c2")
	backup(t, b, t0.Add(5*day), "a", "s2", "c2", "c3")
	backup(t, b, t0.Add(18*day), "a", "s3", "c3")
	backup(t, b, t0.Add(24*day), "a", "s4", "c3", "c4")
	return b, t0.Add(25 * day)
}

var historyKept = []string{"a/config", "a/refs/s3", "a/refs/s4", "a/snapshots/s3", "a/snapshots/s4"}

func TestPruneOldSnapshots(t *testing.T) {
	b, now := setupHistory(t)
	before := objects(t, b)
	var synced []string
	b.syncDir = func(dir string) error {
		rel, _ := filepath.Rel(filepath.Join(b.path, "objects"), dir)
		synced = append(synced, filepath.ToSlash(rel))
		return syncDir(dir)
	}

	// Dry run: same counts, nothing touched.
	want := Pruned{Snapshots: 2, Refs: 2, Chunks: 2,
		Bytes: int64(len("root s1") + len("root s2") + 2*2*32 + len("c1") + len("c2"))}
	if p := prune(t, b, now, true); p != want {
		t.Fatalf("dry Prune = %+v, want %+v", p, want)
	}
	if got := objects(t, b); !slices.Equal(got, before) || synced != nil {
		t.Fatalf("dry run changed %v, synced %v", got, synced)
	}

	if p := prune(t, b, now, false); p != want {
		t.Fatalf("Prune = %+v, want %+v", p, want)
	}
	assertObjects(t, b, append(historyKept, nsChunk("a", "c3"), nsChunk("a", "c4"))...)
	// Roots durably gone before refs, refs before chunks.
	if len(synced) != 4 || synced[0] != "a/snapshots" || synced[1] != "a/refs" ||
		!strings.HasPrefix(synced[2], "a/chunks/") || !strings.HasPrefix(synced[3], "a/chunks/") {
		t.Fatalf("synced %v", synced)
	}
	// A second run finds nothing more.
	if p := prune(t, b, now, false); p != (Pruned{}) {
		t.Fatalf("second Prune = %+v", p)
	}
}

func TestPruneKeepsNewestOfStoppedNode(t *testing.T) {
	b := newBucket(t)
	t0 := testNow
	backup(t, b, t0, "a", "s1", "c1")
	backup(t, b, t0.Add(day), "a", "s2", "c2")
	// The root without refs is not a complete snapshot: neither newest nor pruned. Put refuses
	// one since ADR 0025: a store written before it may still hold one.
	must(t, putAt(t, b, t0.Add(2*day), "a/refs/s3", ""))
	must(t, putAt(t, b, t0.Add(2*day), "a/snapshots/s3", "half"))
	must(t, os.Remove(filepath.Join(b.path, "objects", "a", "refs", "s3")))
	p := prune(t, b, t0.Add(100*day), false)
	if p.Snapshots != 1 || p.Refs != 1 || p.Chunks != 1 {
		t.Fatalf("Prune = %+v", p)
	}
	assertObjects(t, b, "a/refs/s2", "a/snapshots/s2", "a/snapshots/s3", nsChunk("a", "c2"))
}

func TestPruneOrphansAfterGrace(t *testing.T) {
	b := newBucket(t)
	t0 := testNow
	// An interrupted run: chunks and refs, no root; and a chunk nothing names.
	must(t, putAt(t, b, t0, nsChunk("a", "c1"), "c1"))
	must(t, putAt(t, b, t0, "a/refs/s1", refsBody("c1")))
	must(t, putAt(t, b, t0, nsChunk("a", "lost"), "lost"))
	all := objects(t, b)
	if p := prune(t, b, t0.Add(grace), false); p != (Pruned{}) {
		t.Fatalf("Prune within grace = %+v", p)
	}
	assertObjects(t, b, all...)
	if p := prune(t, b, t0.Add(grace+time.Nanosecond), false); p.Refs != 1 || p.Chunks != 2 {
		t.Fatalf("Prune after grace = %+v", p)
	}
	assertObjects(t, b)
}

// A backup interleaved with prunes (ADR 0007): a chunk the run found present may be pruned
// before its refs arrive; refs-incomplete makes the client upload it again, and once refs is
// stored every chunk it names is kept.
func TestPruneDuringBackup(t *testing.T) {
	b := newBucket(t)
	t0 := testNow
	must(t, putAt(t, b, t0, nsChunk("a", "old"), "old")) // left by an interrupted run
	now := t0.Add(3 * day)
	// The run asks missing: "old" is present, so it uploads only "new".
	if m, err := b.Missing([]string{nsChunk("a", "old"), nsChunk("a", "new")}); err != nil || len(m) != 1 {
		t.Fatalf("Missing = %v, %v", m, err)
	}
	must(t, putAt(t, b, now, nsChunk("a", "new"), "new"))
	must(t, putAt(t, b, now.Add(-grace+time.Hour), nsChunk("a", "mid"), "mid"))
	if p := prune(t, b, now, false); p.Chunks != 1 {
		t.Fatalf("Prune = %+v", p)
	}
	if err := putAt(t, b, now, "a/refs/s1", refsBody("old", "new", "mid")); !errors.Is(err, ErrRefsIncomplete) {
		t.Fatalf("refs Put = %v", err)
	}
	must(t, putAt(t, b, now, nsChunk("a", "old"), "old"))
	must(t, putAt(t, b, now, "a/refs/s1", refsBody("old", "new", "mid")))
	// refs stored, root not yet: "mid" is now older than grace but named by refs.
	if p := prune(t, b, now.Add(2*time.Hour), false); p != (Pruned{}) {
		t.Fatalf("Prune = %+v", p)
	}
	must(t, putAt(t, b, now.Add(2*time.Hour), "a/snapshots/s1", "root"))
	prune(t, b, now.Add(keep+day), false)
	assertObjects(t, b, "a/refs/s1", "a/snapshots/s1", nsChunk("a", "mid"), nsChunk("a", "new"), nsChunk("a", "old"))
}

// A root is accepted only with its refs (ADR 0025); a retry of a stored root still answers 200.
func TestRootRequiresRefs(t *testing.T) {
	b := newBucket(t)
	if err := putAt(t, b, testNow, "a/snapshots/s1", "root"); !errors.Is(err, ErrRefsIncomplete) {
		t.Fatalf("root without refs: %v", err)
	}
	assertClean(t, b)
	backup(t, b, testNow, "a", "s1", "c1")
	if created, err := put(b, "a/snapshots/s1", "root s1"); created || err != nil {
		t.Fatalf("retry = %v, %v", created, err)
	}
}

// The M3 review's run paused past grace after its refs: pruning removes the orphan refs and
// their chunks, so the late root is refused rather than stored without its chunks.
func TestRootAfterOrphanPruned(t *testing.T) {
	b := newBucket(t)
	must(t, putAt(t, b, testNow, nsChunk("a", "c1"), "c1"))
	must(t, putAt(t, b, testNow, "a/refs/s1", refsBody("c1")))
	if p := prune(t, b, testNow.Add(grace+time.Nanosecond), false); p.Refs != 1 || p.Chunks != 1 {
		t.Fatalf("Prune = %+v", p)
	}
	if err := putAt(t, b, testNow.Add(grace+time.Hour), "a/snapshots/s1", "root"); !errors.Is(err, ErrRefsIncomplete) {
		t.Fatalf("late root: %v", err)
	}
	assertObjects(t, b)
}

func TestPruneUnavailable(t *testing.T) {
	b, now := setupHistory(t)
	b.syncDir = func(string) error { return errInjected }
	if _, err := b.Prune(now, keep, grace, false); !errors.Is(err, ErrBecameUnavailable) {
		t.Fatalf("Prune = %v", err)
	}
	if _, err := b.Prune(now, keep, grace, true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Prune of an unavailable bucket = %v", err)
	}
}

func TestSnapshotsAndDelete(t *testing.T) {
	b, _ := setupHistory(t)
	list, err := b.Snapshots("a")
	must(t, err)
	var names []string
	for _, s := range list {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"s1", "s2", "s3", "s4"}) || !list[1].Received.Equal(testNow.Add(5*day)) ||
		list[0].Bytes != int64(len("root s1")+2*32) {
		t.Fatalf("Snapshots = %+v", list)
	}
	if list, err := b.Snapshots("nobody"); err != nil || len(list) != 0 {
		t.Fatalf("Snapshots(nobody) = %v, %v", list, err)
	}
	if _, err := b.Snapshots("a/b"); err == nil {
		t.Fatal("Snapshots(a/b) accepted")
	}

	must(t, b.DeleteSnapshot("a", "s4"))
	if err := b.DeleteSnapshot("a", "s4"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteSnapshot = %v", err)
	}
	if err := b.DeleteSnapshot("a", "../x"); err == nil {
		t.Fatal("DeleteSnapshot(../x) accepted")
	}
	// Its chunks go at the next run: s3 is now the newest and keeps c3; c4 is unreferenced.
	p := prune(t, b, testNow.Add(27*day), false)
	if p.Snapshots != 2 || p.Chunks != 3 {
		t.Fatalf("Prune = %+v", p)
	}
	assertObjects(t, b, "a/config", "a/refs/s3", "a/snapshots/s3", nsChunk("a", "c3"))
}

// A prune waiting for a namespace's lock while another call fails the bucket (a deletion whose
// fsync failed) deletes nothing once it gets the lock (ADR 0007).
func TestPruneUnavailableWhileWaiting(t *testing.T) {
	b := newBucket(t)
	must(t, putAt(t, b, testNow, nsChunk("a", "x"), "x")) // a: one orphan chunk, deleted last
	backup(t, b, testNow, "b", "s1", "c1")
	backup(t, b, testNow.Add(day), "b", "s2", "c2")
	want := slices.DeleteFunc(objects(t, b), func(o string) bool { return o == nsChunk("a", "x") })

	unlock := b.LockNamespace("b")
	syncDir, aDone := b.syncDir, make(chan struct{})
	var once sync.Once
	b.syncDir = func(d string) error { once.Do(func() { close(aDone) }); return syncDir(d) }
	res := make(chan error)
	go func() { _, err := b.Prune(testNow.Add(keep+2*day), keep, grace, false); res <- err }()
	<-aDone // past Prune's first check, on its way to b's lock
	b.fail(errInjected)
	unlock()
	if err := <-res; !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Prune = %v", err)
	}
	assertObjects(t, b, want...)
}

// One flipped byte in a retained refs body aborts the run before any chunk goes; reading a
// corrupt object leaves the bucket available. An unsorted list too.
func TestPruneCorruptRefs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]byte)
		want error
	}{
		{"flipped", func(f []byte) { f[HeaderSize+5] ^= 1 }, ErrSHA256Mismatch},
		{"unsorted", func(f []byte) {
			body := f[HeaderSize:]
			var x [32]byte
			copy(x[:], body[:32])
			copy(body[:32], body[32:64])
			copy(body[32:64], x[:])
			h, _ := DecodeHeader(f[:HeaderSize], int64(len(f)))
			h.SHA256 = sha256.Sum256(body)
			copy(f, h.Encode())
		}, ErrRefsInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, now := setupHistory(t)
			p := b.objectPath("a/refs/s4") // c3 and c4: retained
			f, err := os.ReadFile(p)
			must(t, err)
			tc.edit(f)
			must(t, os.WriteFile(p, f, 0o600))
			if _, err := b.Prune(now, keep, grace, false); !errors.Is(err, tc.want) {
				t.Fatalf("Prune = %v, want %v", err, tc.want)
			}
			if err := b.Err(); err != nil {
				t.Fatalf("bucket failed: %v", err)
			}
			for _, c := range []string{"c1", "c2", "c3", "c4"} {
				if _, err := os.Stat(b.objectPath(nsChunk("a", c))); err != nil {
					t.Errorf("chunk %s: %v", c, err)
				}
			}
		})
	}
}

// A listing waits for a deletion holding the namespace's lock and never shows half of it.
func TestSnapshotsDuringDelete(t *testing.T) {
	b, _ := setupHistory(t)
	unlock := b.LockNamespace("a") // a deletion of s4 in progress
	res := make(chan []Snapshot)
	go func() {
		list, err := b.Snapshots("a")
		if err != nil {
			t.Error(err)
		}
		res <- list
	}()
	time.Sleep(50 * time.Millisecond)
	must(t, os.Remove(b.objectPath("a/snapshots/s4")))
	must(t, os.Remove(b.objectPath("a/refs/s4")))
	select {
	case <-res:
		t.Fatal("listed while the namespace was locked")
	default:
	}
	unlock()
	list := <-res
	if len(list) != 3 || list[2].Name != "s3" || list[2].Bytes != int64(len("root s3")+32) {
		t.Fatalf("Snapshots = %+v", list)
	}
}
