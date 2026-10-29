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
			Path: "etc/hosts", Type: backupv1.Entry_TYPE_FILE, Mode: 0o644, MtimeNs: 1,
			Size: 3, Chunks: [][]byte{make([]byte, 32)},
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
