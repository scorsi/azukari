package bucket

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/key"
)

var testNow = time.Date(2026, 10, 29, 12, 0, 0, 123, time.UTC)

func newBucket(t *testing.T) *Bucket {
	t.Helper()
	mp := t.TempDir()
	path := filepath.Join(mp, "backup")
	if s, err := Open(path, mp, mounted); s != Created || err != nil {
		t.Fatalf("Open = %v, %v", s, err)
	}
	b := New(path, 1<<20, 1000)
	b.freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	b.now = func() time.Time { return testNow }
	return b
}

func put(b *Bucket, k, body string) (bool, error) {
	return b.Put(k, int64(len(body)), sha256.Sum256([]byte(body)), strings.NewReader(body))
}

// assertClean checks that objects/ holds exactly the keys given and tmp/ is empty.
func assertClean(t *testing.T, b *Bucket, keys ...string) {
	t.Helper()
	var got []string
	must(t, filepath.WalkDir(filepath.Join(b.path, "objects"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(b.path, "objects"), p)
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	}))
	if fmt.Sprint(got) != fmt.Sprint(keys) {
		t.Fatalf("objects = %v, want %v", got, keys)
	}
	if e, err := os.ReadDir(filepath.Join(b.path, "tmp")); err != nil || len(e) != 0 {
		t.Fatalf("tmp/ = %v, %v", e, err)
	}
}

// ADR 0015: "a" and "A" are two keys (bucket filesystems are case-sensitive, ADR 0004).
func TestKeysCaseSensitive(t *testing.T) {
	b := newBucket(t)
	probe := filepath.Join(b.path, "tmp", "x")
	must(t, os.WriteFile(probe, nil, 0o600))
	_, err := os.Stat(filepath.Join(b.path, "tmp", "X"))
	must(t, os.Remove(probe))
	if err == nil {
		t.Skip("case-insensitive filesystem (e.g. macOS APFS); ADR 0004 requires a case-sensitive one")
	}
	for _, k := range []string{"n/a", "n/A"} {
		if created, err := put(b, k, k); !created || err != nil {
			t.Fatalf("Put(%q) = %v, %v", k, created, err)
		}
	}
	for _, k := range []string{"n/a", "n/A"} {
		_, rc, err := b.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != k {
			t.Fatalf("Get(%q) = %q", k, got)
		}
	}
}

func TestPutCreateRetryConflict(t *testing.T) {
	b := newBucket(t)
	if created, err := put(b, "n/chunks/ab/x", "hello"); !created || err != nil {
		t.Fatalf("first Put = %v, %v", created, err)
	}
	if created, err := put(b, "n/chunks/ab/x", "hello"); created || err != nil {
		t.Fatalf("retry = %v, %v; want 200", created, err)
	}
	if _, err := put(b, "n/chunks/ab/x", "other"); !errors.Is(err, ErrExists) {
		t.Fatalf("other bytes: %v; want ErrExists", err)
	}
	// A key under an object, and an object over a directory: 409, the bucket keeps serving.
	if _, err := put(b, "n/chunks/ab/x/y", "z"); !errors.Is(err, ErrExists) {
		t.Fatalf("under an object: %v", err)
	}
	if _, err := put(b, "n/chunks", "z"); !errors.Is(err, ErrExists) {
		t.Fatalf("over a directory: %v", err)
	}
	if b.Err() != nil {
		t.Fatal(b.Err())
	}
	assertClean(t, b, "n/chunks/ab/x")

	h, body, err := b.Get("n/chunks/ab/x")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "hello" {
		t.Fatalf("body %q, %v", data, err)
	}
	if want := (Header{Size: 5, SHA256: sha256.Sum256([]byte("hello")), ReceiveTime: testNow.UnixNano()}); h != want {
		t.Fatalf("header %+v, want %+v", h, want)
	}
	for _, k := range []string{"n/absent", "n/chunks", "n/chunks/ab/x/y"} {
		if _, _, err := b.Get(k); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%s) = %v", k, err)
		}
	}
	if _, _, err := b.Get("../x"); !errors.Is(err, key.ErrInvalid) {
		t.Errorf("Get(../x) = %v", err)
	}
}

type cutReader struct{ r io.Reader }

func (c cutReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestPutRefusalsLeaveNothing(t *testing.T) {
	b := newBucket(t)
	sum := sha256.Sum256([]byte("hello"))
	for name, c := range map[string]struct {
		size int64
		sum  [32]byte
		body io.Reader
		want error
	}{
		"hash":      {5, sha256.Sum256([]byte("world")), strings.NewReader("hello"), ErrSHA256Mismatch},
		"short":     {6, sum, strings.NewReader("hello"), ErrSizeMismatch},
		"long":      {4, sum, strings.NewReader("hello"), ErrSizeMismatch},
		"cut":       {10, sum, cutReader{strings.NewReader("hello")}, io.ErrUnexpectedEOF},
		"too large": {1<<20 + 1, sum, strings.NewReader("hello"), ErrTooLarge},
		"negative":  {-1, sum, strings.NewReader(""), ErrTooLarge},
		"bad key":   {5, sum, strings.NewReader("hello"), key.ErrInvalid},
	} {
		k := "n/x"
		if name == "bad key" {
			k = "n/../x"
		}
		if created, err := b.Put(k, c.size, c.sum, c.body); created || !errors.Is(err, c.want) {
			t.Errorf("%s: %v, %v; want %v", name, created, err, c.want)
		}
	}
	if b.Err() != nil {
		t.Fatal(b.Err())
	}
	assertClean(t, b)
}

func TestPutConcurrent(t *testing.T) {
	b := newBucket(t)
	var wg sync.WaitGroup
	results := make(chan bool, 16)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, err := put(b, "n/k", "same bytes")
			if err != nil {
				t.Error(err)
			}
			results <- created
		}()
	}
	wg.Wait()
	close(results)
	n := 0
	for c := range results {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d Puts created the object, want 1", n)
	}
	assertClean(t, b, "n/k")
}

func TestPutDiskReserve(t *testing.T) {
	b := newBucket(t)
	free := int64(1000 + HeaderSize + 100) // reserve 1000
	b.freeSpace = func(string) (int64, error) { return free, nil }
	if _, err := b.Put("n/a", 101, [32]byte{}, strings.NewReader("")); !errors.Is(err, ErrDiskReserve) {
		t.Fatalf("over the reserve: %v", err)
	}
	if _, err := put(b, "n/a", strings.Repeat("x", 100)); err != nil {
		t.Fatalf("at the reserve: %v", err)
	}
	assertClean(t, b, "n/a")
	if n, err := FreeSpace(b.path); n <= 0 || err != nil {
		t.Fatalf("FreeSpace = %d, %v", n, err)
	}
}

// gated is a body that blocks its first read until gate closes, then reads r.
type gated struct {
	gate <-chan struct{}
	r    io.Reader
}

func (g gated) Read(p []byte) (int, error) { <-g.gate; return g.r.Read(p) }

// Concurrent uploads on two buckets of one filesystem share its in-flight account: when only
// one fits above the reserve, the others get ErrDiskReserve before writing anything.
func TestPutDiskReserveConcurrent(t *testing.T) {
	mp := t.TempDir()
	body := strings.Repeat("x", 100)
	free := int64(1000 + HeaderSize + 100 + HeaderSize + 99) // reserve 1000: room for one upload, not two
	var bs []*Bucket
	for _, name := range []string{"one", "two"} { // each keeps its own reserve, both 1000 here
		path := filepath.Join(mp, name)
		if s, err := Open(path, mp, mounted); s != Created || err != nil {
			t.Fatalf("Open = %v, %v", s, err)
		}
		b := New(path, 1<<20, 1000)
		b.freeSpace = func(string) (int64, error) { return free, nil }
		bs = append(bs, b)
	}
	if bs[0].space != bs[1].space {
		t.Fatal("buckets on one filesystem do not share an account")
	}

	const n = 8
	gate := make(chan struct{})
	errs := make(chan error, n)
	for i := range n {
		go func() {
			_, err := bs[i%2].Put(fmt.Sprintf("n/%d", i), 100, sha256.Sum256([]byte(body)),
				gated{gate, strings.NewReader(body)})
			errs <- err
		}()
	}
	for range n - 1 { // the refused ones answer while the winner still waits for its body
		if err := <-errs; !errors.Is(err, ErrDiskReserve) {
			t.Fatalf("Put = %v, want ErrDiskReserve", err)
		}
	}
	close(gate)
	if err := <-errs; err != nil {
		t.Fatalf("the admitted Put = %v", err)
	}
	// The fake statfs ignores the winner's bytes: a failed Put must release its reservation too.
	cut := make(chan struct{})
	close(cut)
	if _, err := bs[0].Put("n/cut", 100, sha256.Sum256([]byte(body)), gated{cut, strings.NewReader("short")}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("cut Put = %v", err)
	}
	if _, err := put(bs[1], "n/after", body); err != nil {
		t.Fatalf("Put after a failed one = %v, its reservation was not released", err)
	}
	if bs[0].space.reserved != 0 {
		t.Fatalf("reserved = %d after every Put ended", bs[0].space.reserved)
	}
	for _, b := range bs {
		if e, err := os.ReadDir(filepath.Join(b.path, "tmp")); err != nil || len(e) != 0 {
			t.Fatalf("tmp/ = %v, %v", e, err)
		}
	}
}

func TestUnavailable(t *testing.T) {
	b := newBucket(t)
	must(t, os.RemoveAll(filepath.Join(b.path, "tmp"))) // the next write fails
	if _, err := put(b, "n/a", "x"); !errors.Is(err, ErrBecameUnavailable) {
		t.Fatalf("Put = %v, want ErrBecameUnavailable", err)
	}
	if b.Err() == nil {
		t.Fatal("Err() = nil after a write error")
	}
	must(t, os.Mkdir(filepath.Join(b.path, "tmp"), 0o700)) // stays unavailable until restart
	_, err := put(b, "n/a", "x")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBecameUnavailable) {
		t.Fatalf("Put = %v", err)
	}
	if _, _, err := b.Get("n/a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get = %v", err)
	}
	if _, err := b.Missing([]string{"n/a"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Missing = %v", err)
	}
	if _, _, err := b.List("", "", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("List = %v", err)
	}
}

func TestMissing(t *testing.T) {
	b := newBucket(t)
	for _, k := range []string{"n/a", "n/c/d"} {
		if _, err := put(b, k, k); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.Missing([]string{"n/a", "n/b", "n/c", "n/c/d", "n/a/x"})
	if err != nil || fmt.Sprint(got) != "[n/b n/c n/a/x]" {
		t.Fatalf("Missing = %v, %v", got, err)
	}
	if _, err := b.Missing([]string{"n/a", "/x"}); !errors.Is(err, key.ErrInvalid) {
		t.Fatalf("invalid key: %v", err)
	}
}

func TestList(t *testing.T) {
	b := newBucket(t)
	// "a.c" < "a/b" < "a0" in byte order: directory walk order must not leak through.
	keys := []string{"a/b", "a.c", "a0", "a/b.d/e", "a/b-", "b/x/y/z", "b/x0", "c"}
	for _, k := range keys {
		if _, err := put(b, k, k); err != nil {
			t.Fatal(err)
		}
	}
	all := []string{"a-", "a.c", "a/b", "a/b-", "a/b.d/e", "a0", "b/x/y/z", "b/x0", "c"}
	if _, err := put(b, "a-", "a-"); err != nil {
		t.Fatal(err)
	}
	listKeys := func(prefix, after string, limit int) (string, string) {
		t.Helper()
		objs, next, err := b.List(prefix, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		var ks []string
		for _, o := range objs {
			if o.Size != uint64(len(o.Key)) || o.SHA256 != sha256.Sum256([]byte(o.Key)) {
				t.Fatalf("%s: size %d sha %x", o.Key, o.Size, o.SHA256)
			}
			ks = append(ks, o.Key)
		}
		return strings.Join(ks, " "), next
	}
	if got, next := listKeys("", "", 100); got != strings.Join(all, " ") || next != "" {
		t.Fatalf("all: %q next %q", got, next)
	}
	// Paginate by 2 and get every key once, in order.
	var pages []string
	after := ""
	for {
		got, next := listKeys("", after, 2)
		pages = append(pages, got)
		if next == "" {
			break
		}
		after = next
	}
	if strings.Join(pages, " ") != strings.Join(all, " ") {
		t.Fatalf("pages %q", pages)
	}
	for _, c := range []struct{ prefix, after, want string }{
		{"a/", "", "a/b a/b- a/b.d/e"},
		{"a/b", "", "a/b a/b- a/b.d/e"},
		{"a/b.", "", "a/b.d/e"},
		{"a", "a/b-", "a/b.d/e a0"},
		{"b/x", "", "b/x/y/z b/x0"},
		{"b/x/", "b/x/y/z", ""},
		{"", "b", "b/x/y/z b/x0 c"},
		{"zz", "", ""},
	} {
		if got, next := listKeys(c.prefix, c.after, 100); got != c.want || next != "" {
			t.Errorf("List(%q, %q) = %q, next %q; want %q", c.prefix, c.after, got, next, c.want)
		}
	}
	if got, next := listKeys("a", "", 3); got != "a- a.c a/b" || next != "a/b" {
		t.Errorf("limit: %q next %q", got, next)
	}
	// An exact page leaves next empty.
	if _, next := listKeys("a/", "", 3); next != "" {
		t.Errorf("exact page: next %q", next)
	}
	if _, _, err := b.List("", "", 0); err == nil {
		t.Error("limit 0 accepted")
	}
}

// The newest root of each export (ADR 0030), whatever the order of names; roots of another
// namespace or at another depth do not count.
func TestLastSnapshots(t *testing.T) {
	b := newBucket(t)
	if last, err := b.LastSnapshots("n"); len(last) != 0 || err != nil {
		t.Fatalf("empty: %v, %v", last, err)
	}
	h := time.Hour
	for _, r := range []struct {
		k  string
		at time.Duration
	}{
		{"n/snapshots/d/s1", 0}, {"n/snapshots/d/s2", h}, {"n/snapshots/d/s0", -h},
		{"n/snapshots/e/s1", 3 * h}, {"n/snapshots/flat", 9 * h}, {"n/snapshots/e/x/deep", 9 * h},
		{"m/snapshots/d/s9", 9 * h},
	} {
		must(t, putAt(t, b, testNow, strings.Replace(r.k, "/snapshots/", "/refs/", 1), "")) // ADR 0025
		must(t, putAt(t, b, testNow.Add(r.at), r.k, r.k))
	}
	last, err := b.LastSnapshots("n")
	if err != nil || len(last) != 2 || !last["d"].Equal(testNow.Add(h)) || !last["e"].Equal(testNow.Add(3*h)) {
		t.Fatalf("LastSnapshots = %v, %v", last, err)
	}
	if _, err := b.LastSnapshots("n/snapshots"); !errors.Is(err, key.ErrInvalid) {
		t.Fatalf("bad namespace: %v", err)
	}
}

// The object file is the header then the body, readable offline (ADR 0004).
func TestObjectFileLayout(t *testing.T) {
	b := newBucket(t)
	if _, err := put(b, "n/o", "hello"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(b.path, "objects", "n", "o"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(Header{Size: 5, SHA256: sha256.Sum256([]byte("hello")), ReceiveTime: testNow.UnixNano()}.Encode(), "hello"...)
	if !bytes.Equal(data, want) {
		t.Fatalf("file %x\nwant %x", data, want)
	}
}

func chunkKey(id [32]byte) string {
	h := fmt.Sprintf("%x", id)
	return "n/chunks/" + h[:2] + "/" + h
}

func TestRefs(t *testing.T) {
	b := newBucket(t)
	lo, hi := [32]byte{1}, [32]byte{2}
	refs := string(lo[:]) + string(hi[:])
	if _, err := put(b, chunkKey(lo), "lo"); err != nil {
		t.Fatal(err)
	}
	if created, err := put(b, "n/refs/d/s", refs); created || !errors.Is(err, ErrRefsIncomplete) {
		t.Fatalf("incomplete: %v, %v", created, err)
	}
	// Malformed lists are refused as such, before any missing chunk (hi is absent).
	for name, body := range map[string]string{
		"unsorted":  string(hi[:]) + string(lo[:]),
		"duplicate": string(lo[:]) + string(lo[:]),
		"partial":   refs + "x",
	} {
		if _, err := put(b, "n/refs/d/s", body); !errors.Is(err, ErrRefsInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	assertClean(t, b, chunkKey(lo))
	if _, err := put(b, chunkKey(hi), "hi"); err != nil {
		t.Fatal(err)
	}
	if created, err := put(b, "n/refs/d/s", refs); !created || err != nil {
		t.Fatalf("complete: %v, %v", created, err)
	}
	// Other namespaces' chunks do not count, and other keys are not refs lists.
	if _, err := put(b, "m/refs/d/s", refs); !errors.Is(err, ErrRefsIncomplete) {
		t.Fatalf("other namespace: %v", err)
	}
	if _, err := put(b, "n/snapshots/d/s", "x"); err != nil {
		t.Fatal(err)
	}
}

// A refs Put waits for its namespace's lock, which pruning holds for its run (ADR 0007).
func TestRefsWaitsForLock(t *testing.T) {
	b := newBucket(t)
	unlock := b.LockNamespace("n")
	done := make(chan error)
	go func() { _, err := put(b, "n/refs/d/s", ""); done <- err }()
	if _, err := put(b, "m/refs/d/s", ""); err != nil { // another namespace is not held
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("Put finished under the lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
