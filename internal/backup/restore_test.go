package backup

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	backupv1 "github.com/scorsi/azukari/gen/azukari/backup/v1"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/seal"
)

func (e *env) restore(name, target string, verify bool) error {
	// The restored d1 is read-only: let t.TempDir's cleanup remove it.
	defer os.Chmod(filepath.Join(target, e.tree, "d1"), 0o755)
	return Restore(context.Background(), e.cfg, name, target, verify, logging.New(e.logs, slog.LevelInfo))
}

// sameTree compares src with its copy under target: types, content, modes, mtimes, symlink
// targets, and owners when run as root. FIFOs are not backed up.
func sameTree(t *testing.T, src, target string) {
	t.Helper()
	dst := filepath.Join(target, src)
	n := 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeNamedPipe != 0 {
			return err
		}
		n++
		q := filepath.Join(dst, strings.TrimPrefix(p, src))
		a, err := os.Lstat(p)
		if err != nil {
			return err
		}
		b, err := os.Lstat(q)
		if err != nil {
			t.Errorf("%s: %v", q, err)
			return nil
		}
		sa, sb := a.Sys().(*syscall.Stat_t), b.Sys().(*syscall.Stat_t)
		if a.Mode().Type() != b.Mode().Type() || a.Mode().Type() != fs.ModeSymlink && sa.Mode&0o7777 != sb.Mode&0o7777 ||
			!a.ModTime().Equal(b.ModTime()) || os.Geteuid() == 0 && (sa.Uid != sb.Uid || sa.Gid != sb.Gid) {
			t.Errorf("%s: %v %v %v, restored %v %v %v", p, a.Mode(), a.ModTime(), sa.Uid, b.Mode(), b.ModTime(), sb.Uid)
		}
		switch a.Mode().Type() {
		case 0:
			x, _ := os.ReadFile(p)
			y, _ := os.ReadFile(q)
			if !bytes.Equal(x, y) {
				t.Errorf("%s: content differs", p)
			}
		case fs.ModeSymlink:
			x, _ := os.Readlink(p)
			y, _ := os.Readlink(q)
			if x != y {
				t.Errorf("%s: link %q, restored %q", p, x, y)
			}
		}
		return nil
	})
	if err != nil || n < 8 {
		t.Fatalf("walk: %v, %d entries", err, n)
	}
}

func TestRestore(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	os.Chmod(filepath.Join(e.tree, "d1"), 0o500) // read-only: restored after its contents
	t.Cleanup(func() { os.Chmod(filepath.Join(e.tree, "d1"), 0o755) })
	first := e.run(t)

	target := filepath.Join(t.TempDir(), "r")
	if err := Restore(context.Background(), e.cfg, first, target, false, logging.New(e.logs, slog.LevelInfo)); err != nil {
		t.Fatalf("restore: %v\n%s", err, e.logs.b.String())
	}
	sameTree(t, e.tree, target)
	os.Chmod(filepath.Join(target, e.tree, "d1"), 0o755)
	if e.logs.count(`"restore-completed"`) != 1 {
		t.Error("no restore-completed")
	}

	// A non-empty target is refused, a symlink to a directory too.
	if err := e.restore(first, target, false); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("restore into a full target: %v", err)
	}
	link := filepath.Join(t.TempDir(), "l")
	os.Symlink(t.TempDir(), link)
	if err := e.restore(first, link, false); err == nil {
		t.Error("restore through a symlinked target accepted")
	}

	// --verify passes and writes nothing.
	absent := filepath.Join(t.TempDir(), "v")
	if err := e.restore(first, absent, true); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Errorf("verify wrote %s: %v", absent, err)
	}

	// A second snapshot with a change: the first still restores as it was.
	write(t, filepath.Join(e.tree, "text.txt"), []byte("hello again\n"))
	second := e.run(t)
	old := filepath.Join(t.TempDir(), "old")
	if err := e.restore(first, old, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(old, e.tree, "text.txt")); string(b) != "hello, azukari\n" {
		t.Errorf("first snapshot's text.txt = %q", b)
	}
	// An existing empty directory is a valid target, as an absent one.
	empty := t.TempDir()
	if err := Restore(context.Background(), e.cfg, second, empty, false, logging.New(e.logs, slog.LevelInfo)); err != nil {
		t.Fatalf("restore into an existing empty target: %v", err)
	}
	sameTree(t, e.tree, empty)
	os.Chmod(filepath.Join(empty, e.tree, "d1"), 0o755)

	// Both listed, first field the name.
	var out bytes.Buffer
	if err := Snapshots(context.Background(), e.cfg, &out, logging.New(e.logs, slog.LevelInfo)); err != nil {
		t.Fatal(err)
	}
	var names []string
	for l := range strings.Lines(out.String()) {
		names = append(names, strings.Fields(l)[0])
		if !strings.Contains(l, " entries=8 ") {
			t.Errorf("snapshot line %q", l)
		}
	}
	// Name order: both runs may share a second, the random suffix then decides.
	if slices.Sort(names); !slices.Equal(names, slices.Sorted(slices.Values([]string{first, second}))) {
		t.Errorf("snapshots:\n%s", out.String())
	}
}

// A chunk corrupted on the store's disk: the restore fails naming it.
func TestRestoreCorruptChunk(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	name := e.run(t)
	_, entries := e.snapshot(t, name)
	var id []byte
	for _, en := range entries {
		if strings.HasSuffix(string(en.Path), "/text.txt") {
			id = en.Chunks[0]
		}
	}
	k := "node-a/" + seal.ChunkPath([32]byte(id))
	f := filepath.Join(e.bucket, "objects", k)
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 1
	if err := os.WriteFile(f, b, 0o600); err != nil {
		t.Fatal(err)
	}
	err = e.restore(name, filepath.Join(t.TempDir(), "r"), true)
	if err == nil || !strings.Contains(err.Error(), k) {
		t.Fatalf("err = %v, want one naming %s", err, k)
	}
	if e.logs.count(`"restore-failed"`) != 1 {
		t.Error("no restore-failed")
	}
}

// Crafted entries: their paths are refused before anything is written outside the target.
func TestRestoreRefusedPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []*backupv1.Entry
	}{
		{"dotdot", []*backupv1.Entry{{Path: []byte("../x"), Type: backupv1.Entry_TYPE_FILE}}},
		{"absolute", []*backupv1.Entry{{Path: []byte("/x"), Type: backupv1.Entry_TYPE_FILE}}},
		{"dot", []*backupv1.Entry{{Path: []byte("a/./x"), Type: backupv1.Entry_TYPE_FILE}}},
		{"duplicate", []*backupv1.Entry{{Path: []byte("a"), Type: backupv1.Entry_TYPE_DIRECTORY},
			{Path: []byte("a"), Type: backupv1.Entry_TYPE_DIRECTORY}}},
		{"through a symlink", []*backupv1.Entry{{Path: []byte("a"), Type: backupv1.Entry_TYPE_DIRECTORY},
			{Path: []byte("a/l"), Type: backupv1.Entry_TYPE_SYMLINK, SymlinkTarget: []byte("../..")},
			{Path: []byte("a/l/x"), Type: backupv1.Entry_TYPE_FILE}}},
		{"no parent", []*backupv1.Entry{{Path: []byte("a/x"), Type: backupv1.Entry_TYPE_FILE}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := start(t)
			name := e.crafted(t, tc.entries)
			out := t.TempDir()
			target := filepath.Join(out, "r", "s")
			os.Mkdir(filepath.Dir(target), 0o700)
			err := e.restore(name, target, false)
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if _, err := os.Lstat(filepath.Join(out, "x")); err == nil {
				t.Error("x written outside the target")
			}
			if _, err := os.Lstat(filepath.Join(out, "r", "x")); err == nil {
				t.Error("x written outside the target")
			}
		})
	}
}

// crafted stores a snapshot of entries sealed with node-a's key, paths "/a" configured.
func (e *env) crafted(t *testing.T, entries []*backupv1.Entry) string {
	t.Helper()
	ctx := context.Background()
	if _, err := e.c.Put(ctx, "node-a/config", e.key.Config()); err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	for _, en := range entries {
		protodelim.MarshalTo(&stream, en)
	}
	id, sealed := e.key.SealChunk(stream.Bytes())
	if _, err := e.c.Put(ctx, "node-a/"+seal.ChunkPath(id), sealed); err != nil {
		t.Fatal(err)
	}
	const name = "20261029T120000Z-0badf00d"
	if _, err := e.c.Put(ctx, "node-a/refs/"+name, id[:]); err != nil {
		t.Fatal(err)
	}
	root, _ := proto.Marshal(&backupv1.Root{Format: Format, Paths: []string{"/a"}, MetadataChunks: [][]byte{id[:]},
		Totals: &backupv1.Totals{Entries: uint64(len(entries)), Chunks: 1}})
	if _, err := e.c.Put(ctx, "node-a/snapshots/"+name, e.key.SealRoot(name, root)); err != nil {
		t.Fatal(err)
	}
	return name
}

// Every file and directory is synced, the made parents, the target and its parent too; a
// failing fsync, whichever, fails the restore.
func TestRestoreSync(t *testing.T) {
	e := start(t)
	e.makeTree(t)
	name := e.run(t)
	t.Cleanup(func() { fsync = unix.Fsync })

	calls := 0
	fsync = func(fd int) error { calls++; return unix.Fsync(fd) }
	if err := e.restore(name, filepath.Join(t.TempDir(), "r"), false); err != nil {
		t.Fatal(err)
	}
	// 4 files, 3 directories, the parents made above the tree, the target and its parent.
	made := len(strings.Split(strings.TrimPrefix(filepath.Dir(e.tree), "/"), "/"))
	if want := 4 + 3 + made + 2; calls != want {
		t.Errorf("%d fsyncs, want %d", calls, want)
	}
	if fsync = func(int) error { panic("fsync under verify") }; e.restore(name, filepath.Join(t.TempDir(), "v"), true) != nil {
		t.Error("verify failed")
	}

	for fail := range calls {
		n := 0
		fsync = func(fd int) error {
			if n++; n-1 == fail {
				return syscall.EIO
			}
			return unix.Fsync(fd)
		}
		failed := e.logs.count(`"restore-failed"`)
		if err := e.restore(name, filepath.Join(t.TempDir(), "r"), false); !errors.Is(err, syscall.EIO) {
			t.Errorf("fsync %d failing: err = %v", fail, err)
		}
		if e.logs.count(`"restore-failed"`) != failed+1 {
			t.Errorf("fsync %d failing: no restore-failed", fail)
		}
	}
	if e.logs.count(`"restore-completed"`) != 2 {
		t.Error("a restore with a failed fsync completed")
	}
}
