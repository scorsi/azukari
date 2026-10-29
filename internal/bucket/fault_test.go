package bucket

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var errInjected = errors.New("injected I/O error")

func TestPutSyncsDirectoryChain(t *testing.T) {
	b := newBucket(t)
	var synced []string
	b.syncDir = func(dir string) error {
		rel, _ := filepath.Rel(b.path, dir)
		synced = append(synced, filepath.ToSlash(rel))
		return syncDir(dir)
	}
	want := []string{"objects/n/x/y", "objects/n/x", "objects/n", "objects"}
	for _, wantCreated := range []bool{true, false} { // 201, then the 200 retry
		synced = nil
		if created, err := put(b, "n/x/y/z", "hello"); created != wantCreated || err != nil {
			t.Fatalf("Put = %v, %v", created, err)
		}
		if !slices.Equal(synced, want) {
			t.Fatalf("synced %v, want %v", synced, want)
		}
	}
}

func TestPutFaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inject  func(b *Bucket)
		retry   bool     // the fault hits the 200 retry of an existing object
		stopped bool     // the object may be visible: uncertain commit
		objects []string // left under objects/
	}{
		{"write", func(b *Bucket) { b.write = func(*os.File, []byte) (int, error) { return 0, errInjected } }, false, false, nil},
		{"file-sync", func(b *Bucket) { b.syncFile = func(*os.File) error { return errInjected } }, false, false, nil},
		{"link", func(b *Bucket) { b.link = func(string, string) error { return errInjected } }, false, false, nil},
		{"dir-sync", func(b *Bucket) { b.syncDir = func(string) error { return errInjected } }, false, true, []string{"n/a"}},
		{"dir-sync-retry", func(b *Bucket) { b.syncDir = func(string) error { return errInjected } }, true, true, []string{"n/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBucket(t)
			if tc.retry {
				if _, err := put(b, "n/a", "x"); err != nil {
					t.Fatal(err)
				}
			}
			tc.inject(b)
			created, err := put(b, "n/a", "x")
			if created || !errors.Is(err, ErrBecameUnavailable) || !errors.Is(err, errInjected) {
				t.Fatalf("Put = %v, %v", created, err)
			}
			if errors.Is(err, ErrStopped) != tc.stopped {
				t.Fatalf("Put = %v, stopped want %v", err, tc.stopped)
			}
			assertClean(t, b, tc.objects...)
			fresh := New(b.path, 1<<20, 1000) // real syscalls: refused all the same
			b.write, b.syncFile, b.link, b.syncDir = fresh.write, fresh.syncFile, fresh.link, fresh.syncDir
			if _, err := put(b, "n/b", "y"); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBecameUnavailable) || errors.Is(err, ErrStopped) {
				t.Fatalf("later Put = %v", err)
			}
			if _, _, err := b.Get("n/a"); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("later Get = %v", err)
			}
		})
	}
}

// A writer already past its checks when another one fails the bucket is not acknowledged.
func TestPutConcurrentWriterAfterFailure(t *testing.T) {
	b := newBucket(t)
	arrived, release := make(chan struct{}), make(chan struct{})
	b.link = func(oldname, newname string) error {
		close(arrived)
		<-release
		return os.Link(oldname, newname)
	}
	type result struct {
		created bool
		err     error
	}
	done := make(chan result)
	go func() {
		created, err := put(b, "n/b", "y")
		done <- result{created, err}
	}()
	<-arrived
	b.syncFile = func(*os.File) error { return errInjected }
	if _, err := put(b, "n/a", "x"); !errors.Is(err, ErrBecameUnavailable) {
		t.Fatalf("failing Put = %v", err)
	}
	close(release)
	r := <-done
	if r.created || !errors.Is(r.err, ErrUnavailable) || errors.Is(r.err, ErrBecameUnavailable) {
		t.Fatalf("concurrent Put = %v, %v", r.created, r.err)
	}
}
