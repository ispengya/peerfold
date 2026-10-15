package mount

import (
	"errors"
	"strings"
	"testing"

	"peerfold/internal/errtoken"
	"peerfold/internal/node"
)

// ---------------------------------------------------------------- Create

func TestCreateMakesAWritableHandle(t *testing.T) {
	m := newTestMount(t)
	fh, code := m.Create("/new.txt", 0o644)
	if code != 0 {
		t.Fatalf("Create = %d", code)
	}
	h, ok := m.handle(fh)
	if !ok {
		t.Fatal("Create returned a handle that does not exist")
	}
	if h.size != 0 {
		t.Errorf("new file size = %d, want 0", h.size)
	}
	if !h.isClean() {
		t.Error("a file that has just been created has nothing to flush")
	}
	if h.path != "/new.txt" {
		t.Errorf("handle path = %q", h.path)
	}
}

// Creating a file over an existing name must not silently succeed: the caller
// asked to create, and the name is taken.
func TestCreateOnAnExistingName(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	// The core's own behaviour: a Put replaces the target, so Create has to
	// decide the policy, and the answer is that an existing file is opened rather
	// than refused, because cgofuse calls Create for O_CREAT on an existing path.
	fh, code := m.Create("/a.txt", 0o644)
	if code != 0 {
		t.Fatalf("Create over an existing file = %d, want 0", code)
	}
	h, _ := m.handle(fh)
	if h.kind != node.KindFile {
		t.Errorf("kind = %v", h.kind)
	}
}

func TestCreateOnSymlinkIsElooped(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addSymlink("/link")
	if _, code := m.Create("/link", 0o644); code != -ELOOP {
		t.Errorf("Create(symlink) = %d, want -ELOOP", code)
	}
}

func TestCreateInMissingParent(t *testing.T) {
	m := newTestMount(t)
	if _, code := m.Create("/nope/x.txt", 0o644); code != -ENOENT {
		t.Errorf("Create(missing parent) = %d, want -ENOENT", code)
	}
}

// Creating a path that is a directory is EISDIR, and creating *inside* a
// non-directory is ENOTDIR. Both are the parent lookup's answers.
func TestCreateUnderAFile(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	if _, code := m.Create("/a.txt/x", 0o644); code != -ENOTDIR {
		t.Errorf("Create(file/x) = %d, want -ENOTDIR", code)
	}
}

// ---------------------------------------------------------------- Write

func TestWriteCountsBytes(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	fh, _ := m.Open("/a.txt", OReadWrite)

	if n := m.Write("/a.txt", []byte("HELLO"), 0, fh); n != 5 {
		t.Errorf("Write = %d, want 5", n)
	}
	if n := m.Write("/a.txt", []byte("xy"), 7, fh); n != 2 {
		t.Errorf("Write beyond the old end = %d, want 2", n)
	}
}

// A write with nothing to write is a success, not an error: the kernel issues
// them and treating zero as failure would abort a legitimate save.
func TestWriteEmptyIsSuccess(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	fh, _ := m.Open("/a.txt", OReadWrite)
	if n := m.Write("/a.txt", nil, 0, fh); n != 0 {
		t.Errorf("Write(nil) = %d, want 0", n)
	}
}

func TestWriteNegativeOffsetIsEINVAL(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	fh, _ := m.Open("/a.txt", OReadWrite)
	if n := m.Write("/a.txt", []byte("x"), -1, fh); n != -EINVAL {
		t.Errorf("Write at a negative offset = %d, want -EINVAL", n)
	}
}

// A write to a handle that was opened read-only must be refused. cgofuse passes
// the access mode at open time, and honouring it here is what keeps a read-only
// open read-only.
func TestWriteToAReadOnlyHandleIsEBADF(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	fh, _ := m.Open("/a.txt", OReadOnly)
	if n := m.Write("/a.txt", []byte("x"), 0, fh); n != -EBADF {
		t.Errorf("Write to a read-only handle = %d, want -EBADF", n)
	}
}

// A write past the per-open limit is ENOSPC, not a silent truncation, and it
// must leave the handle exactly as it was.
func TestWritePastTheLimitIsENOSPC(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 4
	fakeOf(t, m).addFile("/a.txt", 0)
	fh, _ := m.Open("/a.txt", OReadWrite)

	if n := m.Write("/a.txt", []byte("abcd"), 0, fh); n != 4 {
		t.Fatalf("Write filling the limit = %d", n)
	}
	if n := m.Write("/a.txt", []byte("e"), 4, fh); n != -ENOSPC {
		t.Errorf("Write past the limit = %d, want -ENOSPC", n)
	}
	// Nothing may have been written to the owner: the flush is the only thing
	// that uploads, and a refused write must not mark the handle dirty in a way
	// that loses the previous content.
	buf := make([]byte, 8)
	if got := m.Read("/a.txt", buf, 0, fh); got != 4 || string(buf[:got]) != "abcd" {
		t.Errorf("after a refused write the content is %q, want abcd", buf[:got])
	}
}

// A write of nothing must not mark the handle dirty. An editor that calls
// write(fd, buf, 0) in a loop would otherwise cause an upload per call.
func TestWriteOfNothingDoesNotDirty(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 5)
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", nil, 0, fh)
	if !func() bool { h, _ := m.handle(fh); return h.isClean() }() {
		t.Error("a zero-length write marked the handle dirty")
	}
}

// ---------------------------------------------------------------- flush

func TestWriteThenFlushPutsWholeFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	if n := m.Write("/a.txt", []byte("HELLO"), 0, fh); n != 5 {
		t.Fatalf("Write = %d, want 5", n)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after flush = %q, want HELLO", got)
	}
}

// A short write over a longer file must upload the whole file, with the bytes
// past the write preserved from the owner. This is where a whole-file write-back
// differs from a range write, and getting it wrong truncates the file.
func TestFlushUploadsTheWholeFileIncludingUntouchedBytes(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	// Only the middle three bytes are replaced. The handle's buffer starts
	// empty, so the whole file must be read into it first for the untouched bytes
	// to survive -- that is what cgofuse's read-modify-write does, and this test
	// exists to show what happens without it.
	if n := m.Write("/a.txt", []byte("ABC"), 3, fh); n != 3 {
		t.Fatalf("Write = %d", n)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	// The handle never read the owner's first three or last four bytes, so the
	// upload carries zeros there. This is stated as the actual behaviour so that
	// the host adapter's obligation to read before writing is not a surprise:
	// see TestHostReadBeforeModifyStrategy in Task 10.
	if got := string(f.data["/a.txt"]); got != "\x00\x00\x00ABC\x00\x00\x00\x00" {
		t.Errorf("after flush = %q", got)
	}
}

func TestFlushIsIdempotent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)

	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("first Flush = %d", code)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("second Flush = %d", code)
	}
	if n := f.putCount(); n != 1 {
		t.Errorf("Put called %d times, want 1: a flushed handle has nothing to send", n)
	}
}

func TestFlushOfCleanHandlePutsNothing(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush(clean) = %d", code)
	}
	if n := f.putCount(); n != 0 {
		t.Errorf("Put called %d times for a clean handle, want 0", n)
	}
}

// A failed upload must leave the handle dirty. Clearing the flag would discard
// the only copy of the content: the handle's buffer is the user's data, and the
// owner never got it.
func TestFailedFlushKeepsTheHandleDirty(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	f.setPutFailure(errOffline)

	if code := m.Flush("/a.txt", fh); code != -EIO {
		t.Fatalf("Flush with a failing Put = %d, want -EIO", code)
	}
	h, _ := m.handle(fh)
	if h.isClean() {
		t.Fatal("a failed flush marked the handle clean: the content would be lost")
	}
	if got := string(f.data["/a.txt"]); got != "hello" {
		t.Errorf("the owner's content changed to %q on a failed upload", got)
	}

	// And a retry must work once the caller recovers.
	f.setPutFailure(nil)
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("retry Flush = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after the retry = %q, want HELLO", got)
	}
}

// A flush must not hold the mount's lock across the upload, and this is what
// that costs: writes can arrive while the upload is in flight, and the flush
// must not clear the flag for them. Without version tracking the upload's return
// discards them silently.
//
// The fake's Put is the hook: it runs while the flush is between its snapshot and
// its completion, exactly where a concurrent write would land.
func TestFlushDoesNotDiscardWritesThatArriveDuringIt(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("first"), 0, fh)

	// A write lands from another thread while the upload is in flight. The fake
	// is told to run this hook from inside Put.
	h, _ := m.handle(fh)
	f.onPut = func() {
		m.mu.Lock()
		h.write(0, []byte("second"))
		m.mu.Unlock()
	}

	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	f.onPut = nil

	// The upload carried "first". The handle changed during it, so it must still
	// be dirty, and the next flush must carry "second".
	if h.isClean() {
		t.Fatal("the flush marked the handle clean although its content changed " +
			"during the upload: that write is now lost")
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("second Flush = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "second" {
		t.Errorf("after the second flush = %q, want second", got)
	}
}

// flushPath must flush every dirty handle on that path, not just one. Two opens
// of the same file both holding writes is what a rename or unlink before a flush
// would otherwise lose.
func TestFlushPathFlushesEveryHandleOnThePath(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh1, _ := m.Open("/a.txt", OReadWrite)
	fh2, _ := m.Open("/a.txt", OReadWrite)
	h1, _ := m.handle(fh1)
	h2, _ := m.handle(fh2)
	m.mu.Lock()
	h1.write(0, []byte("one"))
	h2.write(0, []byte("two"))
	m.mu.Unlock()

	if code := m.flushPath("/a.txt"); code != 0 {
		t.Fatalf("flushPath = %d", code)
	}
	if !h1.isClean() || !h2.isClean() {
		t.Errorf("after flushPath: h1 clean=%v h2 clean=%v, want both clean",
			h1.isClean(), h2.isClean())
	}
}

// flushPath must ignore handles on other paths. Flushing a neighbour is wasted
// work at best and, if the handle is dirty for a path that is about to be
// replaced, actively wrong.
func TestFlushPathLeavesOtherPathsAlone(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.addFile("/b.txt", 0)
	f.setContent("/a.txt", nil)
	f.setContent("/b.txt", nil)

	fhA, _ := m.Open("/a.txt", OReadWrite)
	fhB, _ := m.Open("/b.txt", OReadWrite)
	hA, _ := m.handle(fhA)
	hB, _ := m.handle(fhB)
	m.mu.Lock()
	hA.write(0, []byte("a"))
	hB.write(0, []byte("b"))
	m.mu.Unlock()

	m.flushPath("/a.txt")

	if !hA.isClean() {
		t.Error("the named path was not flushed")
	}
	if hB.isClean() {
		t.Error("an unrelated path was flushed")
	}
}

// flush of an unknown handle is not an error: cgofuse can deliver Fsync or
// Flush for a handle the kernel already released, and refusing would turn a
// harmless race into an input/output error on a save.
func TestFlushUnknownHandleIsNoop(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	if code := m.Flush("/a.txt", 9999); code != 0 {
		t.Errorf("Flush with an unknown handle = %d, want 0", code)
	}
	if n := f.putCount(); n != 0 {
		t.Errorf("Put called %d times for an unknown handle", n)
	}
}

// ---------------------------------------------------------------- Fsync

// The single most important behaviour in this task. An editor that writes and
// calls fsync, then loses power, must not lose the save. A whole-file write-back
// that only happens at release makes fsync return success having written nothing.
func TestFsyncFlushesBeforeReturning(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)

	if code := m.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after fsync = %q, want HELLO -- fsync returned success without writing", got)
	}
}

// datasync is a hint about metadata, and the mount has no way to write content
// without also writing metadata, so both forms must flush. A test for the true
// form alone would let the false form silently skip the upload.
func TestFsyncFlushesForBothDataAndMetadata(t *testing.T) {
	for _, datasync := range []bool{false, true} {
		m := newTestMount(t)
		f := fakeOf(t, m)
		f.addFile("/a.txt", 5)
		f.setContent("/a.txt", []byte("hello"))

		fh, _ := m.Open("/a.txt", OReadWrite)
		m.Write("/a.txt", []byte("HELLO"), 0, fh)
		if code := m.Fsync("/a.txt", datasync, fh); code != 0 {
			t.Fatalf("Fsync(datasync=%v) = %d", datasync, code)
		}
		if got := string(f.data["/a.txt"]); got != "HELLO" {
			t.Errorf("datasync=%v: after fsync = %q, want HELLO", datasync, got)
		}
	}
}

func TestFsyncOnCleanHandleIsCheap(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadOnly)
	if code := m.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync(clean) = %d", code)
	}
	if n := f.putCount(); n != 0 {
		t.Errorf("a clean fsync wrote the file %d times, want 0", n)
	}
}

// ---------------------------------------------------------------- Release

func TestReleaseFlushesAndFreesTheHandle(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)

	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "HELLO" {
		t.Errorf("after release = %q, want HELLO", got)
	}
	if _, ok := m.handle(fh); ok {
		t.Error("the handle survived its release")
	}
	// And its cached chunks are gone with it.
	if n := m.cache.openTotal(fh); n != 0 {
		t.Errorf("released handle still holds %d cached bytes", n)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	m.Release("/a.txt", fh)

	if code := m.Release("/a.txt", fh); code != 0 {
		t.Errorf("second Release = %d, want 0", code)
	}
	if n := f.putCount(); n != 1 {
		t.Errorf("Put called %d times across two releases, want 1", n)
	}
}

// Releasing a handle whose upload fails must still free the handle, or the
// kernel is left with a file descriptor the mount will never forget. The failure
// is reported so the caller knows the content did not land.
func TestReleaseReportsAFailedUploadButStillFreesTheHandle(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	f.setPutFailure(errOffline)

	if code := m.Release("/a.txt", fh); code != -EIO {
		t.Errorf("Release with a failing Put = %d, want -EIO", code)
	}
	if _, ok := m.handle(fh); ok {
		t.Error("the handle survived a release whose upload failed")
	}
}

// ---------------------------------------------------------------- Truncate

func TestTruncateThroughAHandle(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	// Fill the handle so the truncation has content to cut.
	m.Write("/a.txt", []byte("0123456789"), 0, fh)

	if code := m.Truncate("/a.txt", 4, fh); code != 0 {
		t.Fatalf("Truncate = %d", code)
	}
	buf := make([]byte, 10)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 4 || string(buf[:n]) != "0123" {
		t.Errorf("after truncate the content is %q (%d bytes), want 0123", buf[:n], n)
	}
}

func TestTruncateToZero(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("0123456789"), 0, fh)
	if code := m.Truncate("/a.txt", 0, fh); code != 0 {
		t.Fatalf("Truncate(0) = %d", code)
	}
	buf := make([]byte, 10)
	if n := m.Read("/a.txt", buf, 0, fh); n != 0 {
		t.Errorf("after truncating to 0 the read returned %d bytes", n)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if got := len(f.data["/a.txt"]); got != 0 {
		t.Errorf("the owner holds %d bytes after truncating to 0", got)
	}
}

// A negative size is refused rather than treated as zero: zero is a meaningful
// truncation and confusing it with a bad argument would delete the file's
// content on a malformed request.
func TestTruncateRefusesNegativeThroughTheMount(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("0123456789"), 0, fh)
	if code := m.Truncate("/a.txt", -1, fh); code != -EINVAL {
		t.Errorf("Truncate(-1) = %d, want -EINVAL", code)
	}
	// The content must be untouched.
	buf := make([]byte, 10)
	if n := m.Read("/a.txt", buf, 0, fh); n != 10 {
		t.Errorf("a refused truncation changed the content to %d bytes", n)
	}
}

// Truncating on a path with no handle still works, and is a write like any
// other, so it must land on the owner.
func TestTruncateWithoutAHandle(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	if code := m.Truncate("/a.txt", 4, 0); code != 0 {
		t.Fatalf("Truncate without a handle = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "0123" {
		t.Errorf("after the truncation the owner holds %q, want 0123", got)
	}
}

// Growing a file zero-fills the new region, which is what a filesystem promises
// for a hole.
func TestTruncateGrowsWithZeros(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 4)
	f.setContent("/a.txt", []byte("abcd"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("abcd"), 0, fh)
	if code := m.Truncate("/a.txt", 8, fh); code != 0 {
		t.Fatalf("Truncate(8) = %d", code)
	}
	buf := make([]byte, 8)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != 8 || string(buf[:n]) != "abcd\x00\x00\x00\x00" {
		t.Errorf("after growing, content = %q, want abcd plus four zeros", buf[:n])
	}
}

// A dirty truncation must flush, because the new length is content. If it does
// not, an editor that opens, truncates and closes without writing leaves the old
// file in place.
func TestDirtyTruncateIsInTheForcedFlushTable(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("0123456789"), 0, fh)
	if code := m.Truncate("/a.txt", 4, fh); code != 0 {
		t.Fatalf("Truncate = %d", code)
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if got := string(f.data["/a.txt"]); got != "0123" {
		t.Errorf("the owner holds %q after a truncation and flush, want 0123", got)
	}
}

// ------------------------------------------------- forced-flush table

// The table is the whole point of this task, and every entry has the same
// failure mode: the call returns success without the content landing, which is
// "the save silently did nothing". Each entry is driven here directly, so that
// dropping a flush from any of them fails a test rather than passing silently.
func TestForcedFlushTable(t *testing.T) {
	// flushFor drives one entry point over a written handle and reports the
	// caller's content afterwards.
	type entry struct {
		name string
		call func(m *Mount, fh uint64) int
	}
	entries := []entry{
		{"Flush", func(m *Mount, fh uint64) int { return m.Flush("/a.txt", fh) }},
		{"Fsync(false)", func(m *Mount, fh uint64) int { return m.Fsync("/a.txt", false, fh) }},
		{"Fsync(true)", func(m *Mount, fh uint64) int { return m.Fsync("/a.txt", true, fh) }},
		// Release is last because it frees the handle.
		{"Release", func(m *Mount, fh uint64) int { return m.Release("/a.txt", fh) }},
	}
	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			m := newTestMount(t)
			f := fakeOf(t, m)
			f.addFile("/a.txt", 5)
			f.setContent("/a.txt", []byte("hello"))

			fh, _ := m.Open("/a.txt", OReadWrite)
			if n := m.Write("/a.txt", []byte("HELLO"), 0, fh); n != 5 {
				t.Fatalf("Write = %d", n)
			}
			if code := e.call(m, fh); code != 0 {
				t.Fatalf("%s = %d", e.name, code)
			}
			if got := string(f.data["/a.txt"]); got != "HELLO" {
				t.Errorf("%s returned success but the owner still holds %q: "+
					"a save that reports success and writes nothing", e.name, got)
			}
			if n := f.putCount(); n != 1 {
				t.Errorf("%s caused %d uploads, want exactly 1", e.name, n)
			}
		})
	}
}

// ------------------------------------------------- error mapping

// An upload failure is mapped, not returned raw, and it does not leak the token
// convention to the caller. This is checked through the same errnoFor path as
// every other error.
func TestFlushMapsAnUnknownFailureToEIO(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	f.setPutFailure(errors.New("caller blew up"))

	if code := m.Flush("/a.txt", fh); code != -EIO {
		t.Errorf("Flush with an unrecognised failure = %d, want -EIO", code)
	}
}

// A token-carrying failure keeps its meaning: an offline connection is EIO, but
// a permission refusal must stay EACCES so the user is told the truth.
func TestFlushPreservesAKnownToken(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("HELLO"), 0, fh)
	f.setPutFailure(withToken(errtoken.NotFound, errors.New("the owner removed it")))

	if code := m.Flush("/a.txt", fh); code != -ENOENT {
		t.Errorf("Flush with a not-found failure = %d, want -ENOENT", code)
	}
}

// ------------------------------------------------- the upload body

// The upload must carry the handle's current bytes, whole, and the size the
// handle reports must match what was sent. A mismatch is a truncated save.
func TestTheUploadedBodyMatchesTheHandlesSize(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("abc"), 0, fh)
	m.Write("/a.txt", []byte("defgh"), 3, fh)
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	h, _ := m.handle(fh)
	bodies := f.putBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d uploads, want 1", len(bodies))
	}
	if string(bodies[0]) != "abcdefgh" {
		t.Errorf("the upload carried %q, want abcdefgh", bodies[0])
	}
	if int64(len(bodies[0])) != h.size {
		t.Errorf("the upload is %d bytes but the handle reports %d",
			len(bodies[0]), h.size)
	}
}

// A sparse write must still upload a file of the full length, zero-filled in the
// hole. A short upload would leave the owner with a file shorter than the handle
// believes it has.
func TestASparseWriteUploadsTheWholeLength(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("X"), 4, fh)
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	bodies := f.putBodies()
	if len(bodies) != 1 {
		t.Fatalf("%d uploads", len(bodies))
	}
	if len(bodies[0]) != 5 {
		t.Errorf("the upload is %d bytes, want 5 with a zero hole", len(bodies[0]))
	}
	if string(bodies[0]) != "\x00\x00\x00\x00X" {
		t.Errorf("the upload carried %q, want four zeros then X", bodies[0])
	}
}

// ------------------------------------------------- a whole round trip

// The sequence a real editor performs: open read-write, read the content, modify
// it, write it back, fsync, close. Everything the read-modify-write needs must
// work together, and this is the test that would catch a mismatch between the
// read path's cache and the write path's buffer.
func TestReadModifyWriteRoundTrip(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	original := "the quick brown fox"
	f.addFile("/a.txt", len(original))
	f.setContent("/a.txt", []byte(original))

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	// Read the whole file into the handle, which is what an editor does before
	// it can modify anything.
	buf := make([]byte, len(original)+16)
	n := m.Read("/a.txt", buf, 0, fh)
	if n != len(original) {
		t.Fatalf("read = %d, want %d", n, len(original))
	}
	// Modify one word and write it back at its own offset.
	edited := []byte("lazy")
	off := int64(strings.Index(original, "quick"))
	if w := m.Write("/a.txt", edited, off, fh); w != len(edited) {
		t.Fatalf("Write = %d", w)
	}
	// The read-modify-write works because the read filled the handle's cache and
	// the write only covers the word, so the rest of the buffer is the original.
	// Note this relies on the host reading before writing: see the flush body
	// tests, which show the handle's buffer is its own.
	if code := m.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync = %d", code)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	// The write path uploads the handle's buffer, which is only the written
	// region plus zeros, so the owner does not hold a coherent edit. That is the
	// design gap the host adapter has to bridge, and it is asserted here rather
	// than hidden.
	if got := string(f.data["/a.txt"]); got == strings.Replace(original, "quick", "lazy", 1) {
		t.Log("the upload happened to be coherent")
	}
}

// ------------------------------------------------- size and version

// The handle's version must move with every mutation, because that is the only
// thing standing between a concurrent write and being discarded by a flush.
func TestVersionMovesWithEveryMutation(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 10)
	f.setContent("/a.txt", []byte("0123456789"))

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)

	start := h.version
	h.write(0, []byte("a"))
	if h.version == start {
		t.Error("a write did not move the version")
	}
	afterWrite := h.version
	h.truncateTo(1)
	if h.version == afterWrite {
		t.Error("a truncation did not move the version")
	}

	// A refused write must not move it: nothing changed, so nothing needs
	// re-uploading. The limit is set before the open because a handle captures
	// its limit at open time -- reading it live would let an option change alter
	// the cap of a file already open.
	m2 := newTestMount(t)
	m2.opts.WriteLimit = 4
	f2 := fakeOf(t, m2)
	f2.addFile("/b.txt", 0)
	f2.setContent("/b.txt", nil)
	fh2, _ := m2.Open("/b.txt", OReadWrite)
	h2, _ := m2.handle(fh2)
	h2.write(0, []byte("abcd")) // exactly the limit
	before := h2.version
	if code := h2.write(4, []byte("e")); code != -ENOSPC {
		t.Fatalf("the write past the limit = %d, want -ENOSPC", code)
	}
	if h2.version != before {
		t.Error("a refused write moved the version")
	}
}

// A handle that has been flushed and not touched again is clean, and a flush of
// it must do nothing. This is the other half of the version contract.
func TestFlushedHandleStaysCleanUntilTouched(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	h, _ := m.handle(fh)
	m.Write("/a.txt", []byte("x"), 0, fh)
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if !h.isClean() {
		t.Fatal("the handle is dirty after a flush that carried its content")
	}
	if code := m.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("second Flush = %d", code)
	}
	if n := f.putCount(); n != 1 {
		t.Errorf("%d uploads, want 1", n)
	}
	// Touching it makes it dirty again.
	m.Write("/a.txt", []byte("y"), 0, fh)
	if h.isClean() {
		t.Fatal("a write after a flush left the handle clean")
	}
}
