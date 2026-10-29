package backup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	backupv1 "github.com/scorsi/azukari/gen/azukari/backup/v1"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/seal"
)

// objects is where a restore reads the node's objects from: the store's client, or (M5) a
// directory holding a copy of the bucket.
type objects interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// Snapshots writes one line per snapshot of the node to w, in name order:
// "<name> <start> <end> entries=<n> bytes=<n> chunks=<n> new_chunks=<n>", times in RFC 3339
// UTC with nanoseconds. A failure is logged as snapshots-failed.
func Snapshots(ctx context.Context, cfg *config.Backup, w io.Writer, log *slog.Logger) error {
	err := snapshots(ctx, cfg, w)
	if err != nil {
		log.Error("", "event", "snapshots-failed", "err", err)
	}
	return err
}

func snapshots(ctx context.Context, cfg *config.Backup, w io.Writer) error {
	node, k, c, err := connect(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := checkKey(ctx, c, node, k); err != nil {
		return err
	}
	prefix := node + "/snapshots/"
	for o, err := range c.List(ctx, prefix) {
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		name := strings.TrimPrefix(o.Key, prefix)
		root, err := openRoot(ctx, c, node, k, name)
		if err != nil {
			return err
		}
		t := root.Totals
		if _, err := fmt.Fprintf(w, "%s %s %s entries=%d bytes=%d chunks=%d new_chunks=%d\n", name,
			stamp(root.StartTimeNs), stamp(root.EndTimeNs), t.GetEntries(), t.GetBytes(), t.GetChunks(), t.GetNewChunks()); err != nil {
			return err
		}
	}
	return nil
}

func stamp(ns int64) string { return time.Unix(0, ns).UTC().Format(time.RFC3339Nano) }

// checkKey compares the store's config with the data key: a wrong key is named as such.
func checkKey(ctx context.Context, c objects, node string, k *seal.Key) error {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	b, err := c.Get(ctx, node+"/config")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if string(b) != string(k.Config()) {
		return fmt.Errorf("%s/config holds another key check: wrong data key", node)
	}
	return nil
}

// openRoot fetches, opens (aad snapshots/<name>) and decodes a snapshot root.
func openRoot(ctx context.Context, c objects, node string, k *seal.Key, name string) (*backupv1.Root, error) {
	if name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("snapshot name %q is invalid", name)
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	sealed, err := c.Get(ctx, node+"/snapshots/"+name)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", name, err)
	}
	plain, err := k.OpenRoot(name, sealed)
	if err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", name, err)
	}
	root := &backupv1.Root{}
	if err := proto.Unmarshal(plain, root); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", name, err)
	}
	if root.Format != Format {
		return nil, fmt.Errorf("snapshot %s: format %d, want %d", name, root.Format, Format)
	}
	return root, nil
}

// Restore writes snapshot name into target, an empty or absent directory, checking every
// object (tag, identifier, aad) and the root's totals; verify does the same and writes
// nothing. It logs restore-completed or restore-failed (ADR 0009).
func Restore(ctx context.Context, cfg *config.Backup, name, target string, verify bool, log *slog.Logger) error {
	began := time.Now()
	r := &restorer{verify: verify, target: -1, cached: -1, kinds: map[string]bool{},
		chunks: map[[32]byte]struct{}{}, asRoot: os.Geteuid() == 0}
	err := r.restore(ctx, cfg, name, target)
	r.close()
	if err != nil {
		log.Error("", "event", "restore-failed", "snapshot", name, "verify", verify, "err", err)
		return err
	}
	log.Info("", "event", "restore-completed", "bucket", cfg.Bucket, "snapshot", name, "verify", verify,
		"entries", r.entries, "bytes", r.bytes, "chunks", len(r.chunks), "dur_us", time.Since(began).Microseconds())
	return nil
}

type restorer struct {
	ctx    context.Context
	src    objects
	node   string
	key    *seal.Key
	verify bool
	asRoot bool // owners are restored only by root

	target  int // the target directory, -1 under verify
	cached  int // descriptor of cachedP, the parent of the last entry
	cachedP string

	kinds  map[string]bool       // every path restored or made: true for a directory
	dirs   []*backupv1.Entry     // directories, their metadata applied last
	chunks map[[32]byte]struct{} // every chunk read, the metadata stream's included

	entries, bytes uint64
}

func (r *restorer) close() {
	if r.cached >= 0 {
		unix.Close(r.cached)
	}
	if r.target >= 0 {
		unix.Close(r.target)
	}
}

func (r *restorer) restore(ctx context.Context, cfg *config.Backup, name, target string) error {
	node, k, c, err := connect(cfg)
	if err != nil {
		return err
	}
	defer c.Close()
	r.ctx, r.src, r.node, r.key = ctx, c, node, k
	if err := checkKey(ctx, c, node, k); err != nil {
		return err
	}
	root, err := openRoot(ctx, c, node, k, name)
	if err != nil {
		return err
	}
	if !r.verify {
		if r.target, err = openTarget(target); err != nil {
			return err
		}
	}
	tops := map[string]bool{}
	for _, p := range root.Paths {
		tops[strings.TrimPrefix(filepath.Clean(p), "/")] = true
	}

	br := bufio.NewReader(&stream{r: r, ids: root.MetadataChunks})
	for {
		e := &backupv1.Entry{}
		if err := readEntry(br, e); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("metadata stream: %w", err)
		}
		if err := r.entry(e, tops); err != nil {
			return fmt.Errorf("%q: %w", e.Path, err)
		}
	}
	// Deepest first: every directory's contents are done before its mode and mtime are set.
	for i := len(r.dirs) - 1; i >= 0; i-- {
		if err := r.dirMeta(r.dirs[i]); err != nil {
			return fmt.Errorf("%q: %w", r.dirs[i].Path, err)
		}
	}
	t := root.GetTotals()
	if r.entries != t.GetEntries() || r.bytes != t.GetBytes() || uint64(len(r.chunks)) != t.GetChunks() {
		return fmt.Errorf("restored %d entries, %d bytes, %d chunks; the root says %d, %d, %d",
			r.entries, r.bytes, len(r.chunks), t.GetEntries(), t.GetBytes(), t.GetChunks())
	}
	return nil
}

// openTarget creates target or checks it is an empty directory, and opens it.
func openTarget(target string) (int, error) {
	err := os.Mkdir(target, 0o700)
	if errors.Is(err, os.ErrExist) {
		names, rerr := readNames(target)
		if rerr != nil {
			return -1, fmt.Errorf("target: %w", rerr)
		}
		if len(names) > 0 {
			return -1, fmt.Errorf("target %s is not empty", target)
		}
	} else if err != nil {
		return -1, fmt.Errorf("target: %w", err)
	}
	fd, err := unix.Open(target, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("target %s: %w", target, err)
	}
	return fd, nil
}

func readNames(dir string) ([]string, error) {
	if fi, err := os.Lstat(dir); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(1)
}

// entry checks one entry's path and restores it.
func (r *restorer) entry(e *backupv1.Entry, tops map[string]bool) error {
	p := string(e.Path)
	if p == "" || strings.HasPrefix(p, "/") {
		return errors.New("refused: empty or absolute path")
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return errors.New("refused: empty, . or .. segment")
		}
	}
	if _, dup := r.kinds[p]; dup {
		return errors.New("refused: duplicate path")
	}
	parent, name := path.Split(p)
	parent = strings.TrimSuffix(parent, "/")
	if parent != "" && !r.kinds[parent] {
		// Only a configured path may sit under directories the snapshot does not hold.
		if _, seen := r.kinds[parent]; seen || !tops[p] {
			return errors.New("refused: parent is not a restored directory")
		}
		if err := r.mkParents(parent); err != nil {
			return err
		}
	}
	r.entries++
	r.kinds[p] = e.Type == backupv1.Entry_TYPE_DIRECTORY

	var dir int
	if !r.verify {
		var err error
		if dir, err = r.parent(parent); err != nil {
			return err
		}
	}
	switch e.Type {
	case backupv1.Entry_TYPE_DIRECTORY:
		r.dirs = append(r.dirs, e)
		if !r.verify {
			return unix.Mkdirat(dir, name, 0o700)
		}
		return nil
	case backupv1.Entry_TYPE_SYMLINK:
		if r.verify {
			return nil
		}
		if err := unix.Symlinkat(string(e.SymlinkTarget), dir, name); err != nil {
			return err
		}
		if r.asRoot {
			if err := unix.Fchownat(dir, name, int(e.Uid), int(e.Gid), unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
		}
		return setMtime(dir, name, e.MtimeNs)
	case backupv1.Entry_TYPE_FILE:
		return r.file(dir, name, e)
	}
	return fmt.Errorf("type %v unknown", e.Type)
}

// mkParents makes the missing directories above a configured path (0700, as created).
func (r *restorer) mkParents(parent string) error {
	segs := strings.Split(parent, "/")
	for i := range segs {
		p := strings.Join(segs[:i+1], "/")
		if isDir, seen := r.kinds[p]; seen {
			if !isDir {
				return errors.New("refused: parent is not a restored directory")
			}
			continue
		}
		if !r.verify {
			dir, err := r.parent(strings.Join(segs[:i], "/"))
			if err != nil {
				return err
			}
			if err := unix.Mkdirat(dir, segs[i], 0o700); err != nil {
				return err
			}
		}
		r.kinds[p] = true
	}
	return nil
}

// parent opens a directory made by this restore, one segment at a time, never following a
// symlink; the last one stays open for the next entry.
func (r *restorer) parent(p string) (int, error) {
	if p == "" {
		return r.target, nil
	}
	if r.cached >= 0 && r.cachedP == p {
		return r.cached, nil
	}
	if r.cached >= 0 {
		unix.Close(r.cached)
		r.cached = -1
	}
	fd := r.target
	for _, s := range strings.Split(p, "/") {
		next, err := unix.Openat(fd, s, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if fd != r.target {
			unix.Close(fd)
		}
		if err != nil {
			return -1, fmt.Errorf("open %s: %w", p, err)
		}
		fd = next
	}
	r.cached, r.cachedP = fd, p
	return fd, nil
}

// file writes a file from its chunks (O_EXCL, O_NOFOLLOW), then owner, mode and mtime.
func (r *restorer) file(dir int, name string, e *backupv1.Entry) error {
	var f *os.File
	if !r.verify {
		fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err != nil {
			return err
		}
		f = os.NewFile(uintptr(fd), name)
		defer f.Close()
	}
	var n uint64
	for _, id := range e.Chunks {
		p, err := r.chunk(id)
		if err != nil {
			return err
		}
		n += uint64(len(p))
		if f != nil {
			if _, err := f.Write(p); err != nil {
				return err
			}
		}
	}
	if n != e.Size {
		return fmt.Errorf("%d bytes in its chunks, size says %d", n, e.Size)
	}
	r.bytes += n
	if f == nil {
		return nil
	}
	// Owner before mode: chown clears setuid and setgid.
	if r.asRoot {
		if err := f.Chown(int(e.Uid), int(e.Gid)); err != nil {
			return err
		}
	}
	if err := unix.Fchmod(int(f.Fd()), e.Mode&0o7777); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return setMtime(dir, name, e.MtimeNs)
}

// dirMeta sets a restored directory's owner, mode and mtime.
func (r *restorer) dirMeta(e *backupv1.Entry) error {
	if r.verify {
		return nil
	}
	parent, name := path.Split(string(e.Path))
	dir, err := r.parent(strings.TrimSuffix(parent, "/"))
	if err != nil {
		return err
	}
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if r.asRoot {
		if err := unix.Fchown(fd, int(e.Uid), int(e.Gid)); err != nil {
			return err
		}
	}
	if err := unix.Fchmod(fd, e.Mode&0o7777); err != nil {
		return err
	}
	return setMtime(dir, name, e.MtimeNs)
}

// setMtime sets atime and mtime of name in dir, a symlink itself rather than its target.
func setMtime(dir int, name string, ns int64) error {
	t := unix.NsecToTimespec(ns)
	return unix.UtimesNanoAt(dir, name, []unix.Timespec{t, t}, unix.AT_SYMLINK_NOFOLLOW)
}

// chunk fetches and opens one chunk (tag, identifier, aad).
func (r *restorer) chunk(id []byte) ([]byte, error) {
	if len(id) != 32 {
		return nil, fmt.Errorf("chunk identifier of %d bytes", len(id))
	}
	k := r.node + "/" + seal.ChunkPath([32]byte(id))
	ctx, cancel := context.WithTimeout(r.ctx, RequestTimeout)
	defer cancel()
	sealed, err := r.src.Get(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", k, err)
	}
	p, err := r.key.OpenChunk([32]byte(id), sealed)
	if err != nil {
		return nil, fmt.Errorf("chunk %s: %w", k, err)
	}
	r.chunks[[32]byte(id)] = struct{}{}
	return p, nil
}

// stream reads the metadata stream, one chunk at a time.
type stream struct {
	r   *restorer
	ids [][]byte
	buf []byte
}

func (s *stream) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if len(s.ids) == 0 {
			return 0, io.EOF
		}
		b, err := s.r.chunk(s.ids[0])
		if err != nil {
			return 0, err
		}
		s.ids, s.buf = s.ids[1:], b
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}
