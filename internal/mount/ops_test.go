package mount

import (
	"testing"

	"peerfold/internal/node"
)

func TestGetattrRoot(t *testing.T) {
	m := newTestMount(t)
	var st Stat
	if code := m.Getattr("/", &st, 0); code != 0 {
		t.Fatalf("Getattr(/) = %d", code)
	}
	if st.Mode&modeDir == 0 {
		t.Errorf("root Mode = %o, want the directory bit", st.Mode)
	}
	if st.Nlink == 0 {
		t.Error("Nlink must not be 0: some tools use it to detect a broken fs")
	}
}

func TestGetattrFile(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 42)
	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Mode&modeRegular == 0 {
		t.Errorf("Mode = %o, want the regular-file bit", st.Mode)
	}
	if st.Mode&modeDir != 0 {
		t.Errorf("Mode = %o, must not claim to be a directory", st.Mode)
	}
	if st.Size != 42 {
		t.Errorf("Size = %d, want 42", st.Size)
	}
	if st.Nlink != 1 {
		t.Errorf("Nlink = %d for a file, want 1", st.Nlink)
	}
}

// A directory reports Nlink 2, the value POSIX gives a directory with no
// subdirectories. Some tools read Nlink to decide whether a directory is
// reported at all, so it must not be 0.
func TestGetattrDirectoryNlink(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")
	var st Stat
	if code := m.Getattr("/d", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Mode&modeDir == 0 {
		t.Errorf("Mode = %o, want the directory bit", st.Mode)
	}
	if st.Nlink != 2 {
		t.Errorf("Nlink = %d for a directory, want 2", st.Nlink)
	}
}

// The permission bits the owner reported must survive into the Stat, or every
// file in the mount would appear to have whatever mode this layer felt like
// inventing. The index records the owner's full FileMode (Task 1).
//
// The permission bits are widened, not copied: Windows refuses an open with
// access denied when the reported mode lacks the bit the open asked for, so a
// file the owner recorded as 0640 became unopenable through the mount. The
// owner's own read-only choice is preserved on the owner's disk; the mount is a
// federation surface, and every node it reports is readable, writable and
// traversable by the caller. That widening is to owner, group and world together
// (0777): the adapter does not set uid/gid, so the caller reaches the mount as
// the world principal, and world-only widening would still be refused. What the
// test pins is that the *type* and the *widened* bits are right, not that a
// particular widening happened to be chosen.
func TestGetattrCarriesTheOwnersPermissionBits(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	n := node.Node{Path: "/x.txt", Kind: node.KindFile, Size: 1, Mode: 0o100640}
	f.mu.Lock()
	f.ensureParents("/x.txt")
	f.nodes["/x.txt"] = n
	f.mu.Unlock()

	var st Stat
	if code := m.Getattr("/x.txt", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if got := st.Mode & 0o777; got != 0o777 {
		t.Errorf("permission bits = %o, want 777: the mount reports every "+
			"node as readable, writable and traversable for owner, group and "+
			"world, so the caller reaches it as the world principal", got)
	}
	// And the type field names exactly one kind. Testing it bit by bit would be
	// wrong: 0o120000 contains 0o100000, so a symlink would answer "yes" to both
	// the file bit and the symlink bit.
	if got := st.Mode & modeTypeMask; got != modeRegular {
		t.Errorf("type field = %o, want %o (a regular file)", got, modeRegular)
	}
}

func TestGetattrSymlinkIsElooped(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addSymlink("/link")
	var st Stat
	if code := m.Getattr("/link", &st, 0); code != -ELOOP {
		t.Errorf("Getattr(symlink) = %d, want -ELOOP (%d)", code, -ELOOP)
	}
}

func TestGetattrMissing(t *testing.T) {
	m := newTestMount(t)
	var st Stat
	if code := m.Getattr("/nope", &st, 0); code != -ENOENT {
		t.Errorf("Getattr(missing) = %d, want -ENOENT", code)
	}
}

// A stat through the path of a file must be ENOTDIR, which is what the index
// reports when the node at the queried path is itself a file.
func TestGetattrThroughAFileIsNotDir(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	var st Stat
	if code := m.Getattr("/a.txt/x", &st, 0); code != -ENOTDIR {
		t.Errorf("Getattr(file/x) = %d, want -ENOTDIR", code)
	}
}

// A dirty handle reports the size it is about to have. Without this a program
// that writes past the old end sees its own write vanish on the next stat,
// which is how a truncating save corrupts a file.
func TestGetattrUsesDirtyHandleSize(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 10)

	fh := m.newHandleForTest("/a.txt", node.KindFile)
	h, _ := m.handle(fh)
	if code := h.write(0, []byte("0123456789abcdef")); code != 0 {
		t.Fatalf("write = %d", code)
	}

	var st Stat
	if code := m.Getattr("/a.txt", &st, fh); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Size != 16 {
		t.Errorf("Size = %d, want 16: the open handle's size must win", st.Size)
	}

	// Without the handle the index's size is what is known, which is the
	// pre-write value until the write is flushed.
	var indexed Stat
	if code := m.Getattr("/a.txt", &indexed, 0); code != 0 {
		t.Fatalf("Getattr without fh = %d", code)
	}
	if indexed.Size != 10 {
		t.Errorf("Size without fh = %d, want the index's 10", indexed.Size)
	}
}

// A clean handle has nothing to say about size, so the index's value is used
// even when a handle is open. Reporting the handle's copy unconditionally would
// freeze the size at whatever it was when the file was opened.
func TestGetattrIgnoresCleanHandleSize(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 10)

	fh := m.newHandleForTest("/a.txt", node.KindFile)
	h, _ := m.handle(fh)
	h.size = 9999 // seeded, but not dirty

	var st Stat
	if code := m.Getattr("/a.txt", &st, fh); code != 0 {
		t.Fatalf("Getattr = %d", code)
	}
	if st.Size != 10 {
		t.Errorf("Size = %d, want the index's 10: a clean handle must not override it", st.Size)
	}
}

// A file has no children, so listing it is ENOTDIR. This is the one error the
// index distinguishes, and the mount must pass it through rather than turning
// it into ENOENT.
func TestReaddirOnFileIsNotDir(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if code := m.Readdir("/a.txt", func(string, *Stat, int64) bool { return true }, 0, 0); code != -ENOTDIR {
		t.Errorf("Readdir(file) = %d, want -ENOTDIR", code)
	}
}

func TestReaddirMissingIsNoEnt(t *testing.T) {
	m := newTestMount(t)
	if code := m.Readdir("/nope", func(string, *Stat, int64) bool { return true }, 0, 0); code != -ENOENT {
		t.Errorf("Readdir(missing) = %d, want -ENOENT", code)
	}
}

func TestReaddirListsChildrenPlusDots(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addFile("/d/a.txt", 1)
	f.addFile("/d/b.txt", 2)

	var names []string
	code := m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		names = append(names, name)
		return true
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir = %d", code)
	}
	want := map[string]bool{".": true, "..": true, "a.txt": true, "b.txt": true}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected entry %q", n)
		}
		delete(want, n)
	}
	for n := range want {
		t.Errorf("missing entry %q (got %v)", n, names)
	}
}

// Children must come out in a stable order. An unstable one makes a listing
// flicker between reads for no reason the user can see.
func TestReaddirOrderIsStable(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	for _, n := range []string{"c", "a", "d", "b"} {
		f.addFile("/d/"+n, 1)
	}

	read := func() []string {
		var names []string
		m.Readdir("/d", func(name string, st *Stat, off int64) bool {
			names = append(names, name)
			return true
		}, 0, 0)
		return names
	}
	first := read()
	for i := 0; i < 5; i++ {
		got := read()
		if len(got) != len(first) {
			t.Fatalf("listing length changed: %v then %v", first, got)
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("listing order changed: %v then %v", first, got)
			}
		}
	}
	// The dots are first, then the names in order.
	want := []string{".", "..", "a", "b", "c", "d"}
	for i := range want {
		if i >= len(first) || first[i] != want[i] {
			t.Fatalf("listing = %v, want %v", first, want)
		}
	}
}

// Listing must hand back a whole name, not a suffix of one. A child recorded at
// "/d/ab" must never be offered as "b".
func TestReaddirNamesAreWhole(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addFile("/d/ab", 1)

	var names []string
	m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		names = append(names, name)
		return true
	}, 0, 0)
	for _, n := range names {
		if n == "b" {
			t.Errorf("listed %q, which is a suffix of the child's name: %v", n, names)
		}
	}
	if len(names) != 3 {
		t.Errorf("listing = %v, want the dots and ab", names)
	}
}

// A symlink in a listing must be visible but flagged, never passed off as a
// regular file with a plausible size.
func TestReaddirMarksSymlink(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addSymlink("/d/link")

	var got *Stat
	m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		if name == "link" {
			got = st
		}
		return true
	}, 0, 0)
	if got == nil {
		t.Fatal("link not listed")
	}
	if got := got.Mode & modeTypeMask; got != modeSymlink {
		t.Errorf("link type field = %o, want %o (a symlink)", got, modeSymlink)
	}
}

// The dots carry no stat, which is what cgofuse's own examples pass and what the
// host adapter turns into a NULL stat pointer.
func TestReaddirDotsHaveNoStat(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")

	seen := map[string]bool{}
	m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		if name == "." || name == ".." {
			seen[name] = true
			if st != nil {
				t.Errorf("%s carried a stat, want nil", name)
			}
		} else if st == nil {
			t.Errorf("%q carried no stat, want one", name)
		}
		return true
	}, 0, 0)
	if !seen["."] || !seen[".."] {
		t.Errorf("the dots were not both listed: %v", seen)
	}
}

func TestReaddirRespectsFillReturningFalse(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		f.addFile("/d/"+n, 1)
	}
	count := 0
	code := m.Readdir("/d", func(string, *Stat, int64) bool {
		count++
		return count < 3 // stop early, as the kernel does when the buffer fills
	}, 0, 0)
	if code != 0 {
		t.Errorf("Readdir = %d, want 0: a full buffer is not an error", code)
	}
	if count != 3 {
		t.Errorf("fill was called %d times, want exactly 3: it must stop as soon as "+
			"the callback says the buffer is full", count)
	}
}

// Stopping on the very first entry must not be an error either, and must not
// keep walking the children.
func TestReaddirStopsOnTheFirstRefusal(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	for _, n := range []string{"a", "b", "c"} {
		f.addFile("/d/"+n, 1)
	}
	count := 0
	code := m.Readdir("/d", func(string, *Stat, int64) bool {
		count++
		return false
	}, 0, 0)
	if code != 0 {
		t.Errorf("Readdir = %d, want 0", code)
	}
	if count != 1 {
		t.Errorf("fill was called %d times, want 1", count)
	}
}

// Listing an empty directory still lists the dots. Returning nothing at all
// makes some tools treat the directory as unreadable.
func TestReaddirEmptyDirectory(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")

	var names []string
	if code := m.Readdir("/d", func(name string, st *Stat, off int64) bool {
		names = append(names, name)
		return true
	}, 0, 0); code != 0 {
		t.Fatalf("Readdir = %d", code)
	}
	if len(names) != 2 || names[0] != "." || names[1] != ".." {
		t.Errorf("listing = %v, want exactly the dots", names)
	}
}

// A caller error while listing must be mapped, not swallowed into a success with
// no entries: an empty listing and a failed one look the same to a reader.
//
// The failure is injected for the directory's own listing and not for the root,
// so path resolution still succeeds and this reaches the mapping. Injecting it
// for every path would fail inside lookup() instead, and the test would pass
// whether or not Readdir mapped anything.
func TestReaddirMapsACallerError(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.listErr, f.listErrPath = errOffline, "/d"

	code := m.Readdir("/d", func(string, *Stat, int64) bool { return true }, 0, 0)
	if code != -EIO {
		t.Errorf("Readdir with a failing caller = %d, want -EIO", code)
	}
}

// The same, for a path-resolution failure: a lookup that cannot reach the caller
// must report the mapped error rather than ENOENT. Reporting "not found" for a
// connection that is gone tells the user their file disappeared, which is a much
// worse answer than "the filesystem is unavailable".
func TestReaddirMapsAFailingLookup(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.listErr, f.listErrPath = errOffline, "" // every path, including the root

	if code := m.Readdir("/d", func(string, *Stat, int64) bool { return true }, 0, 0); code != -EIO {
		t.Errorf("Readdir with a failing lookup = %d, want -EIO", code)
	}
}

// Getattr and Statfs must map a failing lookup the same way. Each has an early
// return that would otherwise let an offline mount look like a missing file.
func TestGetattrAndStatfsMapAFailingLookup(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.listErr, f.listErrPath = errOffline, ""

	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != -EIO {
		t.Errorf("Getattr with a failing lookup = %d, want -EIO", code)
	}
	var sf Statfs
	if code := m.Statfs("/a.txt", &sf); code != -EIO {
		t.Errorf("Statfs with a failing lookup = %d, want -EIO", code)
	}
}

func TestStatfsReportsSomethingUsable(t *testing.T) {
	m := newTestMount(t)
	var st Statfs
	if code := m.Statfs("/", &st); code != 0 {
		t.Fatalf("Statfs = %d", code)
	}
	if st.Bsize == 0 {
		t.Error("Bsize must not be 0: a zero block size makes some tools divide by zero")
	}
	if st.Namemax == 0 {
		t.Error("Namemax must not be 0")
	}
	if st.Blocks == 0 {
		t.Error("Blocks must not be 0")
	}
	if st.Bfree > st.Blocks || st.Bavail > st.Blocks {
		t.Errorf("free space (%d/%d) exceeds total (%d), which is not self-consistent",
			st.Bfree, st.Bavail, st.Blocks)
	}
	if st.Ffree > st.Files {
		t.Errorf("free inodes %d exceeds total %d", st.Ffree, st.Files)
	}
	// Bavail is what an unprivileged writer is allowed to use, so it must not
	// exceed Bfree.
	if st.Bavail > st.Bfree {
		t.Errorf("Bavail %d exceeds Bfree %d, which no filesystem reports", st.Bavail, st.Bfree)
	}
}

// Statfs for a path that does not exist is ENOENT, not a success with invented
// numbers: a tool asking about a missing directory should be told it is missing.
func TestStatfsMissingPath(t *testing.T) {
	m := newTestMount(t)
	var st Statfs
	if code := m.Statfs("/nope", &st); code != -ENOENT {
		t.Errorf("Statfs(missing) = %d, want -ENOENT", code)
	}
}

// Every type bit this package declares must match the constant the FUSE library
// uses, because the host adapter copies the field across unchanged. Getting one
// wrong makes directories appear as files. These are cgofuse's S_IFDIR,
// S_IFREG and S_IFLNK, restated here as the numbers rather than imported, so the
// core stays free of the library.
func TestTypeBitsMatchTheFuseConstants(t *testing.T) {
	cases := []struct {
		kind node.Kind
		want uint32
	}{
		{node.KindDir, 0o040000},
		{node.KindFile, 0o100000},
		{node.KindSymlink, 0o120000},
	}
	for _, c := range cases {
		if got := typeBits(c.kind); got != c.want {
			t.Errorf("typeBits(%v) = %#o, want %#o", c.kind, got, c.want)
		}
	}
	// An unknown kind must not be reported as a directory: claiming something is
	// a directory makes the kernel try to list it.
	if got := typeBits(node.KindDeleted); got&modeDir != 0 {
		t.Errorf("typeBits(KindDeleted) = %#o, must not be a directory", got)
	}
}

// The type field is one field, not three independent bits, and the difference is
// observable: 0o120000 contains 0o100000. A reader that bit-tests instead of
// masking sees a symlink as both a symlink and a regular file -- which is the
// mistake the tests above originally made. This pins the property so the
// reasoning there cannot be quietly invalidated.
func TestTypeFieldConstantsAreNotDisjointBits(t *testing.T) {
	if modeSymlink&modeRegular == 0 {
		t.Fatal("this test assumes 0o120000 contains 0o100000; if that is no " +
			"longer true, the masking in the tests above could be simplified")
	}
	symlink := statFor(node.Node{Path: "/l", Kind: node.KindSymlink, Mode: 1 << 27})
	if got := symlink.Mode & modeTypeMask; got != modeSymlink {
		t.Errorf("type field = %o, want %o", got, modeSymlink)
	}
	if symlink.Mode&modeRegular == 0 {
		t.Error("a symlink's mode does contain the regular-file bit, which is " +
			"exactly why a caller must mask rather than bit-test")
	}
	// And the mask itself must cover every type this package can produce.
	for _, k := range []node.Kind{node.KindDir, node.KindFile, node.KindSymlink} {
		if bits := typeBits(k); bits&^modeTypeMask != 0 {
			t.Errorf("typeBits(%v) = %#o has bits outside the type mask %#o",
				k, bits, modeTypeMask)
		}
	}
}
