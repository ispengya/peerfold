package mount

import (
	"strings"
	"testing"

	"peerfold/internal/node"
)

// OReadOnly and OReadWrite are cgofuse's flag values, declared here because the
// core does not import the library. These assertions are the point of declaring
// them: if the library's numbers ever differ, the core's meaning of "writable"
// silently changes, and the failure would show up as writes reaching a
// read-only open rather than as a compile error.
func TestOpenFlagValuesMatchTheFuseConstants(t *testing.T) {
	if OReadOnly != 0 {
		t.Errorf("OReadOnly = %d, want 0", OReadOnly)
	}
	if OReadWrite != 2 {
		t.Errorf("OReadWrite = %d, want 2", OReadWrite)
	}
	// The distinction the core relies on is only ever "is the write bit set",
	// so the values must not share it accidentally at the low end.
	if OReadOnly&OReadWrite != 0 {
		t.Error("OReadOnly and OReadWrite share a bit")
	}
}

func TestOpenMissingIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if _, code := m.Open("/nope", OReadOnly); code != -ENOENT {
		t.Errorf("Open(missing) = %d, want -ENOENT", code)
	}
}

func TestOpenSymlinkIsElooped(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addSymlink("/link")
	if _, code := m.Open("/link", OReadOnly); code != -ELOOP {
		t.Errorf("Open(symlink) = %d, want -ELOOP", code)
	}
}

// A relative or escaping path must be refused rather than resolved against
// something unintended. The kernel never sends one, but a refusal is cheap and
// the alternative is a path that means whatever Clean decides it means.
func TestOpenRejectsABadPath(t *testing.T) {
	m := newTestMount(t)
	for _, p := range []string{"relative", "/../escape"} {
		if _, code := m.Open(p, OReadOnly); code != -EINVAL {
			t.Errorf("Open(%q) = %d, want -EINVAL", p, code)
		}
	}
}

func TestOpenDirectoryAsFileIsEisdir(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")
	// Opening a directory for writing is the case editors hit when they try to
	// save over a directory.
	if _, code := m.Open("/d", OReadWrite); code != -EISDIR {
		t.Errorf("Open(dir, write) = %d, want -EISDIR", code)
	}
	// Reading a directory as a file is EISDIR too; Opendir is the other path.
	if _, code := m.Open("/d", OReadOnly); code != -EISDIR {
		t.Errorf("Open(dir, read) = %d, want -EISDIR", code)
	}
}

// Every open must get a distinct handle, and the handle must remember the path
// it was opened for. Reusing a number would let a stale reference from the
// kernel touch a different file.
func TestOpenAllocatesDistinctHandles(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.addFile("/b.txt", 1)

	fa, code := m.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open(a) = %d", code)
	}
	fb, code := m.Open("/b.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open(b) = %d", code)
	}
	if fa == fb {
		t.Fatalf("both opens got handle %d", fa)
	}
	if fa == 0 || fb == 0 {
		t.Errorf("handles are %d and %d; 0 is what cgofuse passes when there was no open", fa, fb)
	}
	ha, _ := m.handle(fa)
	hb, _ := m.handle(fb)
	if ha.path != "/a.txt" || hb.path != "/b.txt" {
		t.Errorf("handles remember %q and %q, want /a.txt and /b.txt", ha.path, hb.path)
	}
	// The size must be seeded from the index so a read of a file that was never
	// written still knows where the end is.
	if ha.size != 1 || hb.size != 1 {
		t.Errorf("seeded sizes are %d and %d, want 1", ha.size, hb.size)
	}
}

func TestOpendirOnFile(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if _, code := m.Opendir("/a.txt"); code != -ENOTDIR {
		t.Errorf("Opendir(file) = %d, want -ENOTDIR", code)
	}
}

func TestOpendirMissing(t *testing.T) {
	m := newTestMount(t)
	if _, code := m.Opendir("/nope"); code != -ENOENT {
		t.Errorf("Opendir(missing) = %d, want -ENOENT", code)
	}
}

func TestOpendirThenReaddir(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addFile("/d/a.txt", 1)
	fh, code := m.Opendir("/d")
	if code != 0 {
		t.Fatalf("Opendir = %d", code)
	}
	var seen int
	if code := m.Readdir("/d", func(string, *Stat, int64) bool { seen++; return true }, 0, fh); code != 0 {
		t.Errorf("Readdir = %d", code)
	}
	if seen == 0 {
		t.Error("Readdir saw nothing after Opendir")
	}
}

func TestReadWholeSmallFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, code := m.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	buf := make([]byte, 64)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 5 {
		t.Fatalf("Read = %d, want 5", n)
	}
	if string(buf[:n]) != "hello" {
		t.Errorf("content = %q", buf[:n])
	}
}

// The kernel reads at a 4 KiB offset into a small file and expects 0, not an
// error. This is the read-ahead that every real filesystem loop does.
func TestReadPastEndIsZero(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	buf := make([]byte, 16)
	if n := m.Read("/a.txt", buf, 4096, fh); n != 0 {
		t.Errorf("Read past EOF = %d, want 0", n)
	}
}

// A buffer that exactly fills a chunk which is not the last one must return that
// chunk and stop. This is the boundary where a "the buffer is full" terminator
// would be needed if the loop could ever revisit the same leftover bytes: the
// next iteration starts at the following chunk, so it cannot, and this test pins
// that reasoning.
func TestReadWithABufferExactlyOneChunk(t *testing.T) {
	m := newTestMount(t)
	m.setCacheForTest(newChunkCache(chunkSize*4, chunkSize*8))

	body := strings.Repeat("A", chunkSize) + strings.Repeat("B", chunkSize)
	f := fakeOf(t, m)
	f.addFile("/big", len(body))
	f.setContent("/big", []byte(body))

	fh, _ := m.Open("/big", OReadOnly)
	buf := make([]byte, chunkSize)
	n := m.Read("/big", buf, 0, fh)
	if n != chunkSize {
		t.Fatalf("Read = %d, want exactly %d", n, chunkSize)
	}
	if buf[0] != 'A' || buf[n-1] != 'A' {
		t.Errorf("the buffer holds the wrong chunk: first %q last %q", buf[0], buf[n-1])
	}
	// The second chunk must come from a second read at that offset, not from the
	// first one overrunning.
	n = m.Read("/big", buf, chunkSize, fh)
	if n != chunkSize {
		t.Fatalf("second Read = %d, want %d", n, chunkSize)
	}
	if buf[0] != 'B' || buf[n-1] != 'B' {
		t.Errorf("the second chunk is wrong: first %q last %q", buf[0], buf[n-1])
	}
}

// A read of exactly the last byte must return that byte and no error. It is the
// boundary the past-the-end case is one step away from.
func TestReadAtTheLastByte(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	buf := make([]byte, 16)
	n := m.Read("/a.txt", buf, 4, fh)
	if n != 1 || string(buf[:n]) != "o" {
		t.Errorf("Read at offset 4 = %q (%d bytes), want %q", buf[:n], n, "o")
	}
}

// A read that reaches exactly the end returns the bytes it asked for, not an
// error and not a short count.
func TestReadToTheExactEnd(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	buf := make([]byte, 5)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Errorf("Read = %q (%d bytes), want hello", buf[:n], n)
	}
}

// A zero-length buffer is a success with nothing read, not an error. The kernel
// does issue these.
func TestReadEmptyBufferIsZero(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	if n := m.Read("/a.txt", nil, 0, fh); n != 0 {
		t.Errorf("Read with an empty buffer = %d, want 0", n)
	}
}

func TestReadRefusesANegativeOffset(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	if n := m.Read("/a.txt", make([]byte, 4), -1, fh); n != -EINVAL {
		t.Errorf("Read at a negative offset = %d, want -EINVAL", n)
	}
}

// A read that spans more than one chunk must stitch them together, because the
// caller asked for one buffer and the wire answers in chunkSize pieces.
//
// The request starts ten bytes before the boundary, so the seam is in the middle
// of the buffer. Testing at the boundary rather than in the middle is what makes
// the test able to fail: a seam in the middle of one chunk proves nothing.
func TestReadAcrossChunks(t *testing.T) {
	m := newTestMount(t)
	m.setCacheForTest(newChunkCache(chunkSize*4, chunkSize*8))

	body := strings.Repeat("A", chunkSize) + strings.Repeat("B", chunkSize)
	f := fakeOf(t, m)
	f.addFile("/big", len(body))
	f.setContent("/big", []byte(body))

	fh, _ := m.Open("/big", OReadOnly)
	buf := make([]byte, chunkSize+10)
	n := m.Read("/big", buf, chunkSize-10, fh)
	if n != len(buf) {
		t.Fatalf("Read = %d, want %d", n, len(buf))
	}
	want := strings.Repeat("A", 10) + strings.Repeat("B", chunkSize)
	if string(buf) != want {
		t.Errorf("stitched content is wrong at the seam")
	}
}

// Reading a large file in one request must reach every byte in order, with the
// chunk boundaries invisible. This catches an off-by-one in the chunk index that
// a two-chunk test at one offset could miss.
func TestReadWholeMultiChunkFileInOneRequest(t *testing.T) {
	m := newTestMount(t)
	m.setCacheForTest(newChunkCache(chunkSize*4, chunkSize*8))

	// Three and a bit chunks, so the last read is short.
	body := make([]byte, chunkSize*3+7)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	f := fakeOf(t, m)
	f.addFile("/big", len(body))
	f.setContent("/big", body)

	fh, _ := m.Open("/big", OReadOnly)
	buf := make([]byte, len(body)+100) // deliberately larger than the file
	n := m.Read("/big", buf, 0, fh)
	if n != len(body) {
		t.Fatalf("Read = %d, want %d", n, len(body))
	}
	if string(buf[:n]) != string(body) {
		t.Errorf("the stitched content differs from the file")
	}
}

// The second read of the same range must be served from the cache, or every
// sequential scan re-fetches from the owner.
func TestReadIsCached(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	buf := make([]byte, 5)
	if n := m.Read("/a.txt", buf, 0, fh); n != 5 {
		t.Fatalf("first read = %d", n)
	}
	before := len(f.callLog())
	if n := m.Read("/a.txt", buf, 0, fh); n != 5 {
		t.Fatalf("second read = %d", n)
	}
	for _, c := range f.callLog()[before:] {
		if strings.HasPrefix(c, "Read ") {
			t.Errorf("cached read still hit the wire: %v", f.callLog()[before:])
		}
	}
}

// A write through the handle must be visible to a subsequent read of the same
// handle without any flush: programs write and then read back.
func TestReadSeesOwnWrites(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	if code := h.write(0, []byte("HELLO")); code != 0 {
		t.Fatalf("write = %d", code)
	}
	buf := make([]byte, 5)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 5 || string(buf[:n]) != "HELLO" {
		t.Errorf("read after write = %q (%d bytes), want HELLO", buf[:n], n)
	}
}

// A dirty read must obey the same end-of-file and offset rules as a clean one:
// a partial read of a written region, past the old end, must come from the
// handle rather than from the owner's shorter copy.
func TestReadOfDirtyHandlePastTheOldEnd(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	if code := h.write(0, []byte("hello world")); code != 0 {
		t.Fatalf("write = %d", code)
	}
	// Bytes 5..11 exist only in the handle. The owner still has 5 bytes.
	buf := make([]byte, 16)
	n := m.Read("/a.txt", buf, 5, fh)
	if n != 6 || string(buf[:n]) != " world" {
		t.Errorf("read past the old end = %q (%d bytes), want %q", buf[:n], n, " world")
	}
	// And past the new end is still 0.
	if n := m.Read("/a.txt", buf, 11, fh); n != 0 {
		t.Errorf("read past the new end = %d, want 0", n)
	}
}

// A read of a dirty handle must not touch the wire at all: the whole point is
// that the handle's own bytes win.
func TestReadOfDirtyHandleDoesNotHitTheWire(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	h.write(0, []byte("HELLO"))
	f.resetCalls()

	buf := make([]byte, 5)
	m.Read("/a.txt", buf, 0, fh)
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "Read ") {
			t.Errorf("a dirty read reached the wire: %v", f.callLog())
		}
	}
}

func TestReadUnknownHandleFallsBackToPath(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	// fh 0 is what cgofuse passes when the kernel read without an open.
	buf := make([]byte, 5)
	n := m.Read("/a.txt", buf, 0, 0)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Errorf("read with fh 0 = %q (%d)", buf[:n], n)
	}
}

func TestReadMissingPathIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if n := m.Read("/nope", make([]byte, 4), 0, 0); n != -ENOENT {
		t.Errorf("Read(missing) = %d, want -ENOENT", n)
	}
}

// A read whose path must be resolved because there is no usable handle must
// report the mapped error, not a short read. An offline mount reported as
// end-of-file is the failure mode this prevents: every file would look truncated.
//
// The read is done with no handle on purpose. A handle carries the path it was
// opened for, so the read does not need to resolve anything, and injecting a
// resolution failure would not be reached. That is exactly why the two cases are
// separate: only the handle-less read can report a resolution failure at all.
func TestReadMapsAFailingCaller(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	f.listErr = errOffline
	if n := m.Read("/a.txt", make([]byte, 5), 0, 0); n != -EIO {
		t.Errorf("Read with a failing caller = %d, want -EIO", n)
	}
}

// A read through an open handle does not resolve the path, so a lookup failure
// cannot affect it. This pins the difference: the handle's path was resolved when
// it was opened, and the read trusts it.
func TestReadThroughAHandleNeedsNoLookup(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, code := m.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	// Even with the caller unable to list anything, the handle's read works.
	f.listErr = errOffline
	buf := make([]byte, 5)
	if n := m.Read("/a.txt", buf, 0, fh); n != 5 || string(buf[:n]) != "hello" {
		t.Errorf("read through a handle = %q (%d bytes), want hello", buf[:n], n)
	}
}

// A read that fails partway through must return the bytes it already has rather
// than discarding them. Losing a partial read would make a large file
// unreadable over a flaky link, because the retry starts from scratch.
func TestPartialReadOnFailureReturnsWhatWasRead(t *testing.T) {
	m := newTestMount(t)
	m.setCacheForTest(newChunkCache(chunkSize*4, chunkSize*8))

	first := strings.Repeat("A", chunkSize)
	second := strings.Repeat("B", chunkSize)
	f := fakeOf(t, m)
	f.addFile("/big", 2*chunkSize)
	f.setContent("/big", []byte(first+second))

	fh, _ := m.Open("/big", OReadOnly)
	// Fail only the second chunk, so the first is read and cached, then the
	// error arrives. See readErrAt for why the failure is a sentinel rather than
	// an arbitrary error: the mount resolves the path before it reads, so a
	// failure injected for every offset would be raised inside resolution and
	// this test would pass without exercising the read path at all.
	f.setReadFailure(chunkSize)

	buf := make([]byte, 2*chunkSize)
	n := m.Read("/big", buf, 0, fh)
	if n != chunkSize {
		t.Errorf("Read = %d, want the first chunk's %d bytes and no more", n, chunkSize)
	}
	if string(buf[:n]) != first {
		t.Errorf("the partial read returned the wrong bytes")
	}
}

// With nothing cached, a failure on the very first read is reported as an errno
// rather than as a short read. Zero would mean end of file, which would tell the
// kernel the file is empty.
func TestFirstChunkFailureIsAnErrnoNotEOF(t *testing.T) {
	m := newTestMount(t)
	m.setCacheForTest(newChunkCache(chunkSize*4, chunkSize*8))

	f := fakeOf(t, m)
	f.addFile("/big", chunkSize*2)
	f.setContent("/big", []byte(strings.Repeat("A", chunkSize*2)))

	fh, _ := m.Open("/big", OReadOnly)
	// Nothing is cached, so the very first chunk read is the one that fails.
	f.setFirstReadFailure()

	// Zero would mean end of file, and the kernel would believe the file is
	// empty. The failure has to be an errno.
	if n := m.Read("/big", make([]byte, 16), 0, fh); n != -EIO {
		t.Errorf("a failed first chunk = %d, want -EIO (0 would mean end of file)", n)
	}
	// And it must be recoverable: the failure is one-shot, so a retry reads.
	if n := m.Read("/big", make([]byte, 16), 0, fh); n != 16 {
		t.Errorf("the retry after a failed first chunk = %d, want 16", n)
	}
}

// A short chunk means the file ended, so the read must stop asking rather than
// looping forever on a chunk the owner keeps returning empty.
func TestReadStopsOnAShortChunk(t *testing.T) {
	m := newTestMount(t)
	m.setCacheForTest(newChunkCache(chunkSize*4, chunkSize*8))

	// The index claims far more than the owner will produce, which is what a
	// stale index looks like.
	f := fakeOf(t, m)
	f.addFile("/big", chunkSize*4)
	f.setContent("/big", []byte("short"))

	fh, _ := m.Open("/big", OReadOnly)
	buf := make([]byte, chunkSize*4)
	n := m.Read("/big", buf, 0, fh)
	if n != 5 {
		t.Errorf("Read = %d, want 5: a short chunk ends the file", n)
	}
	if string(buf[:n]) != "short" {
		t.Errorf("content = %q", buf[:n])
	}
}

// The handle's own size must bound the read, which is what a file truncated
// through this mount looks like.
//
// The content is written before it is truncated. A handle's buffer starts empty
// -- truncate is "this is now the content, of this length" and the handle has no
// content until it is written or read into -- so truncating a never-written
// handle produces zeros, and a test that expected the owner's bytes to reappear
// would be asserting something the design does not do.
func TestReadIsBoundedByTheHandlesSize(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	if code := h.write(0, []byte("0123456789")); code != 0 {
		t.Fatalf("write = %d", code)
	}
	if code := h.truncateTo(3); code != 0 {
		t.Fatalf("truncate = %d", code)
	}
	buf := make([]byte, 10)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 3 || string(buf[:n]) != "012" {
		t.Errorf("read of a truncated handle = %q (%d bytes), want 012", buf[:n], n)
	}
}

// Truncating a handle that was never written yields zeros, not the owner's
// bytes. This is stated as its own test because it is the surprising half of the
// pair above: a handle's buffer is its own, and truncation does not pull the
// owner's content into it.
func TestTruncateWithoutWriteIsZeros(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	if code := h.truncateTo(3); code != 0 {
		t.Fatalf("truncate = %d", code)
	}
	buf := make([]byte, 10)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 3 {
		t.Fatalf("Read = %d, want 3", n)
	}
	if string(buf[:n]) != "\x00\x00\x00" {
		t.Errorf("read = %q, want the handle's own empty buffer padded to 3", buf[:n])
	}
	// The scratch is not the owner's data, and it is still dirty, so a release
	// will write it back. That is what truncation means.
	if !h.dirty {
		t.Error("a truncation must mark the handle dirty, or the new length is lost")
	}
}

// Reading a directory as a file is EISDIR. This is the read path's version of
// the check Open does, and it matters because the kernel can read a path it
// never opened.
func TestReadDirectoryIsEisdir(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")
	// Through a handle opened by Opendir, which is a legitimate way to reach a
	// read on a directory.
	fh, code := m.Opendir("/d")
	if code != 0 {
		t.Fatalf("Opendir = %d", code)
	}
	if n := m.Read("/d", make([]byte, 4), 0, fh); n != -EISDIR {
		t.Errorf("Read(dir) = %d, want -EISDIR", n)
	}
}

// A handle opendir'd on one directory must not serve a read of another path.
// The handle's path is what it was opened for.
func TestHandlePathsDoNotLeakBetweenOpens(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("aaaaa"))
	f.addFile("/b.txt", 5)
	f.setContent("/b.txt", []byte("bbbbb"))

	fa, _ := m.Open("/a.txt", OReadOnly)
	fb, _ := m.Open("/b.txt", OReadOnly)

	buf := make([]byte, 5)
	if n := m.Read("/ignored", buf, 0, fa); n != 5 || string(buf[:n]) != "aaaaa" {
		t.Errorf("read through handle a = %q (%d)", buf[:n], n)
	}
	if n := m.Read("/ignored", buf, 0, fb); n != 5 || string(buf[:n]) != "bbbbb" {
		t.Errorf("read through handle b = %q (%d)", buf[:n], n)
	}
}

// The kind recorded at open time must not change under the handle, because the
// handle's behaviour is chosen from it.
func TestOpenRecordsTheKind(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	fh, _ := m.Open("/a.txt", OReadOnly)
	h, _ := m.handle(fh)
	if h.kind != node.KindFile {
		t.Errorf("the handle records kind %v, want KindFile", h.kind)
	}
	dh, _ := m.Opendir("/")
	dir, _ := m.handle(dh)
	if dir.kind != node.KindDir {
		t.Errorf("the directory handle records kind %v, want KindDir", dir.kind)
	}
}
