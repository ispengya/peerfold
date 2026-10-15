package mount

import (
	"peerfold/internal/node"
)

// openFile is one open handle: the writes it has accumulated but not yet
// flushed, plus the size and kind it was opened with.
//
// Writes accumulate in memory because the wire has no partial write — a Put
// carries a whole file. So a handle can be dirty for its entire life and the
// flush happens at release, fsync, or before any operation that would move or
// delete the path underneath it.
type openFile struct {
	path string
	fh   uint64
	kind node.Kind
	size int64
	// writeLimit is copied from the Mount's options when the handle is opened,
	// not read from them later. A handle's cap is a property of the handle: if
	// it were read live, changing the option would change the cap of files
	// already open, and two handles could disagree about how much they may hold.
	//
	// What it bounds is the bytes held, not the bytes written. An editor that
	// rewrites the same region repeatedly writes far more bytes than the file
	// contains, and counting those would refuse an ordinary save on a file that
	// never grew.
	writeLimit int
	dirty      bool

	// writes holds the file's accumulated bytes as disjoint intervals keyed by
	// start offset. Disjointness is an invariant of splice, which merges each
	// arriving write into whatever it crosses so that the bytes either side of
	// it survive. content() can then scatter the intervals in any order.
	//
	// The offsets are absolute file offsets, so an interval at 1 MiB in a mostly
	// sparse file still costs only the bytes it holds -- but content() materialises
	// the whole file, so a write far into a large file costs that file's size in
	// one allocation. See the note on heldBytes.
	writes map[int64][]byte
}

// newHandleLocked allocates a handle for a path and records it in the table.
//
// The name says what the contract is: the caller must hold Mount.mu. It is not
// exported and every caller is in this package, so a compile error is the only
// enforcement available, and the name is what a reader has to go on.
func (m *Mount) newHandleLocked(path string, kind node.Kind) uint64 {
	m.nextFh++
	fh := m.nextFh
	m.files[fh] = &openFile{
		path:       path,
		fh:         fh,
		kind:       kind,
		writeLimit: m.opts.WriteLimit,
		writes:     map[int64][]byte{},
	}
	return fh
}

// handle returns the open handle for a number, if it is still open.
//
// The caller is expected to hold Mount.mu.
func (m *Mount) handle(fh uint64) (*openFile, bool) {
	h, ok := m.files[fh]
	return h, ok
}

// closeHandle forgets a handle and releases its cached chunks.
//
// The caller is expected to hold Mount.mu, which is why the cache is released
// through its Locked form: taking the cache's mutex while holding this one is
// the documented order, and the Locked form avoids re-entering it.
func (m *Mount) closeHandle(fh uint64) {
	delete(m.files, fh)
	m.cache.dropOpenLocked(fh)
}

// write records a write at off. It returns 0 or -ENOSPC; it never truncates.
//
// The refusal is the point of the function. The wire carries whole files, so
// this buffer is what will eventually be sent; dropping the tail of it to fit a
// limit would mean a file that saves without error and is quietly missing its
// end. Reporting ENOSPC instead makes the writer see the failure.
func (o *openFile) write(off int64, data []byte) int {
	if off < 0 {
		return -EINVAL
	}
	// The limit is checked before anything is recorded, so a refusal leaves the
	// previous content and size exactly as they were.
	if code := o.reserveFor(off, len(data)); code != 0 {
		return code
	}
	o.splice(off, data)
	if end := off + int64(len(data)); end > o.size {
		o.size = end
	}
	o.dirty = true
	return 0
}

// reserveFor reports whether a write of n bytes at off fits in the limit.
//
// The number compared is the footprint the handle would have after the write,
// not the bytes held plus the bytes written. A write landing on bytes the handle
// already holds replaces them rather than adding to them, so the naive sum would
// refuse an ordinary overwrite: an editor rewriting a 8 MiB file in place would
// hit ENOSPC on bytes it is replacing, not growing.
func (o *openFile) reserveFor(off int64, n int) int {
	if o.footprintAfterSplice(off, n) > o.writeLimit {
		return -ENOSPC
	}
	return 0
}

// footprintAfterSplice is how many bytes the handle would hold if a write of n
// bytes at off were applied.
//
// It is heldBytes plus the new bytes, less whatever of the new range already
// exists. The two ranges are subtracted piecewise rather than by whole intervals
// because a write may cover part of an interval at either end.
func (o *openFile) footprintAfterSplice(off int64, n int) int {
	start := off
	end := off + int64(n)
	overwritten := 0
	for s, d := range o.writes {
		// Intersection of [s, s+len(d)) with [start, end).
		lo, hi := s, s+int64(len(d))
		if lo < start {
			lo = start
		}
		if hi > end {
			hi = end
		}
		if hi > lo {
			overwritten += int(hi - lo)
		}
	}
	return o.heldBytes() + n - overwritten
}

// splice applies a write to the interval map, preserving every byte the write
// does not cover.
//
// The map holds non-overlapping intervals keyed by start offset, and a write is
// merged into it by splitting whatever it crosses. The bytes that matter are the
// ones on either side of the new write: a shorter write landing inside a longer
// one must leave the tails of the longer one in place, because that is what the
// platform does. Measured on this machine, write(0,"abcdef") then write(2,"XY")
// reads back "abXYef", with "ef" surviving from the first write.
//
// The obvious implementation -- keeping a run per write under its own offset and
// having content() copy each run up to its own length -- silently drops "ef",
// because a run has no way to say "preserve whatever extends past me". Storing
// intervals rather than per-call runs is what makes the correct answer the
// natural one: the write is applied to the file, not appended to a log of calls.
//
// A write must be contiguous over the interval it declares, so a caller that
// wants to leave a hole writes zeros for it rather than issuing two runs. That is
// what cgofuse does.
func (o *openFile) splice(off int64, data []byte) {
	start := off
	end := off + int64(len(data))
	next := make(map[int64][]byte, len(o.writes)+2)
	for s, d := range o.writes {
		e := s + int64(len(d))
		switch {
		case e <= start || s >= end:
			// Disjoint: keep it whole. Adjacency (e == start or s == end) also
			// lands here, which is right -- neighbouring intervals do not
			// overlap, and keeping them separate is harmless because content()
			// reproduces them in any order and they share no bytes.
			next[s] = d
		case s < start && e > end:
			// The write lands strictly inside this interval, so it splits it in
			// two. Both halves are copies: assigning subslices would leave two
			// map entries sharing one backing array, and a later write through
			// either could then be visible through the other.
			left := make([]byte, start-s)
			copy(left, d[:start-s])
			next[s] = left
			right := make([]byte, e-end)
			copy(right, d[end-s:])
			next[end] = right
		case s < start:
			// Overlaps the write's beginning and ends within it: keep the head.
			keep := make([]byte, start-s)
			copy(keep, d[:start-s])
			next[s] = keep
		case e > end:
			// Overlaps the write's end and starts within it: keep the tail, at
			// its own offset, which is where those bytes really live.
			keep := make([]byte, e-end)
			copy(keep, d[end-s:])
			next[end] = keep
		default:
			// Fully covered by the write, so those bytes are gone.
		}
	}
	next[start] = append([]byte(nil), data...)
	o.writes = next
}

// heldBytes is the number of bytes the handle holds, which is what writeLimit
// bounds.
//
// Counting the map rather than tracking a running total keeps the figure honest
// by construction: it is derived from the intervals that actually exist, so it
// cannot drift out of step with them. It is cheap because the interval map only
// ever holds a handful of entries -- one per disjoint piece a write has carved
// it into.
func (o *openFile) heldBytes() int {
	n := 0
	for _, d := range o.writes {
		n += len(d)
	}
	return n
}

// truncateTo sets the handle's size, dropping any held bytes past the new end.
func (o *openFile) truncateTo(size int64) int {
	if size < 0 {
		return -EINVAL
	}
	o.size = size
	// Everything at or past the new end stops existing, and a run that straddles
	// the end keeps only the part before it. The kept bytes must be copied: the
	// run's slice belongs to the map and reslicing it would leave the truncated
	// bytes reachable and mutable through the shorter slice's capacity.
	for off, data := range o.writes {
		if off >= size {
			delete(o.writes, off)
			continue
		}
		if off+int64(len(data)) > size {
			keep := make([]byte, size-off)
			copy(keep, data)
			o.writes[off] = keep
		}
	}
	o.dirty = true
	return 0
}

// content materialises the current bytes of the handle: the held intervals
// applied over a zero-filled buffer of the handle's size. Sparse regions are
// zeros, which is what a filesystem guarantees for a hole.
//
// Because splice keeps the intervals non-overlapping and keyed by start offset,
// this is a plain scatter and the map can be walked in any order. Nothing here
// has to resolve a conflict, which is the point: the merging happened when the
// write arrived, while the surrounding bytes were still available to preserve.
func (o *openFile) content() []byte {
	out := make([]byte, o.size)
	for off, data := range o.writes {
		if off >= int64(len(out)) {
			continue
		}
		copy(out[off:], data)
	}
	return out
}
