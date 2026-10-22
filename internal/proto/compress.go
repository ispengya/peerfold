package proto

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"sync"
)

// minCompressSize is the smallest content worth compressing.
//
// Below it the flate stream header and the CPU cost are a larger share of the
// frame than the bytes saved. Content above it is almost always a file chunk or
// a listing body, where the trade is clear.
const minCompressSize = 1 << 10

// compressLevel is deliberately the fastest one. The link, not the CPU, is the
// bottleneck this exists for: a 1 MiB frame at this level compresses many times
// faster than even a fast network can carry it, and the ratio on source code and
// JSON is still several times.
const compressLevel = flate.BestSpeed

// flateWriters keeps the allocator out of the data path. flate.Writer holds a
// sizeable window; creating one per frame would cost more than compressing the
// frame.
var flateWriters = sync.Pool{
	New: func() any {
		w, err := flate.NewWriter(io.Discard, compressLevel)
		if err != nil {
			panic("proto: flate level is invalid: " + err.Error())
		}
		return w
	},
}

// compressChunk returns src compressed, or false when compressing it does not
// make the frame smaller.
//
// The false result is the whole non-text story. A JPEG, a zip, or an already
// compressed video frame compresses to about its own size; sending it raw keeps
// the bandwidth where it was. The only cost is one compression attempt, which
// at these rates is far below the transfer itself.
func compressChunk(src []byte) ([]byte, bool) {
	var buf bytes.Buffer
	buf.Grow(len(src)/2 + 64)

	w := flateWriters.Get().(*flate.Writer)
	w.Reset(&buf)
	_, err := w.Write(src)
	if err == nil {
		err = w.Close()
	}
	flateWriters.Put(w)
	if err != nil || buf.Len() >= len(src) {
		return nil, false
	}
	return buf.Bytes(), true
}

// decompressChunk inflates one compressed chunk, refusing to produce more than
// max bytes.
//
// The limit is not optional: a compressed frame is attacker-reachable data, and
// a small frame can otherwise expand to an arbitrarily large allocation. The
// sender is never allowed to put more than MaxChunk of content in one frame, so
// the same bound applies here.
func decompressChunk(src []byte, max int) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(src))
	defer r.Close()

	out, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(out) > max {
		return nil, fmt.Errorf("proto: compressed chunk expands past %d bytes", max)
	}
	return out, nil
}
