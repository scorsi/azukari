package seal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// testKeyHex is the fixed test key of the vectors: bytes 0x00..0x1f.
const testKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func testKey(t *testing.T) *Key {
	t.Helper()
	k, err := ParseKey([]byte(testKeyHex + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Test plaintexts: one that zstd shrinks, one it cannot (SHA-256 counter stream).
func compressible() []byte { return bytes.Repeat([]byte("azukari "), 512) }

func incompressible() []byte {
	var b []byte
	for i := uint64(0); i < 8; i++ {
		s := sha256.Sum256(binary.LittleEndian.AppendUint64(nil, i))
		b = append(b, s[:]...)
	}
	return b
}

type chunkVector struct {
	Plaintext string `json:"plaintext"` // hex
	ID        string `json:"id"`
	Nonce     string `json:"nonce"`
	Sealed    string `json:"sealed"`
}

type vectors struct {
	Key       string                 `json:"key"`
	Subkeys   map[string]string      `json:"subkeys"`
	KeyCheck  string                 `json:"key_check"`
	Config    string                 `json:"config"`
	GearTable string                 `json:"gear_table_sha256"` // of the 256 uint64, little-endian
	Chunks    map[string]chunkVector `json:"chunks"`
}

func compute(k *Key) vectors {
	v := vectors{
		Key: testKeyHex,
		Subkeys: map[string]string{
			"azukari id":    hex.EncodeToString(k.id),
			"azukari enc":   hex.EncodeToString(k.enc),
			"azukari nonce": hex.EncodeToString(k.nonce),
			"azukari gear":  hex.EncodeToString(k.gear),
			"azukari check": hex.EncodeToString(k.check),
		},
		KeyCheck: k.KeyCheck(),
		Config:   string(k.Config()),
		Chunks:   map[string]chunkVector{},
	}
	t := k.GearTable()
	var b []byte
	for _, x := range t {
		b = binary.LittleEndian.AppendUint64(b, x)
	}
	s := sha256.Sum256(b)
	v.GearTable = hex.EncodeToString(s[:])
	for name, p := range map[string][]byte{"empty": {}, "compressible": compressible(), "incompressible": incompressible()} {
		id, sealed := k.SealChunk(p)
		n := k.chunkNonce(id)
		v.Chunks[name] = chunkVector{
			Plaintext: hex.EncodeToString(p),
			ID:        hex.EncodeToString(id[:]),
			Nonce:     hex.EncodeToString(n[:]),
			Sealed:    hex.EncodeToString(sealed),
		}
	}
	return v
}

// TestVectors fails on any change of the format's output: such a change is a format version
// (ADR 0006, 0010), never a regenerated testdata file.
func TestVectors(t *testing.T) {
	k := testKey(t)
	got, err := json.MarshalIndent(compute(k), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile("testdata/vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("vectors changed (a format version, ADR 0006):\n%s", got)
	}

	// The stored sealed bytes still open, independently of the encoder.
	var v vectors
	if err := json.Unmarshal(want, &v); err != nil {
		t.Fatal(err)
	}
	for name, c := range v.Chunks {
		id, _ := hex.DecodeString(c.ID)
		sealed, _ := hex.DecodeString(c.Sealed)
		p, err := k.OpenChunk([32]byte(id), sealed)
		if err != nil || hex.EncodeToString(p) != c.Plaintext {
			t.Errorf("%s: open = %x, %v", name, p, err)
		}
	}
	if c := v.Chunks["compressible"]; len(c.Sealed)/2 >= len(c.Plaintext)/2 {
		t.Errorf("compressible chunk not compressed")
	}
	if c := v.Chunks["incompressible"]; len(c.Sealed)/2 != len(c.Plaintext)/2+Overhead {
		t.Errorf("incompressible chunk: %d sealed bytes for %d", len(c.Sealed)/2, len(c.Plaintext)/2)
	}
}

func TestParseKey(t *testing.T) {
	for _, s := range []string{testKeyHex, testKeyHex + "\n", strings.ToUpper(testKeyHex)} {
		if _, err := ParseKey([]byte(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{
		"", "\n", testKeyHex[:62], testKeyHex + "00", testKeyHex + "\n\n", testKeyHex + "\r\n",
		testKeyHex + " ", " " + testKeyHex, "\n" + testKeyHex, "zz" + testKeyHex[2:],
	} {
		if _, err := ParseKey([]byte(s)); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%q: err = %v, want ErrInvalidKey", s, err)
		}
	}
}

func TestOpenRefusals(t *testing.T) {
	k := testKey(t)
	p := compressible()
	id, sealed := k.SealChunk(p)
	otherID := k.ChunkID(incompressible())

	flip := func(i int) []byte { b := bytes.Clone(sealed); b[i] ^= 1; return b }
	version := bytes.Clone(sealed)
	version[0] = 2
	// Authentic for otherID's aad but holding p: only the re-hash catches it.
	wrongPlain := k.seal(ChunkPath(otherID), k.chunkNonce(otherID), p)

	for name, c := range map[string]struct {
		id     [32]byte
		sealed []byte
	}{
		"version":    {id, version},
		"nonce":      {id, flip(1)},
		"body":       {id, flip(len(sealed) - 20)},
		"tag":        {id, flip(len(sealed) - 1)},
		"short":      {id, sealed[:Overhead-1]},
		"swapped":    {otherID, sealed},
		"truncated":  {id, sealed[:len(sealed)-1]},
		"identifier": {otherID, wrongPlain},
	} {
		if _, err := k.OpenChunk(c.id, c.sealed); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}

	k2, _ := ParseKey([]byte(strings.Repeat("ab", 32)))
	if _, err := k2.OpenChunk(id, sealed); !errors.Is(err, ErrCorrupt) {
		t.Errorf("other key: err = %v", err)
	}
	// Authentic, but an unknown compression byte.
	var n [24]byte
	bad := k.aead.Seal(append([]byte{Version}, n[:]...), n[:], []byte{2, 'x'}, []byte(ChunkPath(id)))
	if _, err := k.OpenChunk(id, bad); !errors.Is(err, ErrCorrupt) {
		t.Errorf("compression: err = %v", err)
	}
}

func TestRoot(t *testing.T) {
	k := testKey(t)
	p := []byte("root bytes")
	a, b := k.SealRoot("20261029T120000Z-0011aabb", p), k.SealRoot("20261029T120000Z-0011aabb", p)
	if bytes.Equal(a, b) {
		t.Error("two root seals share a nonce")
	}
	if got, err := k.OpenRoot("20261029T120000Z-0011aabb", a); err != nil || !bytes.Equal(got, p) {
		t.Errorf("open = %q, %v", got, err)
	}
	if _, err := k.OpenRoot("20261029T120000Z-0011aabc", a); !errors.Is(err, ErrCorrupt) {
		t.Errorf("other name: err = %v", err)
	}
}
