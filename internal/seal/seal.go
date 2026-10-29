// Package seal holds every key and sealing operation of azukari-backup (ADR 0006): the data key,
// its subkeys, chunk identifiers and nonces, the key check, the gear table and sealed objects.
// azukari-server and azukari never import it (ADR 0001, 0002).
package seal

import (
	"bytes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"
)

// Version is byte 0 of every sealed object.
const Version = 1

const (
	nonceSize = chacha20poly1305.NonceSizeX
	// Overhead is what sealing adds to a payload: version, nonce, compression byte, tag.
	Overhead = 1 + nonceSize + 1 + chacha20poly1305.Overhead

	compressNone = 0
	compressZstd = 1
)

var (
	// ErrInvalidKey is wrapped by every refusal of a key file.
	ErrInvalidKey = errors.New("data key invalid")
	// ErrCorrupt is wrapped by every refusal of a sealed object: version, tag, compression, identifier.
	ErrCorrupt = errors.New("sealed object corrupt")
)

// Key is a node's data key and its subkeys.
type Key struct {
	id, enc, nonce, gear, check []byte
	aead                        cipher.AEAD
}

// LoadKey reads a data key file (ADR 0011 data_key_file). Its mode is the caller's business.
func LoadKey(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseKey(b)
}

// ParseKey accepts exactly 64 hex characters (32 bytes), optionally followed by one "\n".
func ParseKey(b []byte) (*Key, error) {
	b = bytes.TrimSuffix(b, []byte("\n"))
	if len(b) != 64 {
		return nil, fmt.Errorf("%w: %d characters, want 64 hex and an optional newline", ErrInvalidKey, len(b))
	}
	master := make([]byte, 32)
	if _, err := hex.Decode(master, b); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	// Subkeys: HKDF-SHA-256, no salt (the master key is already 32 uniform bytes, RFC 5869 §3.1),
	// info = the ADR 0006 label, 32 bytes each.
	sub := func(label string) []byte {
		k, err := hkdf.Key(sha256.New, master, nil, label, 32)
		if err != nil {
			panic(err) // only for a length over 255*32
		}
		return k
	}
	enc := sub("azukari enc")
	aead, err := chacha20poly1305.NewX(enc)
	if err != nil {
		panic(err) // the key size is fixed
	}
	return &Key{
		id:    sub("azukari id"),
		enc:   enc,
		nonce: sub("azukari nonce"),
		gear:  sub("azukari gear"),
		check: sub("azukari check"),
		aead:  aead,
	}, nil
}

func mac(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

// ChunkID is HMAC-SHA-256 of a chunk's plaintext under the id subkey.
func (k *Key) ChunkID(plaintext []byte) [32]byte {
	return [32]byte(mac(k.id, plaintext))
}

// ChunkPath is a chunk's key without the node segment, "chunks/<2 hex>/<64 hex>": its aad.
func ChunkPath(id [32]byte) string {
	h := hex.EncodeToString(id[:])
	return "chunks/" + h[:2] + "/" + h
}

// chunkNonce is the first 24 bytes of HMAC-SHA-256 of the identifier under the nonce subkey.
func (k *Key) chunkNonce(id [32]byte) [nonceSize]byte {
	return [nonceSize]byte(mac(k.nonce, id[:]))
}

// KeyCheck is the hex of HMAC-SHA-256 of "azukari" under the check subkey.
func (k *Key) KeyCheck() string {
	return hex.EncodeToString(mac(k.check, []byte("azukari")))
}

// Config is the exact bytes of the <node>/config object, no trailing newline.
func (k *Key) Config() []byte {
	return []byte(`{"format":1,"key_check":"` + k.KeyCheck() + `"}`)
}

// GearTable is FastCDC's table: HKDF-Expand-SHA-256 with the gear subkey as PRK and an empty
// info, 2048 bytes read as 256 little-endian uint64.
func (k *Key) GearTable() [256]uint64 {
	b, err := hkdf.Expand(sha256.New, k.gear, "", 256*8)
	if err != nil {
		panic(err)
	}
	var t [256]uint64
	for i := range t {
		t[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return t
}

// Level and single concurrency fixed: the sealed bytes of a chunk are a format vector (ADR 0010).
var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	decoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
)

// SealChunk returns a chunk's identifier and sealed bytes; the same plaintext always seals to
// the same bytes under one key.
func (k *Key) SealChunk(plaintext []byte) (id [32]byte, sealed []byte) {
	id = k.ChunkID(plaintext)
	return id, k.seal(ChunkPath(id), k.chunkNonce(id), plaintext)
}

// OpenChunk opens a chunk sealed under id and checks its plaintext hashes to id.
func (k *Key) OpenChunk(id [32]byte, sealed []byte) ([]byte, error) {
	p, err := k.open(ChunkPath(id), sealed)
	if err != nil {
		return nil, err
	}
	if got := k.ChunkID(p); !hmac.Equal(got[:], id[:]) {
		return nil, fmt.Errorf("%w: plaintext does not match identifier", ErrCorrupt)
	}
	return p, nil
}

// SealRoot seals a snapshot root named name (aad "snapshots/<name>") with a random nonce: the
// caller keeps the result for retries.
func (k *Key) SealRoot(name string, plaintext []byte) []byte {
	var n [nonceSize]byte
	rand.Read(n[:]) // never fails since Go 1.24
	return k.seal("snapshots/"+name, n, plaintext)
}

// OpenRoot opens a snapshot root named name.
func (k *Key) OpenRoot(name string, sealed []byte) ([]byte, error) {
	return k.open("snapshots/"+name, sealed)
}

func (k *Key) seal(aad string, nonce [nonceSize]byte, plaintext []byte) []byte {
	inner := append([]byte{compressNone}, plaintext...)
	if z := encoder.EncodeAll(plaintext, []byte{compressZstd}); len(z) < len(inner) {
		inner = z
	}
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(inner)+chacha20poly1305.Overhead)
	out[0] = Version
	copy(out[1:], nonce[:])
	return k.aead.Seal(out, nonce[:], inner, []byte(aad))
}

func (k *Key) open(aad string, sealed []byte) ([]byte, error) {
	if len(sealed) < Overhead {
		return nil, fmt.Errorf("%w: %d bytes", ErrCorrupt, len(sealed))
	}
	if sealed[0] != Version {
		return nil, fmt.Errorf("%w: version %d", ErrCorrupt, sealed[0])
	}
	inner, err := k.aead.Open(nil, sealed[1:1+nonceSize], sealed[1+nonceSize:], []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: authentication failed", ErrCorrupt, aad)
	}
	switch inner[0] {
	case compressNone:
		return inner[1:], nil
	case compressZstd:
		p, err := decoder.DecodeAll(inner[1:], nil)
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
		}
		return p, nil
	default:
		return nil, fmt.Errorf("%w: compression %d", ErrCorrupt, inner[0])
	}
}
