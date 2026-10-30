package backup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/key"
	"github.com/scorsi/azukari/internal/seal"
)

// copyDir reads a node's objects from a copy of a bucket's directory, a replica for instance
// (ADR 0006, 0008): FORMAT, then objects/<key> as the store lays them out (ADR 0004). A copy
// is untrusted input: every object is checked against its header's size and SHA-256 and
// bounded like a GET, a symlink anywhere below the copy is refused, names starting with "."
// (rsync's temporary files) are ignored, and tmp/ is never read.
type copyDir struct {
	objects int // descriptor of <copy>/objects
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func openCopy(dir string) (*copyDir, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", dir, err)
	}
	defer unix.Close(fd)
	format, err := readAt(fd, "FORMAT", 64)
	if err != nil {
		return nil, fmt.Errorf("copy %s: FORMAT: %w", dir, err)
	}
	if string(format) != bucket.FormatContent {
		return nil, fmt.Errorf("copy %s: FORMAT holds %q, want %q", dir, format, bucket.FormatContent)
	}
	objects, err := unix.Openat(fd, "objects", dirFlags, 0)
	if err != nil {
		return nil, fmt.Errorf("copy %s: objects: %w", dir, refuseLink(err))
	}
	return &copyDir{objects: objects}, nil
}

func (d *copyDir) Close() { unix.Close(d.objects) }

func refuseLink(err error) error {
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("symlink refused: %w", err)
	}
	return err
}

// open opens rel ("" for objects/ itself) one segment at a time, never following a symlink.
func (d *copyDir) open(rel string, dir bool) (*os.File, error) {
	fd, err := unix.Openat(d.objects, ".", dirFlags, 0)
	if err != nil {
		return nil, err
	}
	segs := strings.Split(rel, "/")
	if rel == "" {
		segs = nil
	}
	for i, s := range segs {
		flags := dirFlags
		if i == len(segs)-1 && !dir {
			flags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
		}
		next, err := unix.Openat(fd, s, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, refuseLink(err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// read reads the regular file name in dir, at most max bytes.
func readAt(dir int, name string, max int64) ([]byte, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refuseLink(err)
	}
	return readFile(os.NewFile(uintptr(fd), name), max)
}

func readFile(f *os.File, max int64) ([]byte, error) {
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%v)", fi.Mode().Type())
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%d bytes, over %d", fi.Size(), max)
	}
	b := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Get returns k's body after checking it against its object header (ADR 0004).
func (d *copyDir) Get(_ context.Context, k string, max int64) ([]byte, error) {
	if err := key.Check(k); err != nil {
		return nil, err
	}
	f, err := d.open(k, false)
	if err != nil {
		return nil, err
	}
	b, err := readFile(f, bucket.HeaderSize+max)
	if err != nil {
		return nil, err
	}
	h, err := bucket.DecodeHeader(b, int64(len(b)))
	if err != nil {
		return nil, err
	}
	body := b[bucket.HeaderSize:]
	if sha256.Sum256(body) != h.SHA256 {
		return nil, errors.New("body does not match its header's SHA-256")
	}
	return body, nil
}

// entries lists rel's names in byte order, refusing symlinks and skipping "." names.
func (d *copyDir) entries(rel string) ([]fs.DirEntry, error) {
	f, err := d.open(rel, true)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	all, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, e := range all {
		switch {
		case strings.HasPrefix(e.Name(), "."):
		case e.Type()&fs.ModeSymlink != 0:
			return nil, fmt.Errorf("objects/%s: symlink refused", path.Join(rel, e.Name()))
		default:
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// List yields the objects directly in the directory prefix names; only the snapshots/ listing
// calls it. shortcut: one directory level, not the store's recursive prefix match; extend if
// a caller ever lists a deeper prefix.
func (d *copyDir) List(_ context.Context, prefix string) iter.Seq2[*azukariv1.Object, error] {
	return func(yield func(*azukariv1.Object, error) bool) {
		entries, err := d.entries(strings.TrimSuffix(prefix, "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			yield(nil, err)
			return
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				yield(nil, fmt.Errorf("%s%s: not a regular file", prefix, e.Name()))
				return
			}
			if !yield(&azukariv1.Object{Key: prefix + e.Name()}, nil) {
				return
			}
		}
	}
}

// namespace finds the node's namespace without its SVID: the one whose config holds this
// data key's key check (ADR 0006: a restore needs only the key and a copy of the bucket).
func (d *copyDir) namespace(k *seal.Key) (string, error) {
	entries, err := d.entries("")
	if err != nil {
		return "", err
	}
	var found []string
	errs := []error{errors.New("no namespace of the copy holds this data key's config: wrong data key or wrong copy")}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := d.Get(context.Background(), e.Name()+"/config", maxConfig)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			errs = append(errs, fmt.Errorf("%s/config: %w", e.Name(), err))
		case string(b) == string(k.Config()):
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return "", errors.Join(errs...)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("namespaces %v hold this data key's config: cannot choose", found)
}
