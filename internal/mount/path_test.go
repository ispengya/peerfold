package mount

import (
	"testing"

	"github.com/ispengya/peerfold/internal/node"
)

// TestLookupReusesTheDirectoryListing pins the metadata cache: resolving two
// paths in one directory must transfer that directory once, not once per path.
// Before the cache every Getattr transferred the whole parent listing.
func TestLookupReusesTheDirectoryListing(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 4)
	f.addFile("/b.txt", 4)

	if _, code := m.lookup("/a.txt"); code != 0 {
		t.Fatalf("lookup(/a.txt) = %d", code)
	}
	afterFirst := f.countCalls("List /")
	if afterFirst == 0 {
		t.Fatal("the first lookup did not list the parent")
	}

	if _, code := m.lookup("/b.txt"); code != 0 {
		t.Fatalf("lookup(/b.txt) = %d", code)
	}
	for i := 0; i < 3; i++ {
		if _, code := m.lookup("/a.txt"); code != 0 {
			t.Fatalf("repeat lookup(/a.txt) = %d", code)
		}
	}
	if got := f.countCalls("List /"); got != afterFirst {
		t.Fatalf("repeated lookups made %d extra List calls, want 0", got-afterFirst)
	}
}

// TestListingCacheIsInvalidatedByAPathChange pins the other half: a mutation
// must not leave a stale listing answering lookups for the rest of the TTL.
func TestListingCacheIsInvalidatedByAPathChange(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 4)

	if _, code := m.lookup("/a.txt"); code != 0 {
		t.Fatalf("lookup(/a.txt) = %d", code)
	}
	before := f.countCalls("List /")

	f.addFile("/b.txt", 4)
	m.invalidateListings("/b.txt")

	if _, code := m.lookup("/b.txt"); code != 0 {
		t.Fatalf("lookup(/b.txt) after invalidation = %d", code)
	}
	if got := f.countCalls("List /"); got <= before {
		t.Fatalf("the invalidated parent was not listed again (List calls %d, want > %d)", got, before)
	}
}

// readdirNames collects the names Readdir reports for dir.
func readdirNames(t *testing.T, m *Mount, dir string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	code := m.Readdir(dir, func(name string, _ *Stat, _ int64) bool {
		names[name] = true
		return true
	}, 0, 0)
	if code != 0 {
		t.Fatalf("Readdir(%q) = %d", dir, code)
	}
	return names
}

// TestReaddirShowsAPendingCreate pins the local-create window: the owner's
// watcher has not pushed the new name yet, so the server's listing does not
// contain it. The directory listing must still agree with the create the
// caller just performed.
func TestReaddirShowsAPendingCreate(t *testing.T) {
	m := newTestMount(t)
	if _, code := m.Create("/new.txt", 0o644, OReadWrite); code != 0 {
		t.Fatalf("Create = %d", code)
	}
	if !readdirNames(t, m, "/")["new.txt"] {
		t.Fatal("Readdir does not show the file that was just created")
	}
}

// TestReaddirHidesATombstonedName pins the local-delete window: the cached
// listing still contains the name, and the server still will until the owner's
// event arrives. The tombstone must win in the listing, as it already does in
// lookup.
func TestReaddirHidesATombstonedName(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 4)

	// Populate the listing cache from the caller before deleting locally.
	if _, code := m.lookup("/a.txt"); code != 0 {
		t.Fatalf("lookup(/a.txt) = %d", code)
	}

	m.mu.Lock()
	m.events["/a.txt"] = node.Node{Path: "/a.txt", Kind: node.KindDeleted}
	m.mu.Unlock()

	if readdirNames(t, m, "/")["a.txt"] {
		t.Fatal("Readdir brought back a name with a local tombstone")
	}
}

func TestCleanRejectsEscape(t *testing.T) {
	for _, in := range []string{"/../etc/passwd", "/a/../../b", "..", "/.."} {
		if got, err := Clean(in); err == nil {
			t.Errorf("Clean(%q) = %q, want an error (it escapes the root)", in, got)
		}
	}
}

func TestCleanAccepts(t *testing.T) {
	cases := map[string]string{
		"/":          "/",
		"":           "/",
		"/a":         "/a",
		"/a/b.txt":   "/a/b.txt",
		"/a//b":      "/a/b",
		"/a/./b":     "/a/b",
		"/a/b/":      "/a/b",
		"/中文/文件.txt": "/中文/文件.txt",
	}
	for in, want := range cases {
		got, err := Clean(in)
		if err != nil {
			t.Errorf("Clean(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitParent(t *testing.T) {
	parent, base, err := SplitParent("/a/b.txt")
	if err != nil || parent != "/a" || base != "b.txt" {
		t.Errorf("SplitParent(/a/b.txt) = %q, %q, %v", parent, base, err)
	}
	if _, _, err := SplitParent("/"); err == nil {
		t.Error("SplitParent(/) must fail: the root has no parent")
	}
}

func TestLookupRejectsSymlink(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")
	f.addSymlink("/a/link")
	f.addFile("/a/real.txt", 3)

	if _, code := m.lookup("/a/link"); code != -ELOOP {
		t.Errorf("lookup(symlink) = %d, want -ELOOP (%d)", code, -ELOOP)
	}
	if _, code := m.lookup("/a/real.txt"); code != 0 {
		t.Errorf("lookup(regular) = %d, want 0", code)
	}
}

func TestLookupMissingAndNotDir(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a/real.txt", 3)

	if _, code := m.lookup("/a/nope"); code != -ENOENT {
		t.Errorf("lookup(missing) = %d, want -ENOENT", code)
	}
	if _, code := m.lookup("/a/real.txt/x"); code != -ENOTDIR {
		t.Errorf("lookup(under file) = %d, want -ENOTDIR", code)
	}
}

// A name this mount created but whose owning client has not pushed back yet is
// not in the index, but it is a real directory in the namespace Windows is
// already using. Windows revalidates the new path -- and every path beneath it
// -- immediately after Mkdir succeeds, before the client's watcher has had a
// chance to report the new node to the server. Answering ENOENT there makes the
// first child create in a freshly created directory fail with a path that looks
// missing, even though the caller just created it.
func TestLookupSeesAPendingDirectory(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if code := m.Mkdir("/a/sub", 0o755); code != 0 {
		t.Fatalf("Mkdir(/a/sub) = %d, want 0", code)
	}

	if n, code := m.lookup("/a/sub"); code != 0 || n.Kind != node.KindDir {
		t.Errorf("lookup(pending directory) = %+v, %d; want a directory and 0", n, code)
	}
	// The pending name is a real directory, so a path beneath it is a path
	// beneath a directory -- the same walk the index would answer once the
	// owner's push arrived.
	if _, code := m.lookup("/a/sub/child.txt"); code != -ENOENT {
		t.Errorf("lookup(child of pending directory) = %d, want -ENOENT for the "+
			"missing child itself, not for its (existing) parent", code)
	}
	// A name that is genuinely absent under the pending directory is still
	// ENOENT, so the pending window does not invent entries.
	if _, code := m.lookup("/a/sub/deeper/nope.txt"); code != -ENOENT {
		t.Errorf("lookup(missing child of pending directory) = %d, want -ENOENT", code)
	}
}

// A name this mount created but whose owning client has not pushed back yet is
// a real directory, so listing it must not answer ENOTDIR. The listing is
// empty because the owner has not reported any children yet, but empty is a
// directory listing, and ENOTDIR is not.
func TestReaddirSeesAPendingDirectory(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if code := m.Mkdir("/a/sub", 0o755); code != 0 {
		t.Fatalf("Mkdir(/a/sub) = %d, want 0", code)
	}

	var names []string
	if code := m.Readdir("/a/sub", func(name string, st *Stat, off int64) bool {
		names = append(names, name)
		return true
	}, 0, 0); code != 0 {
		t.Errorf("Readdir(pending directory) = %d, want 0", code)
	}
	if len(names) != 2 || names[0] != "." || names[1] != ".." {
		t.Errorf("Readdir(pending directory) = %v, want the two dot entries and "+
			"nothing else: the owner has not reported any children yet", names)
	}
}

// A file this mount created but whose owning client has not pushed back yet is
// a real file in the namespace Windows is already using. Windows revalidates
// the new path immediately after Create succeeds, before the client's watcher
// has reported it; answering ENOENT there makes the open appear not to exist
// even though Create succeeded a moment ago.
func TestOpenSeesAPendingFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if fh, code := m.Create("/a/new.txt", 0o644, OReadWrite); code != 0 {
		t.Fatalf("Create(/a/new.txt) = %d, want 0", code)
	} else if fh == 0 {
		t.Fatal("Create returned a zero handle with a zero code")
	}

	if fh, code := m.Open("/a/new.txt", OReadOnly); code != 0 {
		t.Errorf("Open(pending file) = %d, want 0: the name this mount just "+
			"created is a real file until the owner's push arrives", code)
	} else if fh == 0 {
		t.Error("Open returned a zero handle with a zero code")
	}
	// The pending name is a real file, so a path beneath it is a path beneath a
	// file, and ENOTDIR is the answer -- the same as for a file the index knows
	// about.
	if _, code := m.Open("/a/new.txt/child", OReadOnly); code != -ENOTDIR {
		t.Errorf("Open(beneath a pending file) = %d, want -ENOTDIR", code)
	}
	// A name that is genuinely absent is still ENOENT, so the pending window
	// does not invent files.
	if _, code := m.Open("/a/still-missing.txt", OReadOnly); code != -ENOENT {
		t.Errorf("Open(missing file) = %d, want -ENOENT", code)
	}
}

// A file this mount created but whose owning client has not pushed back yet is
// a real file, so a stat must not answer ENOENT. Windows asks for attributes
// immediately after Create, and ENOENT there makes the whole open appear not to
// exist even though Create succeeded.
func TestGetattrSeesAPendingFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if fh, code := m.Create("/a/new.txt", 0o644, OReadWrite); code != 0 {
		t.Fatalf("Create(/a/new.txt) = %d, want 0", code)
	} else if fh == 0 {
		t.Fatal("Create returned a zero handle with a zero code")
	}

	var st Stat
	if code := m.Getattr("/a/new.txt", &st, 0); code != 0 {
		t.Errorf("Getattr(pending file) = %d, want 0", code)
	}
	if st.Size != 0 || st.Mode&modeTypeMask != modeRegular {
		t.Errorf("Getattr(pending file) = %+v, want an empty regular file", st)
	}
}

// A file this mount created but whose owning client has not pushed back yet is
// a real file, so writing to it by path -- which cgofuse can deliver after a
// connection was replaced -- must not answer ENOENT.
func TestWriteThroughPathSeesAPendingFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if fh, code := m.Create("/a/new.txt", 0o644, OReadWrite); code != 0 {
		t.Fatalf("Create(/a/new.txt) = %d, want 0", code)
	} else if fh == 0 {
		t.Fatal("Create returned a zero handle with a zero code")
	}

	// The write arrives with a handle the mount does not recognise -- which is
	// what cgofuse delivers after a connection was replaced, and the reason the
	// path fallback exists. The directory sentinel (noHandle) is deliberately
	// not that case: it has no path to resolve and is refused with EBADF by
	// TestWriteWithNoHandleForAPendingDirectoryIsRefused.
	if n := m.Write("/a/new.txt", []byte("hello"), 0, 9999); n != 5 {
		t.Errorf("Write(pending file, unknown handle) = %d, want 5", n)
	}
	if got := f.data["/a/new.txt"]; string(got) != "hello" {
		t.Errorf("the owner's copy is %q, want %q", got, "hello")
	}
}

// A file this mount created but whose owning client has not pushed back yet is
// a real file, so unlinking it must remove it -- and the name must stop being
// pending the moment the removal succeeds, or lookup would go on answering for
// a name that is gone until the client's watcher reported the removal.
func TestUnlinkClearsThePendingFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if fh, code := m.Create("/a/new.txt", 0o644, OReadWrite); code != 0 {
		t.Fatalf("Create(/a/new.txt) = %d, want 0", code)
	} else if fh == 0 {
		t.Fatal("Create returned a zero handle with a zero code")
	}
	if code := m.Unlink("/a/new.txt"); code != 0 {
		t.Fatalf("Unlink(pending file) = %d, want 0", code)
	}
	if _, code := m.lookup("/a/new.txt"); code != -ENOENT {
		t.Errorf("lookup after Unlink = %d, want -ENOENT: the name this mount "+
			"created and then removed must not stay pending", code)
	}
}

// A directory this mount created but whose owning client has not pushed back
// yet is a real directory, so rmdir must remove it -- and the name must stop
// being pending the moment the removal succeeds.
func TestRmdirClearsThePendingDirectory(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")

	if code := m.Mkdir("/a/sub", 0o755); code != 0 {
		t.Fatalf("Mkdir(/a/sub) = %d, want 0", code)
	}
	if code := m.Rmdir("/a/sub"); code != 0 {
		t.Fatalf("Rmdir(pending directory) = %d, want 0", code)
	}
	if _, code := m.lookup("/a/sub"); code != -ENOENT {
		t.Errorf("lookup after Rmdir = %d, want -ENOENT: the name this mount "+
			"created and then removed must not stay pending", code)
	}
}

// A rename of a name this mount created must move the pending entry with it.
// Dropping it would put the very window the pending table exists for back in
// the wrong place: the destination is a real name until the owning client
// reports the move.
func TestRenameMovesThePendingEntry(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")
	f.addDir("/b")

	if fh, code := m.Create("/a/old.txt", 0o644, OReadWrite); code != 0 {
		t.Fatalf("Create(/a/old.txt) = %d, want 0", code)
	} else if fh == 0 {
		t.Fatal("Create returned a zero handle with a zero code")
	}
	if code := m.Rename("/a/old.txt", "/b/new.txt"); code != 0 {
		t.Fatalf("Rename(/a/old.txt -> /b/new.txt) = %d, want 0", code)
	}
	if _, code := m.lookup("/a/old.txt"); code != -ENOENT {
		t.Errorf("lookup(source after Rename) = %d, want -ENOENT", code)
	}
	if n, code := m.lookup("/b/new.txt"); code != 0 || n.Kind != node.KindFile {
		t.Errorf("lookup(destination after Rename) = %+v, %d; want a file and 0: "+
			"the pending entry has to move with the name", n, code)
	}
}

func TestLookupRoot(t *testing.T) {
	m := newTestMount(t)
	n, code := m.lookup("/")
	if code != 0 || n.Kind != node.KindDir {
		t.Errorf("lookup(/) = %+v, %d; want a directory and 0", n, code)
	}
	// The root's mode comes from the options, since it is synthesised and there
	// is no index entry to read one from.
	if n.Mode != m.opts.RootMode {
		t.Errorf("lookup(/) mode = %o, want the configured %o", n.Mode, m.opts.RootMode)
	}
}

// The walk descends one component at a time, so a deep path exercises every
// step of it. A deep path also exposes an asymmetry in the layer below that is
// worth pinning rather than assuming, because it is narrower than POSIX:
// ENOTDIR is reported when the immediate parent is a file, and ENOENT when a
// file sits further up the path. See TestFakeListMirrorsTheIndex for the
// measurement that establishes it and internal/index/index.go for why.
func TestLookupWalksEveryComponent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a/b/c/d.txt", 4)

	for _, p := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d.txt"} {
		n, code := m.lookup(p)
		if code != 0 {
			t.Errorf("lookup(%q) = %d, want 0", p, code)
			continue
		}
		if n.Path != p {
			t.Errorf("lookup(%q) resolved to %q", p, n.Path)
		}
	}

	// Every prefix of an existing path must exist, and a name under a
	// directory that does not exist is ENOENT no matter how deep it is.
	for _, p := range []string{"/zzz", "/zzz/yyy", "/zzz/yyy/xxx"} {
		if _, code := m.lookup(p); code != -ENOENT {
			t.Errorf("lookup(%q) = %d, want -ENOENT", p, code)
		}
	}

	// One level under a file: the parent IS the file, so the parent cannot be
	// listed and ENOTDIR is the answer.
	if _, code := m.lookup("/a/b/c/d.txt/x"); code != -ENOTDIR {
		t.Errorf("lookup(file parent) = %d, want -ENOTDIR", code)
	}
	// Two levels under a file. The parent "/a/b/c/d.txt/x" does not exist as a
	// node, so the index answers "not found" and that is what comes back. It
	// is not ENOTDIR, and a test demanding ENOTDIR here would be demanding
	// behaviour the real stack cannot produce.
	if _, code := m.lookup("/a/b/c/d.txt/x/y"); code != -ENOENT {
		t.Errorf("lookup(two levels under a file) = %d, want -ENOENT: "+
			"the index does not walk ancestors, so only the immediate parent is diagnosed", code)
	}
}

// A name must match exactly. A child whose name ends with the segment's name
// must not answer for it, which is what a suffix comparison would have done.
func TestLookupMatchesWholeNames(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/ab", 1)

	if _, code := m.lookup("/b"); code != -ENOENT {
		t.Errorf("lookup(/b) = %d, want -ENOENT: /ab must not answer for /b", code)
	}
	if _, code := m.lookup("/ab"); code != 0 {
		t.Errorf("lookup(/ab) = %d, want 0", code)
	}
}

// A missing path under a directory that is itself missing is ENOENT, not
// ENOTDIR, even though both are failures of the same walk. Getting this wrong
// makes "mkdir -p /a/b" impossible, because the shell would report the wrong
// reason and stop.
func TestMissingParentIsENOENTNotENOTDIR(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/exists/file.txt", 1)

	if _, code := m.lookup("/missing/file.txt"); code != -ENOENT {
		t.Errorf("lookup(missing parent) = %d, want -ENOENT", code)
	}
}

// lookupParent is what every mutating operation uses, so it has to refuse the
// root, refuse a missing parent, and refuse a parent that is a file.
func TestLookupParent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a/real.txt", 3)

	t.Run("happy path", func(t *testing.T) {
		n, base, code := m.lookupParent("/a/new.txt")
		if code != 0 || base != "new.txt" || n.Path != "/a" {
			t.Errorf("lookupParent = %+v, %q, %d; want /a, new.txt, 0", n, base, code)
		}
	})

	t.Run("root has no parent", func(t *testing.T) {
		if _, _, code := m.lookupParent("/"); code != -EINVAL {
			t.Errorf("lookupParent(/) = %d, want -EINVAL", code)
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		if _, _, code := m.lookupParent("/nope/new.txt"); code != -ENOENT {
			t.Errorf("lookupParent(missing parent) = %d, want -ENOENT", code)
		}
	})

	t.Run("parent is a file", func(t *testing.T) {
		if _, _, code := m.lookupParent("/a/real.txt/new.txt"); code != -ENOTDIR {
			t.Errorf("lookupParent(file parent) = %d, want -ENOTDIR", code)
		}
	})

	t.Run("escaping path", func(t *testing.T) {
		if _, _, code := m.lookupParent("/a/../../etc/passwd"); code != -EINVAL {
			t.Errorf("lookupParent(escaping) = %d, want -EINVAL", code)
		}
	})
}

func TestCleanRejectsRelative(t *testing.T) {
	if _, err := Clean("a/b"); err == nil {
		t.Error("Clean accepted a relative path; FUSE paths are absolute")
	}
}

func TestSplitParentNonRoot(t *testing.T) {
	cases := []struct{ in, parent, base string }{
		{"/a", "/", "a"},
		{"/a/b", "/a", "b"},
		{"/a/b/c", "/a/b", "c"},
		{"/中文/文件", "/中文", "文件"},
	}
	for _, c := range cases {
		parent, base, err := SplitParent(c.in)
		if err != nil || parent != c.parent || base != c.base {
			t.Errorf("SplitParent(%q) = %q, %q, %v; want %q, %q", c.in, parent, base, err, c.parent, c.base)
		}
	}
}
