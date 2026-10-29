// Package gen_test checks that the generated code links and round-trips (ADR 0003, 0006).
package gen_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	backupv1 "github.com/scorsi/azukari/gen/azukari/backup/v1"
	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
)

func TestRoundTrip(t *testing.T) {
	for _, m := range []proto.Message{
		&azukariv1.ListResponse{
			Objects: []*azukariv1.Object{{Key: "node-a/config", Size: 42, Sha256: make([]byte, 32)}},
			Next:    "node-a/config",
		},
		&backupv1.Entry{
			Path: []byte("etc/hosts"), Type: backupv1.Entry_TYPE_FILE, Mode: 0o644, MtimeNs: 1,
			Size: 3, Chunks: [][]byte{make([]byte, 32)},
		},
		// Unix names are bytes: 0xff and invalid UTF-8 must survive exactly.
		&backupv1.Entry{Path: []byte("var/lib/a/n\xffame"), Type: backupv1.Entry_TYPE_FILE},
		&backupv1.Entry{
			Path: []byte("var/lib/a/link"), Type: backupv1.Entry_TYPE_SYMLINK,
			SymlinkTarget: []byte("../\xc3\x28\xff"),
		},
	} {
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		got := m.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(b, got); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(m, got) {
			t.Errorf("round trip: got %v, want %v", got, m)
		}
	}
}

// Paths are relative to the filesystem root, so the same name under two configured paths
// stays two distinct entries.
func TestEntryPathsDoNotCollide(t *testing.T) {
	var got []string
	for _, p := range []string{"var/lib/a/config", "var/lib/b/config"} {
		b, err := proto.Marshal(&backupv1.Entry{Path: []byte(p), Type: backupv1.Entry_TYPE_FILE})
		if err != nil {
			t.Fatal(err)
		}
		var e backupv1.Entry
		if err := proto.Unmarshal(b, &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, string(e.Path))
	}
	if got[0] != "var/lib/a/config" || got[1] != "var/lib/b/config" {
		t.Errorf("entries: got %q", got)
	}
}
