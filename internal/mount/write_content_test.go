package mount

import "testing"

// ownerContent reads what the fake owner currently holds for path.
func ownerContent(t *testing.T, m *Mount, path string) string {
	t.Helper()
	f := fakeOf(t, m)
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.data[path])
}

// A write that covers only part of an existing file must leave the rest alone.
// The handle starts without the owner's bytes; without loading them first,
// content() would fill every byte outside the write with zeros.
func TestAPartialWritePreservesTheUntouchedBytes(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addContent("/a.txt", []byte("abcdefgh"))

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	if n := m.Write("/a.txt", []byte("XY"), 2, fh); n != 2 {
		t.Fatalf("Write = %d, want 2", n)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := ownerContent(t, m, "/a.txt"); got != "abXYefgh" {
		t.Fatalf("owner content = %q, want %q", got, "abXYefgh")
	}
}

// A file created through the mount and closed without a write must still reach
// the owner. It used to stay pending forever: the handle was clean, flush
// returned early, and no Put ever carried the create.
func TestACreatedEmptyFileReachesTheOwner(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)

	fh, code := m.Create("/empty.txt", 0o644, OReadWrite)
	if code != 0 {
		t.Fatalf("Create = %d", code)
	}
	if code := m.Flush("/empty.txt", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if code := m.Release("/empty.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := f.putCount(); got != 1 {
		t.Fatalf("uploads = %d, want 1: the create never reached the owner", got)
	}
	if got := ownerContent(t, m, "/empty.txt"); got != "" {
		t.Fatalf("owner content = %q, want empty", got)
	}
	if _, ok := f.nodes["/empty.txt"]; !ok {
		t.Fatal("the owner has no node for the empty file")
	}
}

// Truncating a file to zero needs no bytes from it. Fetching the whole file
// first is what made opening a large file with O_TRUNC cost a full download.
func TestTruncateToZeroDoesNotReadTheFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addContent("/big.txt", []byte("0123456789"))

	fh, code := m.Open("/big.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	before := f.countCalls("Read ")
	if code := m.Truncate("/big.txt", 0, fh); code != 0 {
		t.Fatalf("Truncate = %d", code)
	}
	if got := f.countCalls("Read "); got != before {
		t.Fatalf("truncate to zero made %d read(s), want 0", got-before)
	}
	if code := m.Release("/big.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := ownerContent(t, m, "/big.txt"); got != "" {
		t.Fatalf("owner content = %q, want empty", got)
	}
}

// Truncating to a shorter non-zero length keeps a prefix, so the bytes of that
// prefix still have to be loaded first.
func TestTruncateToAShorterLengthKeepsThePrefix(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addContent("/a.txt", []byte("0123456789"))

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	if code := m.Truncate("/a.txt", 4, fh); code != 0 {
		t.Fatalf("Truncate = %d", code)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := ownerContent(t, m, "/a.txt"); got != "0123" {
		t.Fatalf("owner content = %q, want %q", got, "0123")
	}
}

// After a truncation to zero the handle holds the complete (empty) file, so a
// write that follows must not fetch the old content back from the owner.
func TestAWriteAfterTruncateToZeroStartsFromEmpty(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addContent("/a.txt", []byte("old content"))

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	if code := m.Truncate("/a.txt", 0, fh); code != 0 {
		t.Fatalf("Truncate = %d", code)
	}
	before := f.countCalls("Read ")
	if n := m.Write("/a.txt", []byte("new"), 0, fh); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}
	if got := f.countCalls("Read "); got != before {
		t.Fatalf("the write after truncation made %d read(s), want 0", got-before)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := ownerContent(t, m, "/a.txt"); got != "new" {
		t.Fatalf("owner content = %q, want %q", got, "new")
	}
}

// A write that covers the whole existing file needs none of the old bytes, so
// rewriting a large file must not download it first.
func TestAFullOverwriteDoesNotReadTheOldContent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addContent("/a.txt", []byte("0123456789"))

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d", code)
	}
	before := f.countCalls("Read ")
	if n := m.Write("/a.txt", []byte("abcdefghij"), 0, fh); n != 10 {
		t.Fatalf("Write = %d, want 10", n)
	}
	if got := f.countCalls("Read "); got != before {
		t.Fatalf("a full overwrite made %d read(s), want 0", got-before)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}
	if got := ownerContent(t, m, "/a.txt"); got != "abcdefghij" {
		t.Fatalf("owner content = %q, want %q", got, "abcdefghij")
	}
}

// This version targets files up to 10 MB. The default write limit must leave
// room for one of those written through the mount, in several kernel-sized
// writes rather than a single buffer.
func TestATenMegabyteFileCanBeSaved(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)

	fh, code := m.Create("/ten.bin", 0o644, OReadWrite)
	if code != 0 {
		t.Fatalf("Create = %d", code)
	}
	const size = 10 << 20
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i*31 + 7)
	}
	for off := 0; off < size; off += len(chunk) {
		if n := m.Write("/ten.bin", chunk, int64(off), fh); n != len(chunk) {
			t.Fatalf("Write at %d = %d, want %d", off, n, len(chunk))
		}
	}
	if code := m.Flush("/ten.bin", fh); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if code := m.Release("/ten.bin", fh); code != 0 {
		t.Fatalf("Release = %d", code)
	}

	bodies := f.putBodies()
	if len(bodies) != 1 {
		t.Fatalf("uploads = %d, want 1", len(bodies))
	}
	body := bodies[0]
	if len(body) != size {
		t.Fatalf("uploaded %d bytes, want %d", len(body), size)
	}
	for _, off := range []int{0, 1 << 20, size/2 + 3, size - 1} {
		if body[off] != chunk[off%len(chunk)] {
			t.Fatalf("byte %d = %#x, want %#x", off, body[off], chunk[off%len(chunk)])
		}
	}
}
