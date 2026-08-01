package main

import (
	"hash"
	"io"
)

// The trailer used to carry a running CRC-32 checksum, computed while the
// body streams and only known once the last byte has been read - exactly
// what HTTP trailers exist for, so no separate request or in-memory
// buffering is needed.
const crc32TrailerHeader = "X-Crc32"

// crcTeeReader wraps an io.Reader, feeding every byte read into a running
// CRC-32 checksum, and invokes onEOF exactly once with the final sum as
// soon as the underlying reader reports io.EOF - which happens while the
// HTTP client is still in the middle of writing the request, in time for
// the computed value to go out as a trailer.
type crcTeeReader struct {
	r     io.Reader
	crc   hash.Hash32
	onEOF func(sum uint32)
	done  bool
}

func (t *crcTeeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.crc.Write(p[:n])
	}
	if err == io.EOF && !t.done {
		t.done = true
		if t.onEOF != nil {
			t.onEOF(t.crc.Sum32())
		}
	}
	return n, err
}
