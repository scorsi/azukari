package backup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	backupv1 "github.com/scorsi/azukari/gen/azukari/backup/v1"
	"github.com/scorsi/azukari/internal/client"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/seal"
	"github.com/scorsi/azukari/internal/server"
	"github.com/scorsi/azukari/internal/svid"
	"github.com/scorsi/azukari/internal/testca"
)

const (
	serverID = "spiffe://example.org/store-host/azukari-server"
	nodeA    = "spiffe://example.org/node-a/azukari-backup"
	keyA     = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	keyB     = "ff0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) count(sub string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.b.String(), sub)
}

type env struct {
	cfg    *config.Backup
	bucket string // the bucket's directory
	tree   string // the backed-up path
	logs   *syncBuffer
	c      *client.Client
	key    *seal.Key
}

// start serves the bucket "backup" on loopback with a real internal/server and builds a
// backup configuration for node-a over a fresh tree.
func start(t *testing.T) *env {
	t.Helper()
	b := client.Backoff
	client.Backoff = time.Millisecond
	t.Cleanup(func() { client.Backoff = b })
	ca := testca.New(t)
	dir := t.TempDir()
	cfg := &config.Server{Buckets: []config.Bucket{{Name: "backup", Type: "backup", Path: filepath.Join(dir, "b"),
		MountPoint: dir, MaxObjectBytes: 8 << 20, DiskReserve: "0B", Nodes: []string{nodeA}}}}
	src, err := svid.Load(ca.SVID(t, serverID))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	s, err := server.New(cfg, src, logging.New(logs, slog.LevelInfo), func(string) (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, []net.Listener{ln}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	for logs.count(`"server-started"`) == 0 {
		time.Sleep(time.Millisecond)
	}

	e := &env{bucket: filepath.Join(dir, "b"), tree: filepath.Join(t.TempDir(), "a"), logs: logs}
	files := ca.SVID(t, nodeA)
	e.cfg = &config.Backup{Server: "https://" + ln.Addr().String(), Bucket: "backup",
		DataKeyFile: writeKey(t, keyA), Exports: map[string]config.Export{"data": {Paths: []string{e.tree}}}, TLS: config.BackupTLS{TLS: files, ServerID: serverID}}
	nsrc, err := svid.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	e.c = client.New(e.cfg, nsrc)
	t.Cleanup(e.c.Close)
	if e.key, err = seal.ParseKey([]byte(keyA)); err != nil {
		t.Fatal(err)
	}
	return e
}

func writeKey(t *testing.T, k string) string {
	p := filepath.Join(t.TempDir(), "data.key")
	testca.Write(t, p, []byte(k+"\n"))
	return p
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

// tree holds an empty file, a random file of several chunks, text, nested directories, a
// symlink and a FIFO (skipped). It returns the large file's content.
func (e *env) makeTree(t *testing.T) []byte {
	big := make([]byte, 10<<20)
	rand.NewChaCha8([32]byte{1}).Read(big)
	write(t, filepath.Join(e.tree, "big"), big)
	write(t, filepath.Join(e.tree, "empty"), nil)
	write(t, filepath.Join(e.tree, "text.txt"), []byte("hello, azukari\n"))
	write(t, filepath.Join(e.tree, "d1/d2/nested.txt"), []byte("nested\n"))
	if err := os.Symlink("text.txt", filepath.Join(e.tree, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(e.tree, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	return big
}

func (e *env) run(t *testing.T) string {
	t.Helper()
	name, err := Run(context.Background(), e.cfg, "data", logging.New(e.logs, slog.LevelInfo))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, e.logs.b.String())
	}
	return name
}

// list returns every key under node-a/.
func (e *env) list(t *testing.T) []string {
	t.Helper()
	var keys []string
	for o, err := range e.c.List(context.Background(), "node-a/") {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, o.Key)
	}
	return keys
}

func (e *env) get(t *testing.T, k string) []byte {
	t.Helper()
	b, err := e.c.Get(context.Background(), k, 1<<30)
	if err != nil {
		t.Fatalf("get %s: %v", k, err)
	}
	return b
}

func (e *env) chunk(t *testing.T, id []byte) []byte {
	t.Helper()
	p, err := e.key.OpenChunk([32]byte(id), e.get(t, "node-a/"+seal.ChunkPath([32]byte(id))))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// snapshot opens the root of name and decodes its metadata stream.
func (e *env) snapshot(t *testing.T, name string) (*backupv1.Root, []*backupv1.Entry) {
	t.Helper()
	plain, err := e.key.OpenRoot("data", name, e.get(t, "node-a/snapshots/data/"+name))
	if err != nil {
		t.Fatal(err)
	}
	root := &backupv1.Root{}
	if err := proto.Unmarshal(plain, root); err != nil {
		t.Fatal(err)
	}
	var stream []byte
	for _, id := range root.MetadataChunks {
		stream = append(stream, e.chunk(t, id)...)
	}
	var entries []*backupv1.Entry
	br := bufio.NewReader(bytes.NewReader(stream))
	for {
		en := &backupv1.Entry{}
		if err := protodelim.UnmarshalFrom(br, en); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, en)
	}
	return root, entries
}

// refsSet checks refs/<name> is sorted, unique, and returns its chunk keys.
func (e *env) refsSet(t *testing.T, name string) []string {
	t.Helper()
	refs := e.get(t, "node-a/refs/data/"+name)
	var keys []string
	for i := 0; i < len(refs); i += 32 {
		if i > 0 && bytes.Compare(refs[i-32:i], refs[i:i+32]) >= 0 {
			t.Fatalf("refs not sorted and unique at %d", i/32)
		}
		keys = append(keys, "node-a/"+seal.ChunkPath([32]byte(refs[i:i+32])))
	}
	return keys
}

func TestRun(t *testing.T) {
	e := start(t)
	big := e.makeTree(t)
	name := e.run(t)
	if !regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`).MatchString(name) {
		t.Errorf("snapshot name %q, want yyyymmddThhmmssZ-<8 hex> (ADR 0006)", name)
	}

	keys := e.list(t)
	var chunks []string
	for _, k := range keys {
		if strings.HasPrefix(k, "node-a/chunks/") {
			chunks = append(chunks, k)
		}
	}
	for _, k := range []string{"node-a/config", "node-a/refs/data/" + name, "node-a/snapshots/data/" + name} {
		if !slices.Contains(keys, k) {
			t.Errorf("%s missing from %v", k, keys)
		}
	}
	if !bytes.Equal(e.get(t, "node-a/config"), e.key.Config()) {
		t.Error("config holds other bytes")
	}
	if refs := e.refsSet(t, name); !slices.Equal(refs, chunks) {
		t.Errorf("refs = %v, chunks stored = %v", refs, chunks)
	}

	root, entries := e.snapshot(t, name)
	rel := strings.TrimPrefix(e.tree, "/")
	var paths []string
	byPath := map[string]*backupv1.Entry{}
	for _, en := range entries {
		p := strings.TrimPrefix(string(en.Path), rel)
		paths = append(paths, p)
		byPath[p] = en
	}
	want := []string{"", "/big", "/d1", "/d1/d2", "/d1/d2/nested.txt", "/empty", "/link", "/text.txt"}
	if !slices.Equal(paths, want) {
		t.Fatalf("entries = %q, want %q", paths, want)
	}
	if e.logs.count(`"file-skipped"`) != 1 || e.logs.count("/fifo") != 1 {
		t.Error("fifo not logged as file-skipped")
	}
	if en := byPath["/link"]; en.Type != backupv1.Entry_TYPE_SYMLINK || string(en.SymlinkTarget) != "text.txt" {
		t.Errorf("link = %v", en)
	}
	if en := byPath["/d1"]; en.Type != backupv1.Entry_TYPE_DIRECTORY || en.Mode != 0o755 {
		t.Errorf("d1 = %v", en)
	}
	if en := byPath["/empty"]; en.Type != backupv1.Entry_TYPE_FILE || en.Size != 0 || len(en.Chunks) != 0 {
		t.Errorf("empty = %v", en)
	}
	en := byPath["/big"]
	var got []byte
	for _, id := range en.Chunks {
		got = append(got, e.chunk(t, id)...)
	}
	if en.Size != uint64(len(big)) || en.Mode != 0o640 || en.Uid != uint32(os.Getuid()) || len(en.Chunks) < 3 || !bytes.Equal(got, big) {
		t.Errorf("big: size %d, mode %o, uid %d, %d chunks, content equal %v", en.Size, en.Mode, en.Uid, len(en.Chunks), bytes.Equal(got, big))
	}
	if string(e.chunk(t, byPath["/text.txt"].Chunks[0])) != "hello, azukari\n" {
		t.Error("text.txt content")
	}
	tot := root.Totals
	if root.Format != Format || !slices.Equal(root.Paths, e.cfg.Exports["data"].Paths) || tot.Entries != 8 ||
		tot.Bytes != uint64(len(big))+15+7 || tot.Chunks != uint64(len(chunks)) || tot.NewChunks != tot.Chunks ||
		root.EndTimeNs < root.StartTimeNs {
		t.Errorf("root = %v", root)
	}
	if e.logs.count(`"backup-completed"`) != 1 {
		t.Error("no backup-completed")
	}

	// Second run, one file changed: only its chunk and the metadata stream's are new.
	write(t, filepath.Join(e.tree, "text.txt"), []byte("hello again\n"))
	second := e.run(t)
	root2, _ := e.snapshot(t, second)
	if n := root2.Totals.NewChunks; n == 0 || n > 3 {
		t.Errorf("second run uploaded %d chunks, want 1 to 3", n)
	}
}

// A chunk pruned between missing and refs: refs-incomplete, the run uploads it again.
func TestRefsIncomplete(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	e.run(t) // a first snapshot, so the second run finds every chunk present
	write(t, filepath.Join(e.tree, "new.txt"), []byte("new\n"))
	beforeRefs = func() {
		// Every chunk, the metadata stream's included: both ways back are taken.
		if err := os.RemoveAll(filepath.Join(e.bucket, "objects", "node-a", "chunks")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeRefs = func() {} })
	name := e.run(t)
	if e.logs.count(`"reason":"refs-incomplete"`) != 1 {
		t.Errorf("want one refs-incomplete refusal")
	}
	refs := e.refsSet(t, name)
	if m, err := e.c.Missing(context.Background(), refs); err != nil || len(m) != 0 {
		t.Errorf("missing after the run: %v, %v", m, err)
	}
	e.snapshot(t, name) // the metadata stream opens again
}

// The refs and chunks pruned before the root (ADR 0025): the root is refused once, the run
// uploads the chunks and the refs again, then the root.
func TestRootRefsIncomplete(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	calls := 0
	beforeRoot = func() {
		if calls++; calls > 1 {
			return
		}
		for _, d := range []string{"refs", "chunks"} {
			if err := os.RemoveAll(filepath.Join(e.bucket, "objects", "node-a", d)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { beforeRoot = func() {} })
	name := e.run(t)
	if calls != 2 || e.logs.count(`"reason":"refs-incomplete"`) != 1 {
		t.Errorf("%d root PUTs, %d refs-incomplete; want 2 and 1", calls, e.logs.count(`"reason":"refs-incomplete"`))
	}
	refs := e.refsSet(t, name)
	if m, err := e.c.Missing(context.Background(), refs); err != nil || len(m) != 0 {
		t.Errorf("missing after the run: %v, %v", m, err)
	}
	e.snapshot(t, name)
}

// The refs pruned before every root PUT: the run fails after RootAttempts.
func TestRootRefsIncompleteFails(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	calls := 0
	beforeRoot = func() {
		calls++
		if err := os.RemoveAll(filepath.Join(e.bucket, "objects", "node-a", "refs")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeRoot = func() {} })
	e.runFails(t, "refs-incomplete")
	if calls != RootAttempts || e.logs.count(`"reason":"refs-incomplete"`) != RootAttempts {
		t.Errorf("%d root PUTs, %d refs-incomplete; want %d", calls, e.logs.count(`"reason":"refs-incomplete"`), RootAttempts)
	}
}

func TestWrongKey(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	e.run(t)
	before := len(e.list(t))
	e.cfg.DataKeyFile = writeKey(t, keyB)
	_, err := Run(context.Background(), e.cfg, "data", logging.New(e.logs, slog.LevelInfo))
	if err == nil || !strings.Contains(err.Error(), "wrong data key") {
		t.Fatalf("err = %v, want wrong data key", err)
	}
	if e.logs.count(`"backup-failed"`) != 1 {
		t.Error("no backup-failed")
	}
	if after := len(e.list(t)); after != before {
		t.Errorf("%d objects after the refused run, want %d", after, before)
	}
}

// A file of more than ~120k chunks has an Entry over protodelim's 4 MiB default: run's
// recovery and restore must both read it.
func TestReadEntryLarge(t *testing.T) {
	e := &backupv1.Entry{Path: []byte("var/lib/big")}
	for i := range 130_000 {
		id := make([]byte, 32)
		id[0], id[1], id[2] = byte(i), byte(i>>8), byte(i>>16)
		e.Chunks = append(e.Chunks, id)
	}
	var buf bytes.Buffer
	if _, err := protodelim.MarshalTo(&buf, e); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= 4<<20 {
		t.Fatalf("record is %d bytes, want over 4 MiB", buf.Len())
	}
	if err := protodelim.UnmarshalFrom(bufio.NewReader(bytes.NewReader(buf.Bytes())), &backupv1.Entry{}); err == nil {
		t.Fatal("default options read it: the test no longer covers the bound")
	}
	got := &backupv1.Entry{}
	if err := readEntry(bufio.NewReader(&buf), got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, e) {
		t.Fatal("entry differs after the round trip")
	}
}

// runFails runs once, expects backup-failed naming want and no snapshot root published.
func (e *env) runFails(t *testing.T, want string) {
	t.Helper()
	_, err := Run(context.Background(), e.cfg, "data", logging.New(e.logs, slog.LevelInfo))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want one naming %q", err, want)
	}
	if e.logs.count(`"backup-failed"`) != 1 || e.logs.count(`"backup-completed"`) != 0 {
		t.Error("want one backup-failed, no backup-completed")
	}
	for _, k := range e.list(t) {
		if strings.HasPrefix(k, "node-a/snapshots/") {
			t.Errorf("root %s published by a failed run", k)
		}
	}
}

// A file or directory that cannot be read fails the run: never a root that lacks it.
func TestUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a mode 000 path")
	}
	for _, kind := range []string{"file", "dir"} {
		t.Run(kind, func(t *testing.T) {
			e := start(t)
			e.makeTree(t)
			p := filepath.Join(e.tree, "text.txt")
			if kind == "dir" {
				p = filepath.Join(e.tree, "d1")
			}
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(p, 0o755) }) // TempDir's cleanup must list it
			e.runFails(t, p)
		})
	}
}

// A read failing after several chunks were added fails the run.
func TestReadErrorMidway(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	big := filepath.Join(e.tree, "big")
	fileReader = func(f *os.File) io.Reader {
		if f.Name() != big {
			return f
		}
		return io.MultiReader(io.LimitReader(f, 6<<20), iotest.ErrReader(errors.New("disk on fire")))
	}
	t.Cleanup(func() { fileReader = func(f *os.File) io.Reader { return f } })
	e.runFails(t, "disk on fire")
	if !strings.Contains(e.logs.b.String(), big) {
		t.Error("backup-failed does not name the file")
	}
}

// A file removed after its directory was listed, before its lstat or open, is file-skipped:
// the run succeeds and the snapshot restores without it.
func TestVanished(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	statGone, openGone := filepath.Join(e.tree, "gone-stat"), filepath.Join(e.tree, "gone-open")
	write(t, statGone, []byte("x"))
	write(t, openGone, []byte("y"))
	beforeRead = func(path string) {
		switch path {
		case filepath.Join(e.tree, "big"): // listed already, lstat comes later
			os.Remove(statGone)
		case openGone:
			os.Remove(openGone)
		}
	}
	t.Cleanup(func() { beforeRead = func(string) {} })
	before, err := os.Stat(e.tree)
	if err != nil {
		t.Fatal(err)
	}
	name := e.run(t)
	// The removals moved the directory's mtime after it was saved.
	if err := os.Chtimes(e.tree, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{statGone, openGone} {
		if e.logs.count(`"file-skipped","path":"`+p+`"`) != 1 {
			t.Errorf("%s not logged as file-skipped\n%s", p, e.logs.b.String())
		}
	}
	target := t.TempDir()
	if err := e.restore(name, target, false); err != nil {
		t.Fatal(err)
	}
	sameTree(t, e.tree, target)
}

// A root sealed over maxRoot, which a restore would refuse, is never published.
func TestRootTooLarge(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	m := maxRoot
	maxRoot = 64
	t.Cleanup(func() { maxRoot = m })
	e.runFails(t, "over the 64 a restore reads")
}

// A symlink removed after its directory was listed, before its readlink, is file-skipped.
func TestVanishedSymlink(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	link := filepath.Join(e.tree, "gone-link")
	if err := os.Symlink("text.txt", link); err != nil {
		t.Fatal(err)
	}
	beforeRead = func(path string) {
		if path == link {
			os.Remove(link)
		}
	}
	t.Cleanup(func() { beforeRead = func(string) {} })
	before, err := os.Stat(e.tree)
	if err != nil {
		t.Fatal(err)
	}
	name := e.run(t)
	// The removal moved the directory's mtime after it was saved.
	if err := os.Chtimes(e.tree, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if e.logs.count(`"file-skipped","path":"`+link+`"`) != 1 {
		t.Errorf("%s not logged as file-skipped\n%s", link, e.logs.b.String())
	}
	target := t.TempDir()
	if err := e.restore(name, target, false); err != nil {
		t.Fatal(err)
	}
	sameTree(t, e.tree, target)
}
