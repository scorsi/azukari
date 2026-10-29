package bucket

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"testing"
)

// vectorHeader is the header pinned by testdata/header-v1.bin.
var vectorHeader = Header{
	Size:        5,
	SHA256:      sha256.Sum256([]byte("hello")),
	ReceiveTime: 1_793_275_200_123_456_789, // 2026-10-29T12:00:00.123456789Z
}

func TestHeaderVector(t *testing.T) {
	want, err := os.ReadFile("testdata/header-v1.bin")
	if err != nil {
		t.Fatal(err)
	}
	if got := vectorHeader.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("Encode changed the format:\n got %x\nwant %x", got, want)
	}
	h, err := DecodeHeader(want, HeaderSize+5)
	if err != nil || h != vectorHeader {
		t.Fatalf("DecodeHeader = %+v, %v", h, err)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	for _, h := range []Header{{}, vectorHeader, {Size: 1 << 40, ReceiveTime: -1}} {
		b := h.Encode()
		if len(b) != HeaderSize {
			t.Fatalf("len %d", len(b))
		}
		got, err := DecodeHeader(append(b, 'x'), HeaderSize+int64(h.Size))
		if err != nil || got != h {
			t.Fatalf("round trip %+v: %+v, %v", h, got, err)
		}
	}
}

func TestHeaderCorrupt(t *testing.T) {
	good := vectorHeader.Encode()
	for name, c := range map[string]struct {
		at   int
		val  byte
		size int64
	}{
		"magic":       {0, 'X', HeaderSize + 5},
		"version":     {4, 2, HeaderSize + 5},
		"version 0":   {4, 0, HeaderSize + 5},
		"flags":       {5, 1, HeaderSize + 5},
		"reserved 6":  {6, 1, HeaderSize + 5},
		"reserved 7":  {7, 1, HeaderSize + 5},
		"reserved 56": {56, 1, HeaderSize + 5},
		"reserved 63": {63, 0x80, HeaderSize + 5},
		"file longer": {-1, 0, HeaderSize + 6},
		"file short":  {-1, 0, HeaderSize + 4},
		"no body":     {-1, 0, HeaderSize},
		"tiny file":   {-1, 0, 3},
	} {
		b := bytes.Clone(good)
		if c.at >= 0 {
			b[c.at] = c.val
		}
		if _, err := DecodeHeader(b, c.size); !errors.Is(err, ErrCorruptHeader) {
			t.Errorf("%s: err = %v, want ErrCorruptHeader", name, err)
		}
	}
	if _, err := DecodeHeader(good[:HeaderSize-1], HeaderSize+5); !errors.Is(err, ErrCorruptHeader) {
		t.Errorf("short header: err = %v", err)
	}
}
