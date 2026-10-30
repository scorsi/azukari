package bucket

import (
	"bufio"
	"cmp"
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
	"time"

	"github.com/scorsi/azukari/internal/key"
)

// Pruned counts what a retention run deleted, or would delete in a dry run (ADR 0007).
// Bytes are body bytes (the size in the object headers) of every object counted.
type Pruned struct {
	Snapshots int // roots
	Refs      int // refs lists, of pruned snapshots and of interrupted runs
	Chunks    int
	Bytes     int64
}

// Snapshot is one root of a namespace, as the operator lists them.
type Snapshot struct {
	Name     string
	Received time.Time // the root's receive time: the snapshot's age (ADR 0007)
	Bytes    int64     // body bytes of the root and of its refs list
}

type stored struct {
	size     uint64 // body bytes
	received int64  // ns since the Unix epoch
}

func (o stored) olderThan(now time.Time, d time.Duration) bool {
	return now.Sub(time.Unix(0, o.received)) > d
}

// Prune runs retention over every namespace of the bucket (ADR 0007, "A run"), each under its
// lock: complete snapshots whose root was received more than keep before now, except the
// namespace's newest complete one, then refs without a root older than grace, then chunks no
// remaining refs names and older than grace. A dry run deletes nothing and counts the same.
// On error the counts hold what was deleted before it.
func (b *Bucket) Prune(now time.Time, keep, grace time.Duration, dryRun bool) (Pruned, error) {
	var p Pruned
	if err := b.check(); err != nil {
		return p, err
	}
	entries, err := os.ReadDir(filepath.Join(b.path, "objects"))
	if err != nil {
		return p, err
	}
	for _, e := range entries {
		if !e.IsDir() || checkNamespace(e.Name()) != nil {
			continue
		}
		if err := b.pruneNamespace(e.Name(), now, keep, grace, dryRun, &p); err != nil {
			return p, fmt.Errorf("namespace %s: %w", e.Name(), err)
		}
	}
	return p, nil
}

func (b *Bucket) pruneNamespace(ns string, now time.Time, keep, grace time.Duration, dry bool, p *Pruned) error {
	defer b.LockNamespace(ns)()
	base := filepath.Join(b.path, "objects", ns)
	rootDir, refsDir, chunkDir := filepath.Join(base, "snapshots"), filepath.Join(base, "refs"), filepath.Join(base, "chunks")
	roots, err := scan(rootDir, nil)
	if err != nil {
		return err
	}
	refs, err := scan(refsDir, nil)
	if err != nil {
		return err
	}

	newest := ""
	for name, r := range roots {
		if _, ok := refs[name]; ok && (newest == "" ||
			cmp.Or(cmp.Compare(r.received, roots[newest].received), strings.Compare(name, newest)) > 0) {
			newest = name
		}
	}
	var oldRoots, oldRefs []string
	for name, r := range roots {
		if _, ok := refs[name]; ok && name != newest && r.olderThan(now, keep) {
			oldRoots = append(oldRoots, name)
			oldRefs = append(oldRefs, name)
		}
	}
	for name, r := range refs {
		if _, ok := roots[name]; !ok && r.olderThan(now, grace) {
			oldRefs = append(oldRefs, name)
		}
	}
	// Roots are durably gone before their refs, refs before chunks: a crash leaves only more data.
	if err := b.remove(rootDir, oldRoots, roots, dry, &p.Snapshots, &p.Bytes); err != nil {
		return err
	}
	if err := b.remove(refsDir, oldRefs, refs, dry, &p.Refs, &p.Bytes); err != nil {
		return err
	}
	for _, name := range oldRefs {
		delete(refs, name)
	}

	inUse := map[string]bool{} // "<2 hex>/<64 hex>" under chunks/
	for name := range refs {
		if err := readRefs(filepath.Join(refsDir, filepath.FromSlash(name)), inUse); err != nil {
			return err
		}
	}
	chunks, err := scan(chunkDir, func(rel string) bool { return inUse[rel] })
	if err != nil {
		return err
	}
	var oldChunks []string
	for rel, c := range chunks {
		if c.olderThan(now, grace) {
			oldChunks = append(oldChunks, rel)
		}
	}
	return b.remove(chunkDir, oldChunks, chunks, dry, &p.Chunks, &p.Bytes)
}

// scan reads the header of every regular file under dir, by path relative to dir with "/"
// separators, except those skip accepts (their header is not read). An absent dir, or a file
// where dir should be, holds nothing.
func scan(dir string, skip func(rel string) bool) (map[string]stored, error) {
	objs := map[string]stored{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if p == dir && (errors.Is(err, fs.ErrNotExist) || err == nil && !d.IsDir()) {
			return fs.SkipAll
		}
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel) {
			return nil
		}
		h, err := readHeader(p)
		if err != nil {
			return err
		}
		objs[rel] = stored{h.Size, h.ReceiveTime}
		return nil
	})
	return objs, err
}

// readRefs adds the chunk paths a refs list names (sorted 32-byte ids, ADR 0007) to set.
func readRefs(p string, set map[string]bool) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	h, err := fileHeader(f)
	if err != nil {
		return err
	}
	r := bufio.NewReader(io.LimitReader(f, int64(h.Size)))
	var id [sha256.Size]byte
	for {
		if _, err := io.ReadFull(r, id[:]); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		x := hex.EncodeToString(id[:])
		set[x[:2]+"/"+x] = true
	}
}

// remove deletes dir/<rel> for each rel, counting each into n and its body bytes into bytes,
// then fsyncs every directory it touched. A dry run only counts.
func (b *Bucket) remove(dir string, rels []string, objs map[string]stored, dry bool, n *int, bytes *int64) error {
	touched := map[string]bool{}
	for _, rel := range rels {
		if !dry {
			p := filepath.Join(dir, filepath.FromSlash(rel))
			if err := b.removeFile(p); err != nil {
				return err
			}
			touched[filepath.Dir(p)] = true
		}
		*n++
		*bytes += int64(objs[rel].size)
	}
	for d := range touched {
		if err := b.syncDir(d); err != nil {
			return b.fail(err)
		}
	}
	return nil
}

// removeFile unlinks p if it is a regular file; any other failure makes the bucket unavailable.
func (b *Bucket) removeFile(p string) error {
	fi, err := os.Lstat(p)
	if err != nil && errors.Is(notFound(err), ErrNotFound) || err == nil && !fi.Mode().IsRegular() {
		return nil
	}
	if err == nil {
		err = os.Remove(p)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return b.fail(err)
	}
	return nil
}

// DeleteSnapshot deletes namespace's snapshot name for the operator (ADR 0007): its root,
// durably, then its refs list, under the namespace's lock. Its chunks go at the next run.
// An absent root is ErrNotFound.
func (b *Bucket) DeleteSnapshot(namespace, name string) error {
	if err := b.check(); err != nil {
		return err
	}
	if err := checkNamespace(namespace); err != nil {
		return err
	}
	if err := key.Check(namespace + "/snapshots/" + name); err != nil {
		return err
	}
	defer b.LockNamespace(namespace)()
	root := b.objectPath(namespace + "/snapshots/" + name)
	if _, err := readHeader(root); err != nil {
		return err
	}
	for _, p := range []string{root, b.objectPath(namespace + "/refs/" + name)} {
		if err := b.removeFile(p); err != nil {
			return err
		}
		if err := b.syncDir(filepath.Dir(p)); err != nil && !errors.Is(notFound(err), ErrNotFound) {
			return b.fail(err)
		}
	}
	return nil
}

// Snapshots lists namespace's snapshot roots, oldest first, for the operator (ADR 0007).
func (b *Bucket) Snapshots(namespace string) ([]Snapshot, error) {
	if err := b.check(); err != nil {
		return nil, err
	}
	if err := checkNamespace(namespace); err != nil {
		return nil, err
	}
	base := filepath.Join(b.path, "objects", namespace)
	roots, err := scan(filepath.Join(base, "snapshots"), nil)
	if err != nil {
		return nil, err
	}
	refs, err := scan(filepath.Join(base, "refs"), func(rel string) bool { _, ok := roots[rel]; return !ok })
	if err != nil {
		return nil, err
	}
	list := make([]Snapshot, 0, len(roots))
	for name, r := range roots {
		list = append(list, Snapshot{Name: name, Received: time.Unix(0, r.received), Bytes: int64(r.size + refs[name].size)})
	}
	slices.SortFunc(list, func(x, y Snapshot) int {
		return cmp.Or(x.Received.Compare(y.Received), strings.Compare(x.Name, y.Name))
	})
	return list, nil
}

// checkNamespace refuses anything but one key segment.
func checkNamespace(ns string) error {
	if err := key.Check(ns); err != nil || strings.Contains(ns, "/") {
		return fmt.Errorf("%w: namespace %q", key.ErrInvalid, ns)
	}
	return nil
}
