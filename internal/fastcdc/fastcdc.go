// Package fastcdc splits a stream into content-defined chunks (ADR 0006): FastCDC, Xia et al.
// 2016, with normalized chunking. Every choice below is part of the backup format: a change
// moves chunk boundaries, costing deduplication against older snapshots, and is a format
// version (the vectors in testdata/ fail first).
//
// The exact algorithm, for a chunk starting at offset 0 of the remaining data d:
//
//   - Gear hash: fp = fp<<1 + gear[b] (uint64, wrapping), fp = 0 at the chunk's start.
//   - The first MinSize bytes are never hashed: a chunk is at least MinSize bytes, unless it
//     is the last one of the stream.
//   - Bytes d[MinSize : AvgSize] are tested against maskS, the top 22 bits of fp (log2(AvgSize)
//   - 2: harder to match). Bytes d[AvgSize : MaxSize] are tested against maskL, the top 18
//     bits (log2(AvgSize) - 2: easier). This is normalization level 2. The top bits are used
//     because bit k of a shifted gear hash depends on the last k+1 bytes only: the top 22 bits
//     cover a 64-byte window.
//   - After hashing byte d[i], fp&mask == 0 cuts the chunk as d[:i+1].
//   - No match before MaxSize: the chunk is d[:MaxSize]. The stream's end: the rest is the
//     last chunk, whatever its size.
package fastcdc

import (
	"errors"
	"io"
)

// Chunk size bounds, in bytes (ADR 0006).
const (
	MinSize = 256 << 10
	AvgSize = 1 << 20
	MaxSize = 4 << 20
)

const (
	maskS = uint64(1<<22-1) << (64 - 22)
	maskL = uint64(1<<18-1) << (64 - 18)
)

// cut returns the length of the chunk that starts d, d being the whole rest of the stream
// or at least MaxSize bytes of it.
func cut(d []byte, gear *[256]uint64) int {
	n := min(len(d), MaxSize)
	if n <= MinSize {
		return n
	}
	normal := min(n, AvgSize)
	var fp uint64
	i := MinSize
	for ; i < normal; i++ {
		fp = fp<<1 + gear[d[i]]
		if fp&maskS == 0 {
			return i + 1
		}
	}
	for ; i < n; i++ {
		fp = fp<<1 + gear[d[i]]
		if fp&maskL == 0 {
			return i + 1
		}
	}
	return n
}

// Chunker reads a stream and yields its chunks in order, holding at most MaxSize bytes.
type Chunker struct {
	r          io.Reader
	gear       [256]uint64
	buf        []byte
	start, end int // buf[start:end] is read and not yet yielded
	eof        bool
}

// New chunks r with the gear table (derived from the node's key in internal/seal).
func New(r io.Reader, gear [256]uint64) *Chunker {
	return &Chunker{r: r, gear: gear, buf: make([]byte, MaxSize)}
}

// Next returns the next chunk, or io.EOF after the last one (at once for an empty stream).
// The chunk is only valid until the next call. A read error is returned as is.
func (c *Chunker) Next() ([]byte, error) {
	if !c.eof && c.end-c.start < MaxSize {
		c.end = copy(c.buf, c.buf[c.start:c.end])
		c.start = 0
		n, err := io.ReadFull(c.r, c.buf[c.end:])
		c.end += n
		switch {
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			c.eof = true
		case err != nil:
			return nil, err
		}
	}
	if c.start == c.end {
		return nil, io.EOF
	}
	n := cut(c.buf[c.start:c.end], &c.gear)
	chunk := c.buf[c.start : c.start+n]
	c.start += n
	return chunk, nil
}
