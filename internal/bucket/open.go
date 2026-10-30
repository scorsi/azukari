// Package bucket holds a bucket's layout on disk (ADR 0004): FORMAT, objects/, tmp/ and the
// object header.
package bucket

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FormatContent is FORMAT's whole content: format version 2, one snapshot per export (ADR
// 0004, 0030). A bucket of another version is refused, by the store and by a restore.
const FormatContent = "2\n"

// State is a bucket's state after Open.
type State int

const (
	// Unavailable: Open's error says why; the bucket answers 503 bucket-unavailable.
	Unavailable State = iota
	// Available: FORMAT was there and valid.
	Available
	// Created: the layout was created now; the caller logs bucket-created, then serves it.
	Created
)

func (s State) String() string {
	return [...]string{"unavailable", "available", "created"}[s]
}

// Open applies ADR 0004's start decisions to the bucket at path, which must live on the
// mount mountPoint, then empties tmp/. A non-nil error always comes with Unavailable and is
// the reason the caller logs. isMount is IsMountPoint outside unit tests.
func Open(path, mountPoint string, isMount func(string) (bool, error)) (State, error) {
	switch data, err := os.ReadFile(filepath.Join(path, "FORMAT")); {
	case err == nil && string(data) == FormatContent:
		// A creation interrupted after FORMAT may have left objects/ or tmp/ missing.
		if err := makeSubdirs(path); err != nil {
			return Unavailable, err
		}
		if err := emptyDir(filepath.Join(path, "tmp")); err != nil {
			return Unavailable, err
		}
		return Available, nil
	case err == nil:
		return Unavailable, fmt.Errorf("%s/FORMAT holds %q, want %q", path, data, FormatContent)
	case !errors.Is(err, fs.ErrNotExist):
		return Unavailable, err
	}

	// No FORMAT: create only on the bucket's own, mounted, filesystem.
	if ok, err := isMount(mountPoint); err != nil {
		return Unavailable, fmt.Errorf("mount_point %s: %w", mountPoint, err)
	} else if !ok {
		return Unavailable, fmt.Errorf("mount_point %s is not a mount point", mountPoint)
	}
	if err := checkUnder(path, mountPoint); err != nil {
		return Unavailable, err
	}
	entries, err := os.ReadDir(path)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return Unavailable, err
	case len(entries) > 0:
		return Unavailable, fmt.Errorf("%s holds files but no FORMAT: never adopted", path)
	}
	if err := create(path); err != nil {
		return Unavailable, fmt.Errorf("create %s: %w", path, err)
	}
	return Created, nil
}

// IsMountPoint reports whether dir is on another device than its parent ("/" always is).
func IsMountPoint(dir string) (bool, error) {
	var st, parent syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return false, err
	}
	if err := syscall.Stat(filepath.Join(dir, ".."), &parent); err != nil {
		return false, err
	}
	return st.Dev != parent.Dev || st.Ino == parent.Ino, nil
}

// checkUnder refuses a path that is not strictly under mountPoint, lexically or once the
// symbolic links of its longest existing prefix are resolved.
func checkUnder(path, mountPoint string) error {
	if !filepath.IsAbs(path) || !strictlyUnder(filepath.Clean(path), filepath.Clean(mountPoint)) {
		return fmt.Errorf("path %s is not under mount_point %s", path, mountPoint)
	}
	mp, err := filepath.EvalSymlinks(mountPoint)
	if err != nil {
		return err
	}
	existing, rest := filepath.Clean(path), ""
	for {
		real, err := filepath.EvalSymlinks(existing)
		if err == nil {
			if resolved := filepath.Join(real, rest); !strictlyUnder(resolved, mp) {
				return fmt.Errorf("path %s resolves to %s, not under mount_point %s", path, resolved, mp)
			}
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		existing, rest = filepath.Dir(existing), filepath.Join(filepath.Base(existing), rest)
	}
}

func strictlyUnder(p, dir string) bool {
	return p != dir && strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// create makes path (and its missing parents), FORMAT first, then objects/ and tmp/, each
// fsynced in its parent directory. FORMAT first: a crash midway leaves a bucket Open finishes,
// never a non-empty directory without FORMAT, which would never be adopted.
func create(path string) error {
	if err := mkdirAllSync(path); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(path, "FORMAT"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(FormatContent)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = syncDir(path)
	}
	if err != nil {
		return err
	}
	return makeSubdirs(path)
}

// makeSubdirs creates objects/ and tmp/ if missing, each fsynced in path.
func makeSubdirs(path string) error {
	for _, d := range []string{"objects", "tmp"} {
		if err := mkdirAllSync(filepath.Join(path, d)); err != nil {
			return err
		}
	}
	return nil
}

// mkdirAllSync creates dir and its missing parents, fsyncing each new directory's parent.
func mkdirAllSync(dir string) error {
	if fi, err := os.Stat(dir); err == nil {
		if !fi.IsDir() {
			return &fs.PathError{Op: "mkdir", Path: dir, Err: syscall.ENOTDIR}
		}
		return nil
	}
	parent := filepath.Dir(dir)
	if err := mkdirAllSync(parent); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// SyncDir fsyncs directory dir: its entries become durable.
func SyncDir(dir string) error { return syncDir(dir) }

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// emptyDir removes everything in dir: tmp/ only ever holds unfinished writes.
func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
