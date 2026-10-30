package bucket

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/scorsi/azukari/internal/key"
)

// Refusals of the bucket operations; the HTTP layer maps them to ADR 0003's statuses.
// An invalid key wraps key.ErrInvalid (400 key-invalid).
var (
	ErrTooLarge       = errors.New("object too large")         // 413 object-too-large
	ErrDiskReserve    = errors.New("free space below reserve") // 507 disk-reserve
	ErrSHA256Mismatch = errors.New("sha256 mismatch")          // 400 sha256-mismatch
	ErrSizeMismatch   = errors.New("body size mismatch")       // 400 request-invalid
	ErrExists         = errors.New("object exists")            // 409 object-exists
	ErrNotFound       = errors.New("object unknown")           // 404 object-unknown
	ErrUnavailable    = errors.New("bucket unavailable")       // 503 bucket-unavailable
	ErrRefsInvalid    = errors.New("refs invalid")             // 400 refs-invalid
	ErrRefsIncomplete = errors.New("refs incomplete")          // 409 refs-incomplete
	// ErrBecameUnavailable is returned once, by the call whose write or fsync error made the
	// bucket unavailable: the caller logs bucket-unavailable then. It also matches ErrUnavailable.
	ErrBecameUnavailable = fmt.Errorf("%w now", ErrUnavailable)
	// ErrStopped is returned by a Put whose object may or may not be durable: a directory
	// fsync failed once it was visible under objects/ (503 stopped, ADR 0003). The bucket is
	// unavailable from then on; the error also matches ErrBecameUnavailable for the call that
	// failed it.
	ErrStopped = errors.New("writer stopped after an uncertain commit")
)

// Bucket serves the objects of a bucket that Open left Available or Created.
type Bucket struct {
	path      string
	maxObject int64
	reserve   int64
	freeSpace func(dir string) (int64, error)
	space     *account // shared by the buckets on the same filesystem
	now       func() time.Time
	failed    atomic.Pointer[error]
	locks     sync.Map // namespace -> *sync.Mutex
	// The write path's syscalls, replaced by the fault-injection tests.
	write    func(f *os.File, p []byte) (int, error)
	syncFile func(f *os.File) error
	link     func(oldname, newname string) error
	syncDir  func(dir string) error
}

// LockNamespace takes namespace's lock and returns its unlock (ADR 0007): a refs Put holds
// it from its existence check through its link, and pruning holds it for its run.
func (b *Bucket) LockNamespace(namespace string) (unlock func()) {
	m, _ := b.locks.LoadOrStore(namespace, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// account counts the bytes reserved by the uploads in flight on one filesystem, so that
// concurrent uploads cannot together eat into a bucket's disk_reserve.
type account struct {
	mu       sync.Mutex
	reserved int64
}

var accounts sync.Map // filesystem device id -> *account

// accountFor returns the account of path's filesystem.
func accountFor(path string) *account {
	fi, err := os.Stat(path)
	if err != nil {
		return new(account) // Open just succeeded on path; a private account still bounds this bucket
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return new(account)
	}
	a, _ := accounts.LoadOrStore(uint64(st.Dev), new(account))
	return a.(*account)
}

// admit reserves need bytes on the bucket's filesystem if its free space, less the bytes
// already reserved there, keeps the bucket's reserve; the caller defers release.
// shortcut: bytes an upload in flight already wrote count twice (in statfs and in reserved), so
// admission is conservative by up to the in-flight total; track written bytes if that bites.
func (b *Bucket) admit(need int64) (release func(), err error) {
	a := b.space
	a.mu.Lock()
	defer a.mu.Unlock()
	// statfs under the lock: a free value read before another upload released would be stale.
	free, err := b.freeSpace(filepath.Join(b.path, "tmp"))
	if err != nil {
		return nil, b.fail(fmt.Errorf("statfs: %w", err))
	}
	if free-a.reserved-need < b.reserve {
		return nil, fmt.Errorf("%w: %d bytes free, %d in flight, %d announced, reserve %d",
			ErrDiskReserve, free, a.reserved, need, b.reserve)
	}
	a.reserved += need
	return func() {
		a.mu.Lock()
		a.reserved -= need
		a.mu.Unlock()
	}, nil
}

// New returns the bucket at path, opened by Open, with its max_object_bytes and disk_reserve.
func New(path string, maxObjectBytes, diskReserve int64) *Bucket {
	return &Bucket{
		path: path, maxObject: maxObjectBytes, reserve: diskReserve, freeSpace: FreeSpace, now: time.Now,
		space: accountFor(path),
		write: (*os.File).Write, syncFile: (*os.File).Sync, link: os.Link, syncDir: syncDir,
	}
}

// FreeSpace is the space an unprivileged writer may still use on dir's filesystem.
func FreeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Err is nil while the bucket serves, else the write or fsync error that made it
// unavailable until a restart (ADR 0004).
func (b *Bucket) Err() error {
	if p := b.failed.Load(); p != nil {
		return *p
	}
	return nil
}

func (b *Bucket) check() error {
	if err := b.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// fail makes the bucket unavailable for good after a write-side error.
func (b *Bucket) fail(err error) error {
	if b.failed.CompareAndSwap(nil, &err) {
		return fmt.Errorf("%w: %w", ErrBecameUnavailable, err)
	}
	return b.check()
}

// failCommit fails the bucket after an fsync error on an object already visible.
func (b *Bucket) failCommit(err error) error {
	return fmt.Errorf("%w: %w", ErrStopped, b.fail(err))
}

// syncChain fsyncs every directory from dir up to objects/. Another Put may have created one
// of them and not fsynced its entry yet: syncing the whole chain ourselves, rather than
// trusting a directory we found, is the simplest way to acknowledge only a durable path.
func (b *Bucket) syncChain(dir string) error {
	top := filepath.Join(b.path, "objects")
	for ; ; dir = filepath.Dir(dir) {
		if err := b.syncDir(dir); err != nil {
			return err
		}
		if dir == top || len(dir) <= len(top) {
			return nil
		}
	}
}

func (b *Bucket) objectPath(k string) string {
	return filepath.Join(b.path, "objects", filepath.FromSlash(k))
}

// errWriter remembers the file's write error, to tell it from the body's read error.
type errWriter struct {
	write func([]byte) (int, error)
	err   error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// Put creates key from body, which must be size bytes hashing to sum (ADR 0004's write
// path). created is true for a new object (201) and false when the key already holds the
// same bytes (200); other bytes give ErrExists (409). A refused or cut body leaves nothing
// under objects/, and an error reading body is returned as is. A <node>/refs/... key must
// hold sorted, distinct chunk ids naming chunks present (ErrRefsInvalid, ErrRefsIncomplete).
func (b *Bucket) Put(k string, size int64, sum [32]byte, body io.Reader) (created bool, err error) {
	if err := b.check(); err != nil {
		return false, err
	}
	if err := key.Check(k); err != nil {
		return false, err
	}
	if size < 0 || size > b.maxObject {
		return false, fmt.Errorf("%w: %d bytes, max %d", ErrTooLarge, size, b.maxObject)
	}
	release, err := b.admit(size + HeaderSize)
	if err != nil {
		return false, err
	}
	defer release() // runs last: after the temporary file is removed

	f, err := os.CreateTemp(filepath.Join(b.path, "tmp"), "put-")
	if err != nil {
		return false, b.fail(err)
	}
	defer os.Remove(f.Name()) // after a success too: objects/<key> is a second link
	defer f.Close()
	fw := &errWriter{write: func(p []byte) (int, error) { return b.write(f, p) }}
	h := Header{Size: uint64(size), SHA256: sum, ReceiveTime: b.now().UnixNano()}
	if _, err := fw.Write(h.Encode()); err != nil {
		return false, b.fail(err)
	}
	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(fw, hasher), io.LimitReader(body, size+1))
	switch {
	case fw.err != nil:
		return false, b.fail(fw.err)
	case err != nil:
		return false, err
	case n != size:
		return false, fmt.Errorf("%w: %d bytes, announced %d", ErrSizeMismatch, n, size)
	case !bytes.Equal(hasher.Sum(nil), sum[:]):
		return false, ErrSHA256Mismatch
	}
	if err := b.syncFile(f); err != nil {
		return false, b.fail(err)
	}
	if err := f.Close(); err != nil {
		return false, b.fail(err)
	}

	if ns := key.Namespace(k); strings.HasPrefix(k, ns+"/refs/") {
		defer b.LockNamespace(ns)()
		if err := b.checkRefs(ns, f.Name(), size); err != nil {
			return false, err
		}
	}

	dst := b.objectPath(k)
	parent := filepath.Dir(dst)
	// The new directories become durable through syncChain below.
	if err := os.MkdirAll(parent, 0o700); errors.Is(err, syscall.ENOTDIR) {
		// shortcut: a key whose prefix is another object ("a" then "a/b") answers 409; ADR 0003 names no reason for it.
		return false, fmt.Errorf("%w: a prefix of %s is an object", ErrExists, k)
	} else if err != nil {
		return false, b.fail(err)
	}
	switch err := b.link(f.Name(), dst); {
	case err == nil:
		if err := b.syncChain(parent); err != nil {
			return false, b.failCommit(err)
		}
		// A concurrent call failed the bucket meanwhile: never acknowledge after that.
		if err := b.check(); err != nil {
			return false, err
		}
		return true, nil
	case !errors.Is(err, fs.ErrExist):
		return false, b.fail(err)
	}
	old, err := readHeader(dst)
	if errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("%w: %s is a prefix of other keys", ErrExists, k)
	} else if err != nil {
		return false, err
	}
	if old.Size != uint64(size) || old.SHA256 != sum {
		return false, ErrExists
	}
	// A concurrent Put may have linked it without its fsyncs yet: answer 200 only once durable.
	if err := b.syncChain(parent); err != nil {
		return false, b.failCommit(err)
	}
	return false, b.check()
}

// checkRefs reads the refs body written at tmp (ADR 0007): size/32 chunk ids in strictly
// increasing order, else ErrRefsInvalid; each <ns>/chunks/<2 hex>/<64 hex> present, else
// ErrRefsIncomplete. The caller holds namespace's lock.
func (b *Bucket) checkRefs(ns, tmp string, size int64) error {
	if size%sha256.Size != 0 {
		return fmt.Errorf("%w: %d bytes is not a multiple of 32", ErrRefsInvalid, size)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(io.NewSectionReader(f, HeaderSize, size))
	var prev, id [sha256.Size]byte
	var incomplete error // reported only once the whole list is known valid
	for i := int64(0); i < size/sha256.Size; i++ {
		if _, err := io.ReadFull(r, id[:]); err != nil {
			return err
		}
		if i > 0 && bytes.Compare(prev[:], id[:]) >= 0 {
			return fmt.Errorf("%w: id %d is not above the previous one", ErrRefsInvalid, i)
		}
		prev = id
		if incomplete != nil {
			continue
		}
		h := hex.EncodeToString(id[:])
		fi, err := os.Stat(b.objectPath(ns + "/chunks/" + h[:2] + "/" + h))
		if err != nil && !errors.Is(notFound(err), ErrNotFound) {
			return err
		}
		if err != nil || !fi.Mode().IsRegular() {
			incomplete = fmt.Errorf("%w: chunk %s absent", ErrRefsIncomplete, h)
		}
	}
	return incomplete
}

// readHeader reads an object's header; a missing key, or a directory, is ErrNotFound.
func readHeader(p string) (Header, error) {
	f, err := os.Open(p)
	if err != nil {
		return Header{}, notFound(err)
	}
	defer f.Close()
	return fileHeader(f)
}

func fileHeader(f *os.File) (Header, error) {
	fi, err := f.Stat()
	if err != nil {
		return Header{}, err
	}
	if !fi.Mode().IsRegular() {
		return Header{}, ErrNotFound
	}
	buf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Header{}, err
	}
	h, err := DecodeHeader(buf, fi.Size())
	if err != nil {
		return Header{}, fmt.Errorf("%s: %w", f.Name(), err)
	}
	return h, nil
}

func notFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return ErrNotFound
	}
	return err
}

// Get returns key's header and its body, positioned after the header; the caller closes it.
func (b *Bucket) Get(k string) (Header, io.ReadCloser, error) {
	if err := b.check(); err != nil {
		return Header{}, nil, err
	}
	if err := key.Check(k); err != nil {
		return Header{}, nil, err
	}
	f, err := os.Open(b.objectPath(k))
	if err != nil {
		return Header{}, nil, notFound(err)
	}
	h, err := fileHeader(f)
	if err != nil {
		f.Close()
		return Header{}, nil, err
	}
	return h, f, nil
}

// Missing returns, in order, the keys that hold no object (ADR 0003's missing).
func (b *Bucket) Missing(keys []string) ([]string, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	var missing []string
	for _, k := range keys {
		if err := key.Check(k); err != nil {
			return nil, err
		}
		fi, err := os.Stat(b.objectPath(k))
		if err != nil && !errors.Is(notFound(err), ErrNotFound) {
			return nil, err
		}
		if err != nil || !fi.Mode().IsRegular() {
			missing = append(missing, k)
		}
	}
	return missing, nil
}

// Object is one entry of List.
type Object struct {
	Key    string
	Size   uint64
	SHA256 [32]byte
}

var errListFull = errors.New("list full")

// List returns up to limit objects whose key starts with prefix and sorts after
// startAfter, in key order, and next: the start_after of the following page, empty on the
// last one (ADR 0003).
func (b *Bucket) List(prefix, startAfter string, limit int) (objs []Object, next string, err error) {
	if err := b.check(); err != nil {
		return nil, "", err
	}
	if limit < 1 {
		return nil, "", fmt.Errorf("list limit %d must be positive", limit)
	}
	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		// Key order: a directory's keys sort as its name followed by "/".
		sortName := func(e fs.DirEntry) string {
			if e.IsDir() {
				return e.Name() + "/"
			}
			return e.Name()
		}
		slices.SortFunc(entries, func(x, y fs.DirEntry) int { return strings.Compare(sortName(x), sortName(y)) })
		for _, e := range entries {
			k := rel + sortName(e)
			if e.IsDir() {
				// Skip a subtree that cannot match the prefix or lies wholly before startAfter.
				if !strings.HasPrefix(k, prefix) && !strings.HasPrefix(prefix, k) ||
					k <= startAfter && !strings.HasPrefix(startAfter, k) {
					continue
				}
				if err := walk(filepath.Join(dir, e.Name()), k); err != nil {
					return err
				}
				continue
			}
			if !e.Type().IsRegular() || k <= startAfter || !strings.HasPrefix(k, prefix) {
				continue
			}
			h, err := readHeader(filepath.Join(dir, e.Name()))
			if err != nil {
				return err
			}
			objs = append(objs, Object{Key: k, Size: h.Size, SHA256: h.SHA256})
			if len(objs) > limit {
				return errListFull
			}
		}
		return nil
	}
	if err := walk(filepath.Join(b.path, "objects"), ""); err != nil && err != errListFull {
		return nil, "", err
	}
	if len(objs) > limit {
		objs = objs[:limit]
		next = objs[limit-1].Key
	}
	return objs, next, nil
}

// LastSnapshot is the receive time of namespace's newest snapshots/* object, false when it
// has none: read once at start for azukari_last_snapshot_timestamp_seconds (ADR 0009).
func (b *Bucket) LastSnapshot(namespace string) (time.Time, bool, error) {
	if err := checkNamespace(namespace); err != nil {
		return time.Time{}, false, err
	}
	dir := filepath.Join(b.path, "objects", namespace, "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(notFound(err), ErrNotFound) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	var newest int64
	found := false
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		h, err := readHeader(filepath.Join(dir, e.Name()))
		if err != nil {
			return time.Time{}, false, err
		}
		if !found || h.ReceiveTime > newest {
			newest, found = h.ReceiveTime, true
		}
	}
	if !found {
		return time.Time{}, false, nil
	}
	return time.Unix(0, newest), true, nil
}
