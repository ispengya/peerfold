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
	if code := h.Mknod("/a", 0, 0); code != -ENOSYS {
		t.Errorf("Mknod = %d, want -ENOSYS (%d)", code, -ENOSYS)
	}
	if code := h.Bmap("/a", 0, nil); code != -ENOSYS {
		t.Errorf("Bmap = %d, want -ENOSYS (%d)", code, -ENOSYS)
	}
}

// The trace hook is installed before NewHost runs in the real-mount helper, so
// NewHost must carry the callback into the adapter rather than dropping it.
func TestNewHostCarriesTheTrace(t *testing.T) {
	var got []string
	SetHostTrace(func(op, result, path string) { got = append(got, op+":"+path) })
	defer SetHostTrace(nil)

	_, _ = NewHost(newFakeCaller(), DefaultOptions())
	if len(got) == 0 || got[len(got)-1] != "NewHost:" {
		t.Fatalf("NewHost trace = %v, want the last entry to be NewHost", got)
	}
}

// Windows asks for volume and extended-attribute metadata during an ordinary
// create, and WinFsp turns ENOSYS from these callbacks into a failed create.
// The mount has no volume label or extended attributes, so harmless success is
// the answer that keeps the save alive.
func TestWindowsCreateMetadataDoesNotFail(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}
	if code := h.Getvolume("/a", &fuse.Statfs_t{}); code == -ENOSYS {
		t.Error("Getvolume answered ENOSYS; Windows asks for volume information during create and an ENOSYS fails it")
	}
	if code := h.SetvolumeLabel("/a", "peerfold"); code == -ENOSYS {
		t.Error("SetvolumeLabel answered ENOSYS; Windows asks for volume information during create and an ENOSYS fails it")
	}
	if code, _ := h.Getea("/a", "user.x"); code == -ENOSYS {
		t.Error("Getea answered ENOSYS; Windows can ask for extended attributes during create and an ENOSYS fails it")
	}
	if code := h.Listea("/a", func(string) bool { return true }); code == -ENOSYS {
		t.Error("Listea answered ENOSYS; Windows can ask for extended attributes during create and an ENOSYS fails it")
	}
	if code := h.Geteainfo("/a", nil); code == -ENOSYS {
		t.Error("Geteainfo answered ENOSYS; Windows can ask for extended attributes during create and an ENOSYS fails it")
	}
	if code := h.Seteainfo("/a", nil); code == -ENOSYS {
		t.Error("Seteainfo answered ENOSYS; Windows can ask for extended attributes during create and an ENOSYS fails it")
	}
	if code, _ := h.Getpath3("/a", 0); code == -ENOSYS {
		t.Error("Getpath3 answered ENOSYS; Windows can normalize case during create and an ENOSYS fails it")
	}
	if code := h.Setea("/a", "user.x", []byte("x"), 0); code != 0 {
		t.Errorf("Setea = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setea2("/a", "user.x", []byte("x"), 0, nil); code != 0 {
		t.Errorf("Setea2 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setea3("/a", "user.x", []byte("x"), 0, nil); code != 0 {
		t.Errorf("Setea3 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
}

// Symlink creation is refused rather than inherited from the base class's
// ENOSYS. Windows turns that ENOSYS into STATUS_NOT_SUPPORTED, which the Win32
// layer reports as a missing path, so a symlink is the only refusal that would
// be indistinguishable from a failed save.
func TestSymlinkIsRefusedWithoutENOSYS(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}
	if code := h.Symlink("/a", "/b"); code == -ENOSYS {
		t.Error("Symlink answered ENOSYS; Windows reports that as a missing path rather than as an unsupported operation")
	}
	if code := h.Link("/a", "/b"); code == -ENOSYS {
		t.Error("Link answered ENOSYS; Windows reports that as a missing path rather than as an unsupported operation")
	}
	if code, _ := h.Getpath("/a", 0); code == -ENOSYS {
		t.Error("Getpath answered ENOSYS; Windows uses it to normalize case before ordinary writes and reports ENOSYS as a missing path")
	}
}

// Windows can change a file's basic attributes during creation. WinFsp turns an
// ENOSYS from these callbacks into a failed create, so the mount has to say
// "unsupported but harmless" rather than "not supported" for the operations
// Windows issues as part of an ordinary create.
func TestCreateTimeOperationsDoNotFailTheMount(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}

	if code := h.Chmod("/a", 0o644); code != 0 {
		t.Errorf("Chmod = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Chown("/a", 0, 0); code != 0 {
		t.Errorf("Chown = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Utimens("/a", []fuse.Timespec{{Sec: 1}, {Sec: 1}}); code != 0 {
		t.Errorf("Utimens = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Chflags("/a", 0); code != 0 {
		t.Errorf("Chflags = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setcrtime("/a", fuse.Timespec{Sec: 1}); code != 0 {
		t.Errorf("Setcrtime = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setchgtime("/a", fuse.Timespec{Sec: 1}); code != 0 {
		t.Errorf("Setchgtime = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setvolume("/a", "peerfold"); code != 0 {
		t.Errorf("Setvolume = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setea("/a", "user.x", []byte("x"), 0); code != 0 {
		t.Errorf("Setea = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setea2("/a", "user.x", []byte("x"), 0, nil); code != 0 {
		t.Errorf("Setea2 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setea3("/a", "user.x", []byte("x"), 0, nil); code != 0 {
		t.Errorf("Setea3 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Mkdir("/a", 0o755); code != 0 {
		t.Errorf("Mkdir = %d, want 0: Windows sends directory creation during create and an ENOSYS fails it", code)
	}
	if code := h.Setchgtime3("/a", fuse.Timespec{Sec: 1}, 0); code != 0 {
		t.Errorf("Setchgtime3 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setcrtime3("/a", fuse.Timespec{Sec: 1}, 0); code != 0 {
		t.Errorf("Setcrtime3 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
	}
	if code := h.Setbkuptime3("/a", fuse.Timespec{Sec: 1}, 0); code != 0 {
		t.Errorf("Setbkuptime3 = %d, want 0: Windows sends this during create and an ENOSYS fails the create", code)
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

// A Windows create can carry extended attributes. WinFsp treats an ENOSYS from
// Setxattr during Create as a failure of the whole create, so the adapter must
// report "no attributes supported" without making the creation fail.
func TestSetxattrDuringCreateDoesNotFailTheMount(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}
	code := h.Setxattr("/a", "$KERNEL.PURGE.SEC.FILEHASH", []byte("hash"), 0)
	if code == -ENOSYS {
		t.Error("Setxattr answered ENOSYS; WinFsp turns that into a failed create when Windows supplies extended attributes")
	}
	if code != 0 {
		t.Errorf("Setxattr = %d, want 0: this mount has no extended attributes and Windows ignores the harmless success", code)
	}
	if code := h.Removexattr("/a", "user.x"); code != 0 {
		t.Errorf("Removexattr = %d, want 0: this mount has no extended attributes and Windows ignores the harmless success", code)
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

// WinFsp gates the transition from a failed lookup to Create on the parent's
// write permission. A directory reported as 0555 makes the kernel refuse the
// create before cgofuse ever receives it, so the adapter must always expose a
// writable directory to the platform.
func TestGetattrReportsDirectoriesAsWritable(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/readonly")
	n := f.nodes["/readonly"]
	n.Mode = 0o040555
	f.nodes["/readonly"] = n

	h := &cgofuseHost{m: m}
	var st fuse.Stat_t
	if code := h.Getattr("/readonly", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d, want 0", code)
	}
	if st.Mode&fuse.S_IWUSR == 0 {
		t.Errorf("directory Mode = %o, want the owner write bit so Windows can create inside it", st.Mode)
	}
}

// The adapter must widen the mode to owner, group and world together, not just
// the owner. This mount sets no uid/gid, so WinFsp's user-mode library maps
// uid=0 to the "Nobody" SID (S-1-5-0) that never matches the caller's token;
// the only ACE that can grant the caller access is the world (Everyone,
// S-1-1-0) ACE, which carries the world permission bits. Widening owner alone
// (0700) leaves the world bits at zero, so an open of a freshly created file is
// refused with ACCESS_DENIED before any FUSE callback runs.
func TestIntToStatWidensToWorldReadWriteExecute(t *testing.T) {
	in := Stat{Mode: modeRegular | 0o600, Size: 1, Mtime: 1700000000, Nlink: 1}
	st := intToStat(&in)
	if st.Mode&fuse.S_IRWXU != fuse.S_IRWXU {
		t.Errorf("owner bits = %o, want rwx", st.Mode&fuse.S_IRWXU)
	}
	if st.Mode&fuse.S_IRWXG != fuse.S_IRWXG {
		t.Errorf("group bits = %o, want rwx", st.Mode&fuse.S_IRWXG)
	}
	if st.Mode&fuse.S_IRWXO != fuse.S_IRWXO {
		t.Errorf("world bits = %o, want rwx: the caller reaches the mount as the world principal", st.Mode&fuse.S_IRWXO)
	}
}

// A root without S_IFDIR makes WinFsp reject the entire mount before the
// filesystem starts, which is why the root's type field must survive the
// adapter even when its permission bits are configured as a plain 0755.
func TestGetattrReportsTheRootAsADirectory(t *testing.T) {
	m := newTestMount(t)
	h := &cgofuseHost{m: m}
	var st fuse.Stat_t
	if code := h.Getattr("/", &st, 0); code != 0 {
		t.Fatalf("Getattr = %d, want 0", code)
	}
	if st.Mode&fuse.S_IFDIR == 0 {
		t.Errorf("root Mode = %o, want the directory type bit", st.Mode)
	}
	if st.Mode&fuse.S_IWUSR == 0 {
		t.Errorf("root Mode = %o, want the owner write bit so Windows can create at the root", st.Mode)
	}
	if st.Mode&fuse.S_IFMT != fuse.S_IFDIR {
		t.Errorf("root Mode = %o, want exactly one directory type field", st.Mode)
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

// A Create carries flags from the kernel in addition to the create bit. The
// mount's operation core must receive that flag word intact: the adapter is the
// only place that sees cgofuse's representation, and dropping the flags would
// turn a normal save into an unconditional overwrite without telling anybody.
func TestCreateHonoursReadOnlyFlagsFromTheKernel(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")

	h := &cgofuseHost{m: m}
	status, fh := h.Create("/d/new.txt", fuse.O_RDONLY|fuse.O_CREAT, 0o644)
	if status != 0 {
		t.Fatalf("Create = %d, want 0", status)
	}
	if code := h.Write("/d/new.txt", []byte("x"), 0, fh); code != -EBADF {
		t.Errorf("Write through an O_RDONLY Create = %d, want -EBADF (%d)", code, -EBADF)
	}
	h.Release("/d/new.txt", fh)
}

func TestCreateReportsAFileHandleToTheKernel(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")

	h := &cgofuseHost{m: m}
	status, fh := h.Create("/d/new.txt", fuse.O_WRONLY|fuse.O_CREAT|fuse.O_TRUNC, 0o644)
	if status != 0 {
		t.Fatalf("Create = %d, want 0", status)
	}
	if fh == 0 {
		t.Error("Create returned handle 0; cgofuse treats that as the first real handle and Windows reports the create as missing")
	}
	if status, fh = h.Create("/d", fuse.O_RDONLY|fuse.O_CREAT, 0o755); status != -EISDIR {
		t.Errorf("Create on a directory = status %d, want -EISDIR (%d)", status, -EISDIR)
	}
	if fh != noHandle {
		t.Errorf("failed Create handle = %#x, want cgofuse's noHandle %#x", fh, noHandle)
	}
}

func TestGetattrAfterCreateDoesNotReportThePendingFileMissing(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")

	h := &cgofuseHost{m: m}
	status, fh := h.Create("/d/new.txt", fuse.O_WRONLY|fuse.O_CREAT|fuse.O_TRUNC, 0o644)
	if status != 0 {
		t.Fatalf("Create = %d, want 0", status)
	}

	// WinFsp asks for attributes immediately after Create, before the file has
	// been uploaded to the server's index. A missing answer there makes the
	// whole create look not to exist to the caller.
	var st fuse.Stat_t
	if code := h.Getattr("/d/new.txt", &st, fh); code != 0 {
		t.Fatalf("Getattr after Create = %d, want 0", code)
	}
	h.Release("/d/new.txt", fh)
}

func TestCreatePassesTheKernelFlagsThrough(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")

	h := &cgofuseHost{m: m}
	status, fh := h.Create("/d/new.txt", fuse.O_WRONLY|fuse.O_CREAT|fuse.O_TRUNC, 0o644)
	if status != 0 {
		t.Fatalf("Create = %d, want 0", status)
	}
	_ = fh
	if !f.callsWithPrefix("List /d") {
		t.Errorf("Create did not resolve the parent directory; calls were %v", f.callLog())
	}
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

func TestPendingGetattrFillsTheStatStructure(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)

	h := &cgofuseHost{m: m}
	status, fh := h.Open("/a.txt", OReadOnly)
	if status != 0 {
		t.Fatalf("Open = %d, want 0", status)
	}
	var st fuse.Stat_t
	if code := h.Getattr("/a.txt", &st, fh); code != 0 {
		t.Fatalf("Getattr after Open = %d, want 0", code)
	}
	if st.Mode&fuse.S_IFREG == 0 {
		t.Errorf("pending Getattr mode = %#o, want a regular file", st.Mode)
	}
	if st.Size != 0 {
		t.Errorf("pending Getattr size = %d, want 0", st.Size)
	}
	h.Release("/a.txt", fh)
}

func TestGetattrForAnOpenFileDoesNotReportTheFileMissing(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", []byte(""))

	h := &cgofuseHost{m: m}
	status, fh := h.Open("/a.txt", OReadOnly)
	if status != 0 {
		t.Fatalf("Open = %d, want 0", status)
	}

	// WinFsp asks for attributes after opening the handle. The owner's index may
	// still be between the local flush and the watcher push, so the adapter
	// must answer from the open handle rather than report the file missing.
	var st fuse.Stat_t
	if code := h.Getattr("/a.txt", &st, fh); code != 0 {
		t.Fatalf("Getattr after Open = %d, want 0", code)
	}
	h.Release("/a.txt", fh)
}

func TestMkdirNotifiesTheKernelAboutItsParent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	notified := make(chan string, 1)
	m.invalidatePath = func(p string) bool {
		notified <- p
		return true
	}

	if code := m.Mkdir("/d/new", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d, want 0", code)
	}
	select {
	case p := <-notified:
		if p != "/d/new" {
			t.Errorf("invalidated %q, want the new directory /d/new", p)
		}
	default:
		t.Error("Mkdir did not invalidate the new directory; Windows keeps the stale negative lookup and refuses the next child create")
	}
}

func TestMkdirTwiceWhilePendingIsEexist(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")

	if code := m.Mkdir("/d/new-dir", 0o755); code != 0 {
		t.Fatalf("first Mkdir = %d, want 0", code)
	}
	if code := m.Mkdir("/d/new-dir", 0o755); code != -EEXIST {
		t.Errorf("second Mkdir = %d, want -EEXIST (%d)", code, -EEXIST)
	}
}

func TestWriteWithNoHandleForAPendingDirectoryIsRefused(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/parent")

	h := &cgofuseHost{m: m}
	if code := h.Mkdir("/parent/new-dir", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d, want 0", code)
	}
	if status, fh := h.Opendir("/parent/new-dir"); status != 0 || fh == noHandle {
		t.Fatalf("Opendir pending directory = (%d, %#x), want (0, a real handle)", status, fh)
	}
	status, fh := h.Open("/parent/new-dir", OReadOnly)
	if status != -EISDIR {
		t.Errorf("Open of a pending directory = %d, want -EISDIR (%d)", status, -EISDIR)
	}
	if fh != noHandle {
		t.Errorf("failed Open handle = %#x, want cgofuse's noHandle %#x", fh, noHandle)
	}
	if code := h.Write("/parent/new-dir", []byte("x"), 0, noHandle); code != -EBADF {
		t.Errorf("Write with the directory sentinel handle = %d, want -EBADF (%d)", code, -EBADF)
	}
	if code := h.Flush("/parent/new-dir", noHandle); code != 0 {
		t.Errorf("Flush with the directory sentinel handle = %d, want 0", code)
	}
}

func TestOpendirAfterMkdirDoesNotReportThePendingDirectoryMissing(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/parent")

	h := &cgofuseHost{m: m}
	if code := h.Mkdir("/parent/new-dir", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d, want 0", code)
	}

	// WinFsp opens the directory immediately after creating it. The directory is
	// not in the server's index until the client's watcher pushes it, so the
	// adapter must not answer that first Opendir with ENOENT.
	status, fh := h.Opendir("/parent/new-dir")
	if status != 0 {
		t.Fatalf("Opendir after Mkdir = %d, want 0", status)
	}
	h.Releasedir("/parent/new-dir", fh)
}

func TestCreateMkdirAndReleaseThroughHost(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	h := &cgofuseHost{m: m}

	if code := h.Mkdir("/d", 0o755); code != -EEXIST {
		t.Fatalf("Mkdir = %d, want 0", code)
	}
	code, fh := h.Create("/d/new.txt", fuse.O_WRONLY|fuse.O_CREAT, 0o644)
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
	code, fh := h.Create("/a.txt", fuse.O_WRONLY|fuse.O_CREAT, 0o644)
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
