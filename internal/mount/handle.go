package mount

import (
	"bytes"
	"log"

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
	// readOnly records the mode the handle was opened with. cgofuse passes it at
	// open time and the kernel enforces it there too, but the mount is where the
	// mode is known, and a write to a read-only handle is refused with EBADF
	// rather than being allowed to quietly modify a file the caller only opened
	// to read.
	readOnly bool
	// contentSize is the size the index reported when the handle was opened.
	//
	// It is kept separately from size because size is a mutable record of the
	// content's current length, and the two answer different questions: size says
	// how long the handle's bytes are now, while contentSize says how long the
	// file was before this handle touched it. The truncate path needs the second
	// to know whether the handle's buffer actually holds the file.
	contentSize int64
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

	// version counts mutations of this handle: every write, every truncation.
	//
	// It exists so a flush can tell whether the content it uploaded is still the
	// content the handle holds. A flush cannot hold the mount's lock across a
	// whole-file upload -- that is a wire round trip of up to the call timeout --
	// so writes can arrive while the upload is in flight. Without a version, the
	// flush would clear the dirty flag on return and silently discard exactly
	// those writes, which is the lost-update bug this field prevents.
	version uint64

	// dirtyVersion is the handle's version as of the last successful flush. The
	// handle is clean exactly when version == dirtyVersion.
	dirtyVersion uint64
}

// markDirty records that the handle's content changed.
//
// Every mutation bumps the version, which is what makes "is this handle clean"
// answerable without a separate flag that could drift out of step with the
// content: the flag and the version are set together, in one place.
func (o *openFile) markDirty() {
	o.version++
	o.dirty = true
}

// isClean reports whether the handle's content is already on the owner.
//
// It is derived rather than stored, so it cannot disagree with the version that
// a flush compares. Setting o.dirty = false by hand, anywhere, would reintroduce
// the disagreement this avoids.
func (o *openFile) isClean() bool {
	return o.version == o.dirtyVersion
}

// markFlushed records that the given version has reached the owner.
//
// It refuses to clear the flag when the handle has moved on, which is the whole
// point: the upload that just finished carried an older body, so the handle is
// still dirty and a later flush must run. Returning false says the handle
// changed under the flush.
func (o *openFile) markFlushed(v uint64) bool {
	if o.version != v {
		return false
	}
	o.dirtyVersion = v
	o.dirty = false
	return true
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

// flush uploads one handle's whole buffer and marks it clean.
//
// It is idempotent, because Fsync, Flush and Release can all arrive for one
// handle and three whole-file uploads of a 40 MB file is a real cost.
//
// The mount's lock is NOT held across the upload. A Put is a wire round trip of
// up to the call timeout, and holding the lock across it would stop every other
// operation -- including the ones that would notice the connection is gone. The
// cost of releasing it is that writes can land while the upload is in flight, so
// the handle's version is captured with the content and compared afterwards: if
// it moved, the upload carried an older body and the handle stays dirty. Without
// that comparison the clear would discard exactly those writes, silently.
//
// A failed upload leaves the handle dirty for the same reason: the write did not
// land, and clearing the flag would discard the only copy of the content.
//
// A disconnected mount fails the upload with EIO. It is checked before anything
// else, so no round trip is attempted -- and it must be a *failure* rather than a
// silent success, because the caller is usually Release and a flush that claimed
// success while dropping the content is exactly the lost save this package
// exists to prevent.
//
// An unknown handle is a success, not an error. cgofuse can deliver Fsync or
// Flush for a handle the kernel has already released, and refusing would turn a
// harmless race into an input/output error on a save.
func (m *Mount) flush(fh uint64) int {
	m.mu.Lock()
	h, ok := m.files[fh]
	if !ok || h.isClean() {
		m.mu.Unlock()
		return 0
	}
	if m.isDisconnected {
		// The clean-handle case returned above, so this only ever refuses a
		// flush that really had content to send. That keeps the "an unknown or
		// clean handle is a success" promise intact: Release still frees the
		// handle, and a mount with nothing outstanding still unmounts cleanly.
		m.mu.Unlock()
		return -EIO
	}
	path, body, version := h.path, h.content(), h.version
	c := m.caller
	m.mu.Unlock()

	if err := c.Put(path, bytes.NewReader(body)); err != nil {
		return errnoFor(err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// The handle is looked up again rather than reused: it can have been released
	// and reallocated while the upload was in flight, and marking a different
	// handle clean would be worse than doing nothing.
	if h2, ok := m.files[fh]; ok {
		h2.markFlushed(version)
	}
	return 0
}

// flushPath uploads every dirty handle whose path is this one.
//
// It exists for the operations that replace or remove a path: a rename or an
// unlink has to get the content out before the name it was opened under
// disappears, and more than one handle can be open on the same path.
func (m *Mount) flushPath(path string) int {
	m.mu.Lock()
	fhs := make([]uint64, 0, len(m.files))
	for fh, h := range m.files {
		if h.path == path && !h.isClean() {
			fhs = append(fhs, fh)
		}
	}
	m.mu.Unlock()

	code := 0
	for _, fh := range fhs {
		if c := m.flush(fh); c != 0 && code == 0 {
			code = c
		}
	}
	return code
}

// flushAll uploads every dirty handle, and reports how many could not be.
//
// It is what an unmount or a lost connection runs. A count is returned rather
// than a bare errno because losing a connection can leave several handles with
// content that never landed, and the caller has to put that in the log: silence
// here is a save that vanished.
func (m *Mount) flushAll() int {
	m.mu.Lock()
	fhs := make([]uint64, 0, len(m.files))
	for fh, h := range m.files {
		if !h.isClean() {
			fhs = append(fhs, fh)
		}
	}
	m.mu.Unlock()

	failed := 0
	for _, fh := range fhs {
		if m.flush(fh) != 0 {
			failed++
		}
	}
	return failed
}

// flushAllHandles is flushAll for the unmount path, and it is the version that
// says what was lost.
//
// An unmount is a chance to save rather than a crash, so every dirty handle gets
// one final attempt. flushAll reports a count, which is the right shape for a
// caller that wants to decide something; this caller wants to tell the user, so it
// reports each failure by handle instead. Silence here would be an edit that
// vanished with no record of it.
func (m *Mount) flushAllHandles() {
	m.mu.Lock()
	fhs := make([]uint64, 0, len(m.files))
	for fh, h := range m.files {
		if !h.isClean() {
			fhs = append(fhs, fh)
		}
	}
	m.mu.Unlock()

	for _, fh := range fhs {
		if code := m.flush(fh); code != 0 {
			log.Printf("mount: flushing handle %d on unmount failed (%d); the "+
				"owner did not accept the last write", fh, code)
		}
	}
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
	o.markDirty()
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
	o.markDirty()
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
