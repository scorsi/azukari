package fastcdc

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

var update = flag.Bool("update", false, "rewrite testdata/boundaries.txt (a format change)")

// splitmix64: a fixed generator, so the test table and input never depend on math/rand.
type splitmix uint64

func (s *splitmix) next() uint64 {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return z ^ z>>31
}

func testGear() (g [256]uint64) {
	s := splitmix(1)
	for i := range g {
		g[i] = s.next()
	}
	return g
}

func testInput(size int) []byte {
	s := splitmix(2)
	b := make([]byte, size+7)
	for i := 0; i < size; i += 8 {
		v := s.next()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return b[:size]
}

// boundaries returns the end offset of every chunk of r.
func boundaries(t *testing.T, r io.Reader) []int {
	t.Helper()
	c := New(r, testGear())
	var out []int
	off := 0
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		off += len(chunk)
		out = append(out, off)
	}
}

const inputSize = 32<<20 + 12345

func TestVectors(t *testing.T) {
	got := boundaries(t, bytes.NewReader(testInput(inputSize)))
	var text strings.Builder
	for _, b := range got {
		fmt.Fprintln(&text, b)
	}
	const path = "testdata/boundaries.txt"
	if *update {
		if err := os.WriteFile(path, []byte(text.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != string(want) {
		t.Fatalf("boundaries changed: a format change (ADR 0006)\ngot:\n%s", text.String())
	}
}

func TestProperties(t *testing.T) {
	in := testInput(inputSize)
	c := New(iotest.HalfReader(bytes.NewReader(in)), testGear()) // short reads change nothing
	var out []byte
	var ends []int
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk...)
		ends = append(ends, len(out))
		if len(chunk) > MaxSize || len(chunk) == 0 {
			t.Fatalf("chunk of %d bytes", len(chunk))
		}
		if len(chunk) < MinSize && len(out) != len(in) {
			t.Fatalf("chunk of %d bytes before the end", len(chunk))
		}
	}
	if !bytes.Equal(out, in) {
		t.Fatal("concatenation differs from the input")
	}
	if want := boundaries(t, bytes.NewReader(in)); !slices.Equal(ends, want) {
		t.Fatal("boundaries depend on read sizes")
	}
	if n := len(ends); n < 16 || n > 64 {
		t.Fatalf("%d chunks for 32 MiB, want about 32", n)
	}
}

func TestInsertionKeepsBoundaries(t *testing.T) {
	in := testInput(inputSize)
	const at, extra = 16 << 20, 100
	edited := slices.Concat(in[:at], bytes.Repeat([]byte{'x'}, extra), in[at:])
	before := boundaries(t, bytes.NewReader(in))
	after := boundaries(t, bytes.NewReader(edited))

	var lost int
	for _, b := range before {
		shifted := b
		if b > at {
			shifted += extra
		}
		if !slices.Contains(after, shifted) {
			if b <= at {
				t.Fatalf("boundary %d before the insertion moved", b)
			}
			lost++
		}
	}
	if lost > 2 {
		t.Fatalf("%d boundaries lost after a %d-byte insertion", lost, extra)
	}
}

func TestEmpty(t *testing.T) {
	if got := boundaries(t, bytes.NewReader(nil)); len(got) != 0 {
		t.Fatalf("empty input gave chunks %v", got)
	}
}

func TestSmallInputIsOneChunk(t *testing.T) {
	if got := boundaries(t, bytes.NewReader(testInput(1000))); !slices.Equal(got, []int{1000}) {
		t.Fatalf("got %v", got)
	}
}

func TestReadError(t *testing.T) {
	boom := errors.New("boom")
	_, err := New(iotest.ErrReader(boom), testGear()).Next()
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
