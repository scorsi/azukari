// Package backup is `azukari-backup run` (ADR 0006, 0007, 0030): it walks one export's paths,
// chunks and seals every file and the metadata stream, uploads the chunks the store lacks,
// then writes the snapshot's refs and root.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	backupv1 "github.com/scorsi/azukari/gen/azukari/backup/v1"
	"github.com/scorsi/azukari/internal/client"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/fastcdc"
	"github.com/scorsi/azukari/internal/key"
	"github.com/scorsi/azukari/internal/seal"
	"github.com/scorsi/azukari/internal/svid"
)

// Format is Root.format (ADR 0006): 2 since one snapshot per export (ADR 0030).
const Format = 2

// RefsRetries bounds the retries of a refs PUT answered refs-incomplete (ADR 0007).
const RefsRetries = 3

// RootAttempts bounds the root PUTs of a run, each refused refs-incomplete one followed by
// writing the refs again (ADR 0025).
const RootAttempts = 3

// Bounds of a run. Variables so tests can shrink them.
var (
	// RequestTimeout bounds each request, its client retries included.
	RequestTimeout = 2 * time.Minute
	// Parallel is the most chunk uploads in flight.
	Parallel = 8
	// BatchBytes is the sealed bytes held before asking missing and uploading: the memory a
	// run keeps besides the set of chunk identifiers.
	BatchBytes = 64 << 20
)

// beforeRefs runs between the last upload and the refs PUT (test hook: prune a chunk there).
var beforeRefs = func() {}

// beforeRoot runs before each root PUT (test hook: prune the refs there).
var beforeRoot = func() {}

// fileReader is what a regular file is chunked from (test hook: fail a read midway).
var fileReader = func(f *os.File) io.Reader { return f }

// beforeRead runs before a regular file is opened or a symlink read (test hook: remove it there).
var beforeRead = func(path string) {}

// Run backs up the paths of cfg's export into a new snapshot of that export (ADR 0030),
// logs backup-completed or backup-failed (ADR 0009) and returns the snapshot's name.
func Run(ctx context.Context, cfg *config.Backup, export string, log *slog.Logger) (string, error) {
	r := &run{log: log, seen: map[[32]byte]struct{}{}, totals: &backupv1.Totals{}, began: time.Now()}
	name, err := r.run(ctx, cfg, export)
	if err != nil {
		log.Error("", "event", "backup-failed", "export", export, "err", err)
		return "", err
	}
	log.Info("", "event", "backup-completed", "bucket", cfg.Bucket, "export", export, "snapshot", name,
		"entries", r.totals.Entries, "bytes_read", r.totals.Bytes, "bytes_new", r.bytesNew,
		"chunks_new", r.totals.NewChunks, "dur_us", time.Since(r.began).Microseconds())
	return name, nil
}

type sealedChunk struct {
	id     [32]byte
	sealed []byte
}

type run struct {
	log   *slog.Logger
	began time.Time
	key   *seal.Key
	c     *client.Client
	node  string
	cdc   *fastcdc.Chunker

	seen         map[[32]byte]struct{} // every chunk the snapshot references: its refs
	pending      []sealedChunk         // new to this run, not yet checked against the store
	pendingBytes int

	meta  *os.File // the metadata stream, unlinked
	metaW *bufio.Writer

	mu       sync.Mutex // guards the counters below during parallel uploads
	totals   *backupv1.Totals
	bytesNew uint64
}

// skipError is a path of a kind ADR 0006 does not save: logged as file-skipped, the run goes
// on. A file or directory that cannot be read is not one: the run fails, since a snapshot
// that silently lacks it (or holds its chunks without its entry) would not restore as saved.
type skipError struct{ error }

// vanished turns an lstat, open or readlink error of a path removed since its directory was listed
// (ENOENT, or ENOTDIR from a parent replaced meanwhile) into a skipError: nothing of it was
// read yet, so the snapshot lacks it entirely, as if the run had started later.
func vanished[T any](v T, err error) (T, error) {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) {
		err = skipError{err}
	}
	return v, err
}

func (r *run) run(ctx context.Context, cfg *config.Backup, export string) (string, error) {
	exp, ok := cfg.Exports[export]
	if !ok {
		return "", fmt.Errorf("export %q is not in the configuration", export)
	}
	node, k, c, err := connect(cfg)
	if err != nil {
		return "", err
	}
	r.node, r.key, r.c = node, k, c
	defer r.c.Close()
	r.cdc = fastcdc.New(nil, r.key.GearTable())

	var rnd [4]byte
	rand.Read(rnd[:])
	name := r.began.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(rnd[:]) // ADR 0006, 0015
	// The config check bounds the export name for a 1-byte namespace only (ADR 0003): refuse
	// keys over the limit before uploading anything.
	for _, k := range []string{node + "/snapshots/" + export + "/" + name, node + "/refs/" + export + "/" + name} {
		if err := key.Check(k); err != nil {
			return "", fmt.Errorf("export %q: %w", export, err)
		}
	}

	// A different key check there means another data key: refuse before any upload.
	if _, err := r.put(ctx, node+"/config", r.key.Config()); err != nil {
		if isReason(err, http.StatusConflict, "object-exists") {
			return "", fmt.Errorf("%s/config holds another key check: wrong data key", node)
		}
		return "", fmt.Errorf("config: %w", err)
	}

	if r.meta, err = os.CreateTemp("", "azukari-backup-*"); err != nil {
		return "", err
	}
	os.Remove(r.meta.Name()) // nothing left behind, even after a crash
	defer r.meta.Close()
	r.metaW = bufio.NewWriter(r.meta)

	for _, p := range slices.Sorted(slices.Values(exp.Paths)) {
		if err := filepath.WalkDir(p, r.visit(ctx, p)); err != nil {
			return "", err
		}
	}
	if err := r.metaW.Flush(); err != nil {
		return "", fmt.Errorf("metadata stream: %w", err)
	}
	if _, err := r.meta.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var metaChunks [][]byte
	if _, err := r.chunk(ctx, r.meta, func(id [32]byte) { metaChunks = append(metaChunks, id[:]) }); err != nil {
		return "", fmt.Errorf("metadata stream: %w", err)
	}
	if err := r.flush(ctx); err != nil {
		return "", err
	}

	ids := slices.SortedFunc(maps.Keys(r.seen), func(a, b [32]byte) int { return bytes.Compare(a[:], b[:]) })
	refs := make([]byte, 0, 32*len(ids))
	for _, id := range ids {
		refs = append(refs, id[:]...)
	}
	beforeRefs()
	if err := r.putRefs(ctx, node+"/refs/"+export+"/"+name, refs, ids); err != nil {
		return "", err
	}

	r.totals.Chunks = uint64(len(ids))
	root, err := proto.Marshal(&backupv1.Root{
		Format: Format, StartTimeNs: r.began.UnixNano(), EndTimeNs: time.Now().UnixNano(),
		Paths: exp.Paths, MetadataChunks: metaChunks, Totals: r.totals,
	})
	if err != nil {
		return "", err
	}
	// Sealed once with a random nonce: the client's retries send these same bytes.
	sealed := r.key.SealRoot(export, name, root)
	if int64(len(sealed)) > maxRoot {
		return "", fmt.Errorf("root: %d sealed bytes, over the %d a restore reads", len(sealed), maxRoot)
	}
	for attempt := 1; ; attempt++ {
		beforeRoot()
		_, err := r.put(ctx, node+"/snapshots/"+export+"/"+name, sealed)
		if err == nil {
			return name, nil
		}
		if !isReason(err, http.StatusConflict, "refs-incomplete") || attempt == RootAttempts {
			return "", fmt.Errorf("root: %w", err)
		}
		// Pruned while the run paused (ADR 0025): chunks back, then the refs again.
		if err := r.recover(ctx, ids); err != nil {
			return "", err
		}
		if err := r.putRefs(ctx, node+"/refs/"+export+"/"+name, refs, ids); err != nil {
			return "", err
		}
	}
}

// putRefs writes the refs list k, uploading again the chunks pruned meanwhile on each
// refs-incomplete answer, at most RefsRetries times (ADR 0007).
func (r *run) putRefs(ctx context.Context, k string, refs []byte, ids [][32]byte) error {
	for attempt := 0; ; attempt++ {
		_, err := r.put(ctx, k, refs)
		if err == nil {
			return nil
		}
		if !isReason(err, http.StatusConflict, "refs-incomplete") || attempt == RefsRetries {
			return fmt.Errorf("refs: %w", err)
		}
		if err := r.recover(ctx, ids); err != nil {
			return err
		}
	}
}

// visit saves one path of the walk under root. Any error but a skipError under root stops
// the run: a directory that cannot be listed, a file that cannot be read, the store.
func (r *run) visit(ctx context.Context, root string) fs.WalkDirFunc {
	return func(path string, d fs.DirEntry, err error) error {
		var info fs.FileInfo
		if err == nil {
			info, err = vanished(d.Info())
		}
		if err == nil {
			err = r.save(ctx, path, info)
		}
		var skip skipError
		if errors.As(err, &skip) && path != root {
			r.log.Warn("", "event", "file-skipped", "path", path, "err", skip.Error())
			return nil
		}
		return err
	}
}

// save writes the entry of path (lstat info) to the metadata stream, chunking a file's content.
func (r *run) save(ctx context.Context, path string, info fs.FileInfo) error {
	e := &backupv1.Entry{Path: []byte(strings.TrimPrefix(path, "/"))}
	switch info.Mode().Type() {
	case 0:
		e.Type = backupv1.Entry_TYPE_FILE
		if err := r.file(ctx, path, e); err != nil {
			return err
		}
	case fs.ModeDir:
		e.Type = backupv1.Entry_TYPE_DIRECTORY
		setMeta(e, info)
	case fs.ModeSymlink:
		e.Type = backupv1.Entry_TYPE_SYMLINK
		setMeta(e, info)
		beforeRead(path)
		target, err := vanished(os.Readlink(path))
		if err != nil {
			return err
		}
		e.SymlinkTarget = []byte(target)
	default:
		return skipError{fmt.Errorf("type %v not saved", info.Mode().Type())}
	}
	if e.Type != backupv1.Entry_TYPE_SYMLINK && hasXattrs(path) {
		r.log.Warn("", "event", "file-skipped", "path", path, "err", "extended attributes and ACLs not saved")
	}
	if _, err := protodelim.MarshalTo(r.metaW, e); err != nil {
		return fmt.Errorf("metadata stream: %w", err)
	}
	r.totals.Entries++
	return nil
}

// setMeta copies mode (permissions, setuid, setgid, sticky), owner, group and mtime.
func setMeta(e *backupv1.Entry, info fs.FileInfo) {
	e.MtimeNs = info.ModTime().UnixNano()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.Mode, e.Uid, e.Gid = uint32(st.Mode)&0o7777, st.Uid, st.Gid
	}
}

// file chunks a regular file as read; a size or mtime that moved meanwhile is file-changed.
func (r *run) file(ctx context.Context, path string, e *backupv1.Entry) error {
	// O_NOFOLLOW, O_NONBLOCK: a file swapped for a symlink or a FIFO since the walk saw it.
	beforeRead(path)
	f, err := vanished(os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0))
	if err != nil {
		return err // *PathError: names the path
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return skipError{errors.New("no longer a regular file")}
	}
	setMeta(e, before)
	if e.Size, err = r.chunk(ctx, fileReader(f), func(id [32]byte) { e.Chunks = append(e.Chunks, id[:]) }); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	r.totals.Bytes += e.Size
	after, err := f.Stat()
	if err != nil || after.Size() != before.Size() || int64(e.Size) != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		r.log.Warn("", "event", "file-changed", "path", path)
	}
	return nil
}

// chunk splits rd and adds each chunk, calling fn with its identifier in order; it returns
// the bytes read. A read error fails the run like a store error: its chunks so far are
// already in refs.
func (r *run) chunk(ctx context.Context, rd io.Reader, fn func([32]byte)) (uint64, error) {
	r.cdc.Reset(rd)
	var n uint64
	for {
		p, err := r.cdc.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("read: %w", err)
		}
		n += uint64(len(p))
		id, err := r.add(ctx, p)
		if err != nil {
			return n, err
		}
		fn(id)
	}
}

// add records a chunk for refs and, the first time this run meets it, seals it into the batch.
func (r *run) add(ctx context.Context, p []byte) ([32]byte, error) {
	id := r.key.ChunkID(p)
	if _, ok := r.seen[id]; ok {
		return id, nil
	}
	r.seen[id] = struct{}{}
	_, sealed := r.key.SealChunk(p)
	r.pending = append(r.pending, sealedChunk{id, sealed})
	r.pendingBytes += len(sealed)
	if r.pendingBytes >= BatchBytes || len(r.pending) >= client.MissingBatch {
		return id, r.flush(ctx)
	}
	return id, nil
}

// flush asks the store which pending chunks it lacks and uploads those.
func (r *run) flush(ctx context.Context) error {
	keys := make([]string, len(r.pending))
	for i, c := range r.pending {
		keys[i] = r.chunkKey(c.id)
	}
	missing, err := r.missing(ctx, keys)
	if err != nil {
		return err
	}
	var up []sealedChunk
	for i, c := range r.pending {
		if missing[keys[i]] {
			up = append(up, c)
		}
	}
	r.pending, r.pendingBytes = nil, 0
	return r.upload(ctx, up)
}

// upload puts chunks, Parallel at a time; the first error cancels the others.
func (r *run) upload(ctx context.Context, chunks []sealedChunk) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		first error
		sem   = make(chan struct{}, Parallel)
	)
	for _, c := range chunks {
		sem <- struct{}{}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			_, err := r.put(ctx, r.chunkKey(c.id), c.sealed)
			r.mu.Lock()
			defer r.mu.Unlock()
			switch {
			case err == nil:
				r.totals.NewChunks++
				r.bytesNew += uint64(len(c.sealed))
			case first == nil:
				first = fmt.Errorf("chunk: %w", err)
				cancel()
			}
		})
	}
	wg.Wait()
	return first
}

// recover uploads again the chunks of ids the store no longer holds (pruned since missing
// said they were there, ADR 0007): the metadata stream is chunked again, then every file
// whose entry names a lost chunk. A file that changed since cannot give its chunk back.
func (r *run) recover(ctx context.Context, ids [][32]byte) error {
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = r.chunkKey(id)
	}
	missing, err := r.missing(ctx, keys)
	if err != nil {
		return err
	}
	lost := map[[32]byte]bool{}
	for i, k := range keys {
		if missing[k] {
			lost[ids[i]] = true
		}
	}
	reupload := func(rd io.Reader) error {
		r.cdc.Reset(rd)
		for len(lost) > 0 {
			p, err := r.cdc.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return skipError{err}
			}
			if id := r.key.ChunkID(p); lost[id] {
				delete(lost, id)
				_, sealed := r.key.SealChunk(p)
				if err := r.upload(ctx, []sealedChunk{{id, sealed}}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if _, err := r.meta.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := reupload(r.meta); err != nil {
		return fmt.Errorf("metadata stream: %w", err)
	}
	if _, err := r.meta.Seek(0, io.SeekStart); err != nil {
		return err
	}
	br := bufio.NewReader(r.meta)
	for len(lost) > 0 {
		e := &backupv1.Entry{}
		if err := readEntry(br, e); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("metadata stream: %w", err)
		}
		if !slices.ContainsFunc(e.Chunks, func(c []byte) bool { return lost[[32]byte(c)] }) {
			continue
		}
		f, err := os.OpenFile("/"+string(e.Path), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		err = reupload(f)
		f.Close()
		if _, skip := err.(skipError); err != nil && !skip {
			return err
		}
	}
	if len(lost) > 0 {
		return fmt.Errorf("refs: the store lost %d chunks this run cannot read again (files changed since)", len(lost))
	}
	return nil
}

// connect reads the node's namespace from its SVID and its data key, and opens a client.
func connect(cfg *config.Backup) (node string, k *seal.Key, c *client.Client, err error) {
	src, err := svid.Load(cfg.TLS.TLS)
	if err != nil {
		return "", nil, nil, err
	}
	if _, node, err = src.ID(); err != nil {
		return "", nil, nil, err
	}
	if err := key.Check(node); err != nil {
		return "", nil, nil, fmt.Errorf("own SVID host %q is not a namespace: %w", node, err)
	}
	if k, err = seal.LoadKey(cfg.DataKeyFile); err != nil {
		return "", nil, nil, fmt.Errorf("data_key_file: %w", err)
	}
	return node, k, client.New(cfg, src), nil
}

func (r *run) chunkKey(id [32]byte) string { return r.node + "/" + seal.ChunkPath(id) }

func (r *run) put(ctx context.Context, k string, body []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	return r.c.Put(ctx, k, body)
}

// missing returns the set of keys the store lacks, one bounded request per batch.
func (r *run) missing(ctx context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for batch := range slices.Chunk(keys, client.MissingBatch) {
		ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
		m, err := r.c.Missing(ctx, batch)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("missing: %w", err)
		}
		for _, k := range m {
			out[k] = true
		}
	}
	return out, nil
}

func isReason(err error, status int, reason string) bool {
	var se *client.Error
	return errors.As(err, &se) && se.Status == status && se.Reason == reason
}

// readEntry reads one metadata stream record, run's recovery and restore alike. No size bound:
// an Entry lists every chunk of its file (34 bytes each), so protodelim's 4 MiB default would
// refuse files over ~120k chunks; the stream is the node's own (run) or authenticated (restore).
func readEntry(r protodelim.Reader, e *backupv1.Entry) error {
	return protodelim.UnmarshalOptions{MaxSize: -1}.UnmarshalFrom(r, e)
}
