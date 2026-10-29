package backup

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/protodelim"
	"google.golang.org/protobuf/proto"

	backupv1 "github.com/scorsi/azukari/gen/azukari/backup/v1"
	"github.com/scorsi/azukari/internal/seal"
)

// Fixed format vectors (ADR 0006, 0010) under the seal package's test key (keyA). Never
// regenerate testdata/metadata-v1.bin or testdata/root-v1.bin: a change is a format version.

const vectorRootName = "20261029T120000Z-0011aabb"

func id32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// vectorEntries is the content of testdata/metadata-v1.bin, in path order.
func vectorEntries() []*backupv1.Entry {
	return []*backupv1.Entry{
		{Path: []byte("etc"), Type: backupv1.Entry_TYPE_DIRECTORY, Mode: 0o755, MtimeNs: 1793275200123456789},
		{Path: []byte("etc/a"), Type: backupv1.Entry_TYPE_FILE, Mode: 0o644, Uid: 1000, Gid: 100, MtimeNs: 1793275200000000001, Size: 5 << 20, Chunks: [][]byte{id32(0x11), id32(0x22)}},
		{Path: []byte("etc/empty"), Type: backupv1.Entry_TYPE_FILE, Mode: 0o600, MtimeNs: -1},
		{Path: []byte("etc/l"), Type: backupv1.Entry_TYPE_SYMLINK, Mode: 0o777, MtimeNs: 1, SymlinkTarget: []byte("../var/\xff")},
		{Path: []byte("etc/\xfe\x00x"), Type: backupv1.Entry_TYPE_FILE, Mode: 0o4755, Uid: 4294967295, Gid: 4294967294, Size: 1, Chunks: [][]byte{id32(0x33)}},
	}
}

func vectorKey(t *testing.T) *seal.Key {
	t.Helper()
	k, err := seal.ParseKey([]byte(keyA))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestMetadataVector(t *testing.T) {
	stream, err := os.ReadFile("testdata/metadata-v1.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := vectorEntries()
	br := bufio.NewReader(bytes.NewReader(stream))
	for i := 0; ; i++ {
		e := &backupv1.Entry{}
		err := readEntry(br, e)
		if err == io.EOF {
			if i != len(want) {
				t.Fatalf("%d entries, want %d", i, len(want))
			}
			break
		}
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if i >= len(want) || !proto.Equal(e, want[i]) {
			t.Fatalf("entry %d = %v", i, e)
		}
	}
	// The writer's encoding of the same entries is these exact bytes.
	var b bytes.Buffer
	for _, e := range want {
		if _, err := protodelim.MarshalTo(&b, e); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(b.Bytes(), stream) {
		t.Errorf("metadata stream encoding changed (a format version, ADR 0006): %x", b.Bytes())
	}
}

func TestRootVector(t *testing.T) {
	k := vectorKey(t)
	sealed, err := os.ReadFile("testdata/root-v1.bin")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := os.ReadFile("testdata/metadata-v1.bin")
	if err != nil {
		t.Fatal(err)
	}
	p, err := k.OpenRoot(vectorRootName, sealed)
	if err != nil {
		t.Fatal(err)
	}
	got := &backupv1.Root{}
	if err := proto.Unmarshal(p, got); err != nil {
		t.Fatal(err)
	}
	metaID := k.ChunkID(stream)
	want := &backupv1.Root{
		Format: 1, StartTimeNs: 1793275200000000000, EndTimeNs: 1793275261500000000,
		Paths: []string{"/etc", "/var/lib/a"}, MetadataChunks: [][]byte{metaID[:]},
		Totals: &backupv1.Totals{Entries: 5, Bytes: 5<<20 + 1, Chunks: 4, NewChunks: 2},
	}
	if !proto.Equal(got, want) {
		t.Errorf("root = %v\nwant %v", got, want)
	}
	if Format != want.Format {
		t.Errorf("Format = %d, the vector is format %d", Format, want.Format)
	}
}
