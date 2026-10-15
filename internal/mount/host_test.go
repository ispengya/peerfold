package mount

import (
	"reflect"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

// countInterfaceMethods reports how many methods fuse.FileSystemInterface
// declares. It is read off the interface itself rather than from the adapter, so it
// says something about the library rather than about what the adapter embeds.
func countInterfaceMethods() int {
	return reflect.TypeOf((*fuse.FileSystemInterface)(nil)).Elem().NumMethod()
}

// The adapter must satisfy the interface it exists for. This is a compile-time
// assertion, and it is the whole checklist: a missing method is a build failure
// here rather than a surprise the first time a kernel calls into the mount.
var _ fuse.FileSystemInterface = (*cgofuseHost)(nil)

// A note on what this test is worth, because the obvious version of it is not
// worth anything.
//
// fuse.FileSystemBase is NOT an empty struct. It provides a default implementation
// of every method in the interface -- Init and Destroy do nothing and the rest
// return -ENOSYS (fsop.go:423-427). So a reflection count of the adapter's methods
// passes because of what it EMBEDS, and would go on passing if the adapter
// implemented nothing at all. It cannot fail, and a test that cannot fail is a
// liability rather than a check.
//
// What is actually asserted is the count of methods the interface declares, so a
// cgofuse upgrade that adds one is visible instead of silent, plus the two
// deliberate departures from the base: the operations the mount really serves, and
// the one it refuses with something other than ENOSYS.
func TestHostInterfaceShape(t *testing.T) {
	// 32, not the 33 the plan quotes. The plan counted the interface's method
	// declarations by eye and came out one over; the number is read off the
	// interface at run time, so it is the library's own count. Recording the
	// discrepancy rather than silently matching the plan is the point of asserting
	// a literal here at all.
	want := 32
	got := countInterfaceMethods()
	if got != want {
		t.Errorf("fuse.FileSystemInterface declares %d methods; this mount was "+
			"written against %d. If cgofuse was upgraded, check whether the new "+
			"methods matter for the mount before changing this number.", got, want)
	}
}

// The base class supplies -ENOSYS for everything, and the mount relies on that:
// these are operations a peerfold share has no concept of, and writing them out
// would be writing the same three characters repeatedly. The test checks the
// *behaviour* through the adapter, which is what the kernel sees.
func TestUnsupportedOperationsAreEnosys(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	cases := []struct {
		name string
		code int
	}{
		{"Mknod", h.Mknod("/a", 0, 0)},
		{"Link", h.Link("/a", "/b")},
		{"Symlink", h.Symlink("/a", "/b")},
		{"Chmod", h.Chmod("/a", 0o644)},
		{"Chown", h.Chown("/a", 0, 0)},
		{"Utimens", h.Utimens("/a", nil)},
		{"Access", h.Access("/a", 0)},
		{"Setxattr", h.Setxattr("/a", "user.x", nil, 0)},
		{"Removexattr", h.Removexattr("/a", "user.x")},
	}
	for _, c := range cases {
		if c.code != -ENOSYS {
			t.Errorf("%s = %d, want -ENOSYS (%d)", c.name, c.code, -ENOSYS)
		}
	}
}

// Getxattr is the one refusal that must NOT be ENOSYS. Some tools treat ENOSYS as
// a broken filesystem and refuse to copy into it, so "this filesystem has no
// extended attributes" is the answer that keeps them working.
func TestGetxattrIsNotEnosys(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	code, data := h.Getxattr("/a", "user.x")
	if code == -ENOSYS {
		t.Error("Getxattr answered ENOSYS; a tool that treats that as a broken " +
			"filesystem will refuse to copy into the mount")
	}
	if code != -ENOATTR {
		t.Errorf("Getxattr = %d, want -ENOATTR (%d)", code, -ENOATTR)
	}
	if data != nil {
		t.Errorf("Getxattr returned %d bytes with no attribute, want none", len(data))
	}
}

func TestListxattrIsEmptyNotEnosys(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	called := false
	code := h.Listxattr("/a", func(string) bool { called = true; return true })
	if code != 0 {
		t.Errorf("Listxattr = %d, want 0", code)
	}
	if called {
		t.Error("Listxattr called fill; there are no attributes to list")
	}
}

// The core's Stat and cgofuse's Stat_t are different structs, and every field the
// mount claims has to survive the crossing. The mode is the one that matters most:
// it is a bitfield, and a wrong translation makes a file look like a directory.
func TestStatConversionKeepsSizeModeAndMtime(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 42)
	fn := f.nodes["/a.txt"]
	fn.Mode = 0o100644
	fn.Mtime = 1700000000
	f.nodes["/a.txt"] = fn

	h := &cgofuseHost{m: m}
	var st fuse.Stat_t
	if code := h.Getattr("/a.txt", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d, want 0", code)
	}
	if st.Size != 42 {
		t.Errorf("Size = %d, want 42", st.Size)
	}
	if st.Mode&fuse.S_IFREG == 0 {
		t.Errorf("Mode = %o, want the regular-file bit set", st.Mode)
	}
	if st.Mode&fuse.S_IFDIR != 0 {
		t.Errorf("Mode = %o, a regular file must not carry the directory bit", st.Mode)
	}
	if st.Mtim.Sec != 1700000000 {
		t.Errorf("Mtim.Sec = %d, want 1700000000", st.Mtim.Sec)
	}
}

func TestStatConversionForADirectory(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	fn := f.nodes["/d"]
	fn.Mode = 0o040755
	f.nodes["/d"] = fn

	h := &cgofuseHost{m: m}
	var st fuse.Stat_t
	if code := h.Getattr("/d", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d, want 0", code)
	}
	if st.Mode&fuse.S_IFDIR == 0 {
		t.Errorf("Mode = %o, want the directory bit set", st.Mode)
	}
	if st.Nlink == 0 {
		t.Error("Nlink = 0; a directory with a zero link count confuses some tools")
	}
}

// cgofuse's Read returns a byte count, and so does the core. The adapter passes it
// through rather than re-encoding it -- re-encoding would turn "read 5 bytes" into
// "success", which a caller reading a short buffer cannot tell from a full one.
func TestReadReturnsByteCountNotStatus(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))

	h := &cgofuseHost{m: m}
	// cgofuse's Open returns the status FIRST and the handle second
	// (fsop.go:262), which is the reverse of every other layer in this package.
	// The two names below are therefore bound in the library's order, not the
	// core's: naming the first position "handle" would be a lie the compiler
	// cannot catch, because an int and a uint64 are not interchangeable but a
	// reader's assumption is.
	var status int
	var handle uint64
	status, handle = h.Open("/a.txt", OReadOnly)
	if status != 0 {
		t.Fatalf("Open = %d, want 0", status)
	}
	buf := make([]byte, 16)
	n := h.Read("/a.txt", buf, 0, handle)
	if n != 5 {
		t.Errorf("Read = %d, want 5: the return is a byte count, not a status", n)
	}
	if string(buf[:5]) != "hello" {
		t.Errorf("content = %q, want %q", buf[:5], "hello")
	}
	h.Release("/a.txt", handle)
}

// A read error must come back as a NEGATIVE errno, which is how cgofuse reports
// failure. A positive value would be read as a byte count.
func TestReadErrorIsNegative(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	// setReadFailure names a chunk offset, and offset 0 is deliberately not a
	// usable name for one: the fixture reserves it because the mount resolves a
	// path by listing the parent before it reads, so a failure pinned to offset 0
	// lands in resolution and the read under test never sees it. The first-read
	// trigger is the fixture's answer to that, and it is what a read of the first
	// chunk of a file actually is.
	f.setFirstReadFailure()

	h := &cgofuseHost{m: m}
	code, fh := h.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	buf := make([]byte, 16)
	if n := h.Read("/a.txt", buf, 0, fh); n >= 0 {
		t.Errorf("Read = %d, want a negative errno", n)
	}
	h.Release("/a.txt", fh)
}

func TestWriteReturnsByteCountNotStatus(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)

	h := &cgofuseHost{m: m}
	code, fh := h.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if n := h.Write("/a.txt", []byte("hello"), 0, fh); n != 5 {
		t.Errorf("Write = %d, want 5: the return is a byte count, not a status", n)
	}
	if code := h.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d, want 0", code)
	}
	if got := string(f.data["/a.txt"]); got != "hello" {
		t.Errorf("after Flush the owner has %q, want %q", got, "hello")
	}
	h.Release("/a.txt", fh)
}

// The core's fill callback carries a *Stat, cgofuse's carries a *fuse.Stat_t, and
// the offset travels with each entry. The translation is where a listing loses its
// attributes, so the attributes are what the test checks -- not just the names.
func TestReaddirTranslatesFillIncludingStat(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addFile("/d/a.txt", 7)
	fn := f.nodes["/d/a.txt"]
	fn.Mode = 0o100644
	f.nodes["/d/a.txt"] = fn

	h := &cgofuseHost{m: m}
	type entry struct {
		name string
		mode uint32
		size int64
	}
	var got []entry
	code := h.Readdir("/d", func(name string, st *fuse.Stat_t, ofst int64) bool {
		var mode uint32
		var size int64
		if st != nil {
			mode, size = st.Mode, st.Size
		}
		got = append(got, entry{name: name, mode: mode, size: size})
		return true
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir = %d, want 0", code)
	}

	var found bool
	for _, e := range got {
		if e.name == "a.txt" {
			found = true
			if e.mode&fuse.S_IFREG == 0 {
				t.Errorf("a.txt listed with mode %o, want the regular-file bit", e.mode)
			}
			if e.size != 7 {
				t.Errorf("a.txt listed with size %d, want 7", e.size)
			}
		}
	}
	if !found {
		t.Errorf("a.txt missing from the listing: %v", got)
	}
}

// Stopping a listing early must be honoured: cgofuse's fill returns false to say
// the buffer is full, and continuing to call it after that is writing past the end
// of a buffer the kernel sized.
func TestReaddirHonoursAnEarlyStop(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		f.addFile("/d/"+n+".txt", 1)
	}
	h := &cgofuseHost{m: m}

	calls := 0
	code := h.Readdir("/d", func(string, *fuse.Stat_t, int64) bool {
		calls++
		return calls < 2
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir = %d, want 0 even when stopped early", code)
	}
	if calls > 2 {
		t.Errorf("fill was called %d times after it asked to stop at 2", calls)
	}
}

// The fill callback receives nil stat for "." and "..", which is cgofuse's
// convention for entries it supplies no attributes for. Passing a zero Stat_t
// instead would claim both are mode-0 files.
func TestReaddirPassesNilStatForDotEntries(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	h := &cgofuseHost{m: m}

	nonNil := map[string]bool{}
	code := h.Readdir("/d", func(name string, st *fuse.Stat_t, ofst int64) bool {
		nonNil[name] = st != nil
		return true
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir = %d, want 0", code)
	}
	for _, dot := range []string{".", ".."} {
		if has, seen := nonNil[dot]; seen && has {
			t.Errorf("%q was given a stat; cgofuse wants nil for the dot entries", dot)
		}
	}
}

func TestStatfsConversion(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	var st fuse.Statfs_t
	if code := h.Statfs("/", &st); code != 0 {
		t.Fatalf("Statfs = %d, want 0", code)
	}
	if st.Bsize == 0 {
		t.Error("Bsize = 0; tools that divide by the block size will fail")
	}
	if st.Namemax == 0 {
		t.Error("Namemax = 0; some tools refuse to create anything without it")
	}
	if st.Frsize == 0 {
		t.Error("Frsize = 0")
	}
	// The figures have to stay ordered the way a real filesystem orders them.
	if st.Bavail > st.Bfree {
		t.Errorf("Bavail %d > Bfree %d", st.Bavail, st.Bfree)
	}
	if st.Bfree > st.Blocks {
		t.Errorf("Bfree %d > Blocks %d", st.Bfree, st.Blocks)
	}
	if st.Files == 0 {
		t.Error("Files = 0; some tools refuse to write")
	}
}

func TestStatfsOnAMissingPathIsAnErrno(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}
	var st fuse.Statfs_t
	if code := h.Statfs("/nope", &st); code != -ENOENT {
		t.Errorf("Statfs on a missing path = %d, want -ENOENT (%d)", code, -ENOENT)
	}
}

// Open and Opendir return the code first and the handle second, which is the
// opposite of the core's order. Getting it backwards hands the kernel a small
// integer as a file handle and an error code as a status.
func TestOpenAndOpendirUnpackInTheRightOrder(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.addDir("/d")
	h := &cgofuseHost{m: m}

	code, fh := h.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if fh == 0 {
		t.Error("Open returned handle 0 with success; 0 is the value callers use " +
			"to mean 'no handle', so a real handle must not be 0")
	}
	h.Release("/a.txt", fh)

	code, dh := h.Opendir("/d")
	if code != 0 {
		t.Fatalf("Opendir = %d, want 0", code)
	}
	if code := h.Releasedir("/d", dh); code != 0 {
		t.Errorf("Releasedir = %d, want 0", code)
	}
}

// A failure must not produce a plausible-looking handle alongside it.
func TestOpenFailureReturnsAMaximalHandleNotZero(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	code, fh := h.Open("/missing.txt", OReadOnly)
	if code != -ENOENT {
		t.Fatalf("Open = %d, want -ENOENT (%d)", code, -ENOENT)
	}
	if fh == 0 {
		t.Error("Open failed but returned handle 0, which is also what a " +
			"successful open of handle zero would look like")
	}
}

func TestCreateMkdirAndReleaseThroughHost(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	if code := h.Mkdir("/d", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d, want 0", code)
	}
	code, fh := h.Create("/d/new.txt", 0, 0o644)
	if code != 0 {
		t.Fatalf("Create = %d, want 0", code)
	}
	if n := h.Write("/d/new.txt", []byte("hi"), 0, fh); n != 2 {
		t.Fatalf("Write = %d, want 2", n)
	}
	if code := h.Release("/d/new.txt", fh); code != 0 {
		t.Fatalf("Release = %d, want 0", code)
	}
	if got := string(fakeOf(t, m).data["/d/new.txt"]); got != "hi" {
		t.Errorf("created file content = %q, want %q", got, "hi")
	}
}

func TestUnlinkRenameRmdirThroughHost(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.setContent("/a.txt", []byte("x"))
	h := &cgofuseHost{m: m}

	if code := h.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d, want 0", code)
	}
	if code := h.Unlink("/b.txt"); code != 0 {
		t.Fatalf("Unlink = %d, want 0", code)
	}
	if code := h.Mkdir("/empty", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d, want 0", code)
	}
	if code := h.Rmdir("/empty"); code != 0 {
		t.Fatalf("Rmdir = %d, want 0", code)
	}
}

// Fsync and Flush are both in the forced-flush table. Fsync must actually write:
// a program that calls fsync and then loses power is entitled to the data.
func TestFsyncWritesAndFlushSucceeds(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))
	h := &cgofuseHost{m: m}

	_, fh := h.Open("/a.txt", OReadWrite)
	h.Write("/a.txt", []byte("HELLO"), 0, fh)
	if code := h.Fsync("/a.txt", false, fh); code != 0 {
		t.Fatalf("Fsync = %d, want 0", code)
	}
	if got := string(f.data["/a.txt"]); got != "HELLO" {
		t.Errorf("Fsync did not write: owner has %q, want %q", got, "HELLO")
	}
	if code := h.Flush("/a.txt", fh); code != 0 {
		t.Fatalf("Flush = %d, want 0", code)
	}
	h.Release("/a.txt", fh)
}

func TestTruncateThroughHost(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 5)
	f.setContent("/a.txt", []byte("hello"))
	h := &cgofuseHost{m: m}

	_, fh := h.Open("/a.txt", OReadWrite)
	if code := h.Truncate("/a.txt", 3, fh); code != 0 {
		t.Fatalf("Truncate = %d, want 0", code)
	}
	if code := h.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d, want 0", code)
	}
	if got := string(f.data["/a.txt"]); got != "hel" {
		t.Errorf("after truncate to 3 the owner has %q, want %q", got, "hel")
	}
}

// Fsyncdir is not a separate core operation -- a directory has no content of its
// own -- but it must not fall through to the base class's ENOSYS either, because
// some tools call it and treat a refusal as a failure to save.
func TestFsyncdirSucceeds(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	h := &cgofuseHost{m: m}

	code, dh := h.Opendir("/d")
	if code != 0 {
		t.Fatalf("Opendir = %d, want 0", code)
	}
	if code := h.Fsyncdir("/d", false, dh); code == -ENOSYS {
		t.Error("Fsyncdir answered ENOSYS; a tool that syncs a directory before " +
			"renaming into it would treat that as a failure")
	}
	h.Releasedir("/d", dh)
}

// The invalidation hook is how the kernel is told its cached attributes are
// wrong, and it is the reason the handed-out table exists. NewHost must wire it.
func TestNewHostWiresInvalidationToNotify(t *testing.T) {
	seen := make(chan string, 8)
	m := newTestMount(t)
	m.invalidatePath = func(p string) bool { seen <- p; return true }

	// A path the kernel has been told about, then invalidated.
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d, want 0", code)
	}
	m.invalidateAll()

	select {
	case p := <-seen:
		if p != "/a.txt" {
			t.Errorf("invalidated %q, want %q", p, "/a.txt")
		}
	case <-time.After(time.Second):
		t.Error("invalidateAll did not reach the hook for a path the kernel was told about")
	}
}

// NewHost must hand back both the cgofuse host and the core it wraps: the runner
// needs the core to start the supervisor and flush on the way out, and a host
// alone would leave it reaching for a second Mount that is not the one mounted.
func TestNewHostReturnsTheCoreItWraps(t *testing.T) {
	f := newFakeCaller()
	f.addDir("/")
	host, m := NewHost(f, DefaultOptions())
	if host == nil {
		t.Fatal("NewHost returned a nil host")
	}
	if m == nil {
		t.Fatal("NewHost returned a nil core")
	}
	if m.call() != Caller(f) {
		t.Error("the returned core is not the one built over the caller that was passed")
	}
}

// INIT and DESTROY are the mount's lifetime. Init starts the watch, and Destroy
// stops it and makes a final attempt to save: neither unmounting nor a signal is a
// crash, and both are chances to keep what the user typed.
func TestInitStartsTheWatchAndDestroyFlushes(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.watchForTest()

	h := &cgofuseHost{m: m}
	h.Init()

	m.mu.Lock()
	started := m.watchDone != nil
	m.mu.Unlock()
	if !started {
		t.Error("Init did not start the watch loop")
	}

	// A dirty handle must be saved by Destroy.
	code, fh := h.Create("/a.txt", 0, 0o644)
	if code != 0 {
		t.Fatalf("Create = %d, want 0", code)
	}
	h.Write("/a.txt", []byte("bye"), 0, fh)

	h.Destroy()

	if got := string(f.data["/a.txt"]); got != "bye" {
		t.Errorf("Destroy left the owner with %q, want %q: an unmount is a chance "+
			"to save, not a reason to lose the edit", got, "bye")
	}
	m.mu.Lock()
	stopped := m.watchDone == nil
	m.mu.Unlock()
	if !stopped {
		t.Error("Destroy did not stop the watch loop")
	}
}
