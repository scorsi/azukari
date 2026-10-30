package bucket

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mounted(string) (bool, error)    { return true, nil }
func notMounted(string) (bool, error) { return false, nil }

func checkLayout(t *testing.T, path string) {
	t.Helper()
	if data, err := os.ReadFile(filepath.Join(path, "FORMAT")); err != nil || string(data) != "2\n" {
		t.Fatalf("FORMAT: %q, %v", data, err)
	}
	for _, d := range []string{"objects", "tmp"} {
		if fi, err := os.Stat(filepath.Join(path, d)); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", d, err)
		}
	}
}

func TestOpenCreates(t *testing.T) {
	for name, mk := range map[string]func(string) error{
		"absent":         func(string) error { return nil },
		"empty":          func(p string) error { return os.Mkdir(p, 0o700) },
		"parents absent": nil,
	} {
		t.Run(name, func(t *testing.T) {
			mp := t.TempDir()
			path := filepath.Join(mp, "backup")
			if mk == nil {
				path = filepath.Join(mp, "a", "b", "backup")
			} else if err := mk(path); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(path, mp, mounted); s != Created || err != nil {
				t.Fatalf("Open = %v, %v", s, err)
			}
			checkLayout(t, path)
			// Next start: available, tmp/ emptied, objects kept.
			must(t, os.WriteFile(filepath.Join(path, "tmp", "partial"), []byte("x"), 0o600))
			must(t, os.MkdirAll(filepath.Join(path, "tmp", "d", "e"), 0o700))
			must(t, os.WriteFile(filepath.Join(path, "objects", "o"), []byte("x"), 0o600))
			if s, err := Open(path, mp, notMounted); s != Available || err != nil {
				t.Fatalf("reopen = %v, %v", s, err)
			}
			if entries, err := os.ReadDir(filepath.Join(path, "tmp")); err != nil || len(entries) != 0 {
				t.Fatalf("tmp/ not emptied: %v, %v", entries, err)
			}
			if _, err := os.Stat(filepath.Join(path, "objects", "o")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A creation interrupted after FORMAT: Open finishes the layout and serves the bucket.
func TestOpenFormatOnly(t *testing.T) {
	mp := t.TempDir()
	path := filepath.Join(mp, "backup")
	must(t, os.Mkdir(path, 0o700))
	must(t, os.WriteFile(filepath.Join(path, "FORMAT"), []byte(FormatContent), 0o600))
	if s, err := Open(path, mp, notMounted); s != Available || err != nil {
		t.Fatalf("Open = %v, %v", s, err)
	}
	checkLayout(t, path)
}

func TestOpenUnavailable(t *testing.T) {
	type tc struct {
		setup   func(t *testing.T, mp string) (path, mountPoint string)
		isMount func(string) (bool, error)
		want    string
	}
	backup := func(_ *testing.T, mp string) (string, string) { return filepath.Join(mp, "backup"), mp }
	for name, c := range map[string]tc{
		"not mounted":       {backup, notMounted, "not a mount point"},
		"mount check fails": {backup, func(string) (bool, error) { return false, errors.New("boom") }, "boom"},
		"not under": {func(_ *testing.T, mp string) (string, string) {
			return filepath.Join(mp, "other", "backup"), filepath.Join(mp, "disk")
		}, mounted, "not under"},
		"equal to mount point": {func(_ *testing.T, mp string) (string, string) { return mp, mp }, mounted, "not under"},
		"relative":             {func(_ *testing.T, mp string) (string, string) { return "backup", mp }, mounted, "not under"},
		"symlink escapes": {func(t *testing.T, mp string) (string, string) {
			disk, root := filepath.Join(mp, "disk"), filepath.Join(mp, "root")
			must(t, os.Mkdir(disk, 0o700))
			must(t, os.Mkdir(root, 0o700))
			must(t, os.Symlink(root, filepath.Join(disk, "link")))
			return filepath.Join(disk, "link", "backup"), disk
		}, mounted, "resolves to"},
		"files without FORMAT": {func(t *testing.T, mp string) (string, string) {
			p := filepath.Join(mp, "backup")
			must(t, os.MkdirAll(filepath.Join(p, "objects"), 0o700))
			return p, mp
		}, mounted, "never adopted"},
		// The state an older build's interrupted creation left (subdirectories, no FORMAT).
		"subdirs without FORMAT": {func(t *testing.T, mp string) (string, string) {
			p := filepath.Join(mp, "backup")
			must(t, os.MkdirAll(filepath.Join(p, "objects"), 0o700))
			must(t, os.Mkdir(filepath.Join(p, "tmp"), 0o700))
			return p, mp
		}, mounted, "never adopted"},
		"version 1 FORMAT": {func(t *testing.T, mp string) (string, string) {
			p := filepath.Join(mp, "backup")
			must(t, os.Mkdir(p, 0o700))
			must(t, os.WriteFile(filepath.Join(p, "FORMAT"), []byte("1\n"), 0o600))
			return p, mp
		}, mounted, "FORMAT holds"},
		"path is a file": {func(t *testing.T, mp string) (string, string) {
			p := filepath.Join(mp, "backup")
			must(t, os.WriteFile(p, nil, 0o600))
			return p, mp
		}, mounted, "not a directory"},
	} {
		t.Run(name, func(t *testing.T) {
			path, mp := c.setup(t, t.TempDir())
			before := listTree(t, filepath.Dir(mp))
			s, err := Open(path, mp, c.isMount)
			if s != Unavailable || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Open = %v, %v; want unavailable %q", s, err, c.want)
			}
			if after := listTree(t, filepath.Dir(mp)); after != before {
				t.Fatalf("Open changed the tree:\n%s\n->\n%s", before, after)
			}
		})
	}
}

func TestIsMountPoint(t *testing.T) {
	if ok, err := IsMountPoint("/"); !ok || err != nil {
		t.Fatalf("/: %v, %v", ok, err)
	}
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	if ok, err := IsMountPoint(filepath.Join(dir, "sub")); ok || err != nil {
		t.Fatalf("plain dir: %v, %v", ok, err)
	}
	if _, err := IsMountPoint(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("absent dir: no error")
	}
}

func listTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	must(t, filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		b.WriteString(p + "\n")
		return err
	}))
	return b.String()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
