package bucket

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Object header (ADR 0004, 0007): the first HeaderSize bytes of every file under objects/,
// followed by the body. All integers are big-endian; reserved bytes are zero.
//
//	offset size field
//	     0    4 magic "AZKO"
//	     4    1 version (HeaderVersion)
//	     5    1 flags (none defined in version 1: must be zero)
//	     6    2 reserved
//	     8    8 body size in bytes (uint64)
//	    16   32 SHA-256 of the body
//	    48    8 receive time, nanoseconds since the Unix epoch (int64)
//	    56    8 reserved
//
// A change to this layout is a new HeaderVersion; testdata/header-v1.bin pins it.
const (
	HeaderSize    = 64
	HeaderVersion = 1
	headerMagic   = "AZKO"
)

// ErrCorruptHeader is wrapped by every refusal of DecodeHeader.
var ErrCorruptHeader = errors.New("corrupt object header")

// Header is the decoded object header.
type Header struct {
	Size        uint64
	SHA256      [32]byte
	ReceiveTime int64 // ns since the Unix epoch, the store's clock (ADR 0007)
}

// Encode returns the header's HeaderSize bytes.
func (h Header) Encode() []byte {
	b := make([]byte, HeaderSize)
	copy(b[0:4], headerMagic)
	b[4] = HeaderVersion
	binary.BigEndian.PutUint64(b[8:16], h.Size)
	copy(b[16:48], h.SHA256[:])
	binary.BigEndian.PutUint64(b[48:56], uint64(h.ReceiveTime))
	return b
}

// DecodeHeader parses the start of an object file of fileSize bytes and refuses a header
// whose magic, version, flags or reserved bytes are wrong, or whose size disagrees with the file.
func DecodeHeader(b []byte, fileSize int64) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: %d bytes", ErrCorruptHeader, len(b))
	}
	b = b[:HeaderSize]
	switch {
	case string(b[0:4]) != headerMagic:
		return Header{}, fmt.Errorf("%w: magic %q", ErrCorruptHeader, b[0:4])
	case b[4] != HeaderVersion:
		return Header{}, fmt.Errorf("%w: version %d", ErrCorruptHeader, b[4])
	case b[5] != 0:
		return Header{}, fmt.Errorf("%w: flags %#x", ErrCorruptHeader, b[5])
	case !allZero(b[6:8]) || !allZero(b[56:64]):
		return Header{}, fmt.Errorf("%w: reserved bytes not zero", ErrCorruptHeader)
	}
	h := Header{Size: binary.BigEndian.Uint64(b[8:16]), ReceiveTime: int64(binary.BigEndian.Uint64(b[48:56]))}
	copy(h.SHA256[:], b[16:48])
	if fileSize < HeaderSize || h.Size != uint64(fileSize-HeaderSize) {
		return Header{}, fmt.Errorf("%w: size %d in a %d-byte file", ErrCorruptHeader, h.Size, fileSize)
	}
	return h, nil
}

func allZero(b []byte) bool { return len(bytes.Trim(b, "\x00")) == 0 }
