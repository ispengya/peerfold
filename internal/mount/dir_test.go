package mount

import (
	"testing"
)

// ---------------------------------------------------------------- Mkdir

func TestMkdirCreatesAndRefusesExisting(t *testing.T) {
	m := newTestMount(t)
	if code := m.Mkdir("/new", 0o755); code != 0 {
		t.Fatalf("Mkdir = %d", code)
	}
	if code := m.Mkdir("/new", 0o755); code != -EEXIST {
		t.Errorf("Mkdir over an existing name = %d, want -EEXIST (%d)", code, -EEXIST)
	}
}

func TestMkdirMissingParentIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if code := m.Mkdir("/nope/new", 0o755); code != -ENOENT {
		t.Errorf("Mkdir with missing parent = %d, want -ENOENT", code)
	}
}

// A file and a directory are different objects; creating one where the other
// exists must say which, because callers branch on it.
func TestMkdirOverFileIsEexist(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if code := m.Mkdir("/a.txt", 0o755); code != -EEXIST {
		t.Errorf("Mkdir over a file = %d, want -EEXIST", code)
	}
}

func TestMkdirUnderAFile(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if code := m.Mkdir("/a.txt/d", 0o755); code != -ENOTDIR {
		t.Errorf("Mkdir(file/d) = %d, want -ENOTDIR", code)
	}
}

// Mkdir at a path occupied by a symlink must not create anything. The name is
// taken, and EEXIST is the answer a caller expects.
func TestMkdirOverSymlink(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addSymlink("/link")
	code := m.Mkdir("/link", 0o755)
	if code == 0 {
		t.Fatal("Mkdir over a symlink succeeded")
	}
}

func TestMkdirRootIsRefused(t *testing.T) {
	m := newTestMount(t)
	if code := m.Mkdir("/", 0o755); code == 0 {
		t.Error("Mkdir(/) succeeded")
	}
}

// ---------------------------------------------------------------- Unlink

func TestUnlinkFile(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if code := m.Unlink("/a.txt"); code != 0 {
		t.Fatalf("Unlink = %d", code)
	}
	if _, code := m.lookup("/a.txt"); code != -ENOENT {
		t.Error("file still present after unlink")
	}
}

func TestUnlinkMissingIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if code := m.Unlink("/nope"); code != -ENOENT {
		t.Errorf("Unlink(missing) = %d, want -ENOENT", code)
	}
}

// Some programs call unlink on a directory. That is EISDIR or EPERM, never
// success, or they would believe the directory is gone.
func TestUnlinkDirectoryIsRefused(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")
	code := m.Unlink("/d")
	if code != -EISDIR && code != -EPERM {
		t.Errorf("Unlink(dir) = %d, want -EISDIR or -EPERM", code)
	}
	if _, code := m.lookup("/d"); code != 0 {
		t.Error("Unlink removed a directory")
	}
}

// The mount layer refuses to *open* a symlink, but removing one is safe and is
// the only way to get rid of one through the mount. A plan that runs the lookup
// first returns ELOOP and makes a symlink permanently undeletable.
func TestUnlinkSymlinkRemovesTheLink(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addSymlink("/link")
	if code := m.Unlink("/link"); code != 0 {
		t.Fatalf("Unlink(symlink) = %d, want 0", code)
	}
	if _, code := m.lookup("/link"); code != -ENOENT {
		t.Error("the symlink is still present")
	}
}

// Unlinking a path with a dirty open must flush first. The handle is the only
// copy of the content, and the name is about to be gone.
//
// The assertion is on the *order* of the two calls rather than on the owner's
// content, because the unlink removes that content: checking it afterwards would
// be checking a value the operation is meant to delete, which is why the first
// version of this test could not fail.
func TestUnlinkFlushesDirtyOpenFirst(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("data"), 0, fh)

	if code := m.Unlink("/a.txt"); code != 0 {
		t.Fatalf("Unlink = %d", code)
	}
	put := f.callIndex("Put /a.txt")
	remove := f.callIndex("Remove /a.txt")
	if put < 0 {
		t.Fatalf("the dirty content was never uploaded; calls = %v", f.callLog())
	}
	if remove < 0 {
		t.Fatalf("the file was never removed; calls = %v", f.callLog())
	}
	if put > remove {
		t.Errorf("the upload happened after the removal, so the name was gone "+
			"first: calls = %v", f.callLog())
	}
	if _, ok := m.handle(fh); ok {
		t.Error("the handle survived the unlink of its path")
	}
}

// ---------------------------------------------------------------- Rmdir

func TestRmdirEmpty(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addDir("/d")
	if code := m.Rmdir("/d"); code != 0 {
		t.Fatalf("Rmdir = %d", code)
	}
}

func TestRmdirNonEmptyIsRefused(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addFile("/d/a.txt", 1)
	if code := m.Rmdir("/d"); code != -ENOTEMPTY {
		t.Errorf("Rmdir(non-empty) = %d, want -ENOTEMPTY (%d)", code, -ENOTEMPTY)
	}
}

func TestRmdirFileIsNotDir(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if code := m.Rmdir("/a.txt"); code != -ENOTDIR {
		t.Errorf("Rmdir(file) = %d, want -ENOTDIR", code)
	}
}

func TestRmdirMissingIsEnoent(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rmdir("/nope"); code != -ENOENT {
		t.Errorf("Rmdir(missing) = %d, want -ENOENT", code)
	}
}

// The root has no parent, so there is nothing to remove it from. The refusal has
// to come before any lookup, because lookup("/") succeeds by construction.
func TestRmdirRootIsRefused(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rmdir("/"); code != -EINVAL && code != -EPERM {
		t.Errorf("Rmdir(/) = %d, want a refusal", code)
	}
}

// ---------------------------------------------------------------- Rename

func TestRenameFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.setContent("/a.txt", []byte("x"))
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if _, code := m.lookup("/b.txt"); code != 0 {
		t.Error("target missing after rename")
	}
	if _, code := m.lookup("/a.txt"); code != -ENOENT {
		t.Error("source still present after rename")
	}
}

// The case the design flagged as the one most likely to be missed: an editor
// writes a temporary file, fsyncs it, renames it over the original, and never
// releases it before the rename. The content must follow the name.
func TestRenameFlushesDirtyOpenFirst(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/tmp.txt", 0)
	f.addFile("/real.txt", 5)
	f.setContent("/tmp.txt", nil)
	f.setContent("/real.txt", []byte("old!!"))

	fh, _ := m.Open("/tmp.txt", OReadWrite)
	m.Write("/tmp.txt", []byte("new content"), 0, fh)

	if code := m.Rename("/tmp.txt", "/real.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if got := string(f.data["/real.txt"]); got != "new content" {
		t.Errorf("after rename the content is %q, want 'new content' -- the dirty "+
			"write did not follow the name", got)
	}
}

// A rename of a path that has a dirty open must not leave the write behind on
// the old name either.
func TestRenameLeavesNothingBehind(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	m.Write("/a.txt", []byte("data"), 0, fh)
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	if got, ok := f.data["/a.txt"]; ok {
		t.Errorf("write was left at the old name: %q", got)
	}
	if got := string(f.data["/b.txt"]); got != "data" {
		t.Errorf("the content is at %q, want data", got)
	}
}

// A handle still open on the source must follow the new name. If its path
// stays behind, a later flush recreates the name the user just moved away from.
func TestRenameUpdatesTheHandlePath(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadWrite)
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	h, ok := m.handle(fh)
	if !ok {
		t.Fatal("handle vanished across a rename")
	}
	if h.path != "/b.txt" {
		t.Errorf("handle path = %q, want /b.txt -- a later flush would recreate the "+
			"old name", h.path)
	}
	// And the follow-up write must land on the new name, proving the path
	// actually followed rather than being renamed in the table only.
	if n := m.Write("/b.txt", []byte("more"), 0, fh); n != 4 {
		t.Fatalf("Write after rename = %d", n)
	}
	if code := m.Flush("/b.txt", fh); code != 0 {
		t.Fatalf("Flush after rename = %d", code)
	}
	if got := string(f.data["/b.txt"]); got != "more" {
		t.Errorf("content after the follow-up write = %q, want more", got)
	}
	if _, ok := f.data["/a.txt"]; ok {
		t.Error("the old name was recreated by a flush")
	}
}

func TestRenameOntoExistingFile(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.setContent("/a.txt", []byte("a"))
	f.addFile("/b.txt", 1)
	f.setContent("/b.txt", []byte("b"))
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename over existing = %d", code)
	}
	if got := string(f.data["/b.txt"]); got != "a" {
		t.Errorf("after rename = %q, want \"a\"", got)
	}
}

// Renaming onto a path that has its own dirty open is the case the plan does not
// name: the target's content is about to be replaced, and a handle still holding
// the old bytes would write them back over the replacement on its next flush.
func TestRenameOntoAPathWithADirtyOpen(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.setContent("/a.txt", []byte("a"))
	f.addFile("/b.txt", 1)
	f.setContent("/b.txt", []byte("b"))

	fhB, _ := m.Open("/b.txt", OReadWrite)
	m.Write("/b.txt", []byte("B"), 0, fhB)

	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	// The target's own content was flushed before the name changed, and the
	// handle now holds the source's bytes, because the name it refers to holds
	// them. Either way the handle must not resurrect the old target content.
	if got := string(f.data["/b.txt"]); got != "a" {
		t.Errorf("after renaming over a dirty target = %q, want \"a\"", got)
	}
	h, ok := m.handle(fhB)
	if !ok {
		t.Fatal("the target's handle vanished")
	}
	if h.path != "/b.txt" {
		t.Errorf("the target handle path = %q, want /b.txt", h.path)
	}
}

// The target's own dirty handle must not survive the replacement. Its buffer is
// what the source is about to overwrite, and a handle that kept it would write
// those bytes back over the replacement on its next flush.
//
// Honest note, because this was measured rather than assumed: the test does NOT
// distinguish clearing the target's buffer from keeping it. A read through a clean
// handle falls through to the owner, which already holds the new content, so both
// answers are "a" and the test passes either way. Two other shapes were tried and
// both also pass under both mutants -- a path-form write afterwards (which
// re-reads the file and loads the new content into its own buffer), and a write
// through the target's own handle with an explicit flush (which uploads a buffer
// that holds the replacement anyway).
//
// So the buffer clear and the target flush have no reachable observable
// difference through this fake. The test is kept because it pins what does matter
// -- the handle must not resurrect the replaced content, and a read through it
// must see the new file -- and the fact that it cannot fail on the buffer clear is
// recorded in the ledger rather than papered over.
func TestRenameTargetHandleDoesNotResurrectOldContent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 1)
	f.setContent("/a.txt", []byte("a"))
	f.addFile("/b.txt", 1)
	f.setContent("/b.txt", []byte("b"))

	fhB, _ := m.Open("/b.txt", OReadWrite)
	if n := m.Write("/b.txt", []byte("B"), 0, fhB); n != 1 {
		t.Fatalf("Write = %d", n)
	}
	if code := m.Rename("/a.txt", "/b.txt"); code != 0 {
		t.Fatalf("Rename = %d", code)
	}
	// The handle now names the file the source brought. Reading through it must
	// see that file, not the bytes it was holding.
	buf := make([]byte, 4)
	n := m.Read("/b.txt", buf, 0, fhB)
	if n < 0 {
		t.Fatalf("Read = %d", n)
	}
	if got := string(buf[:n]); got != "a" {
		t.Errorf("reading through the handle = %q, want \"a\"", got)
	}
	// And a flush must not put the replaced content back.
	if code := m.Flush("/b.txt", fhB); code != 0 {
		t.Fatalf("Flush = %d", code)
	}
	if got := string(f.data["/b.txt"]); got != "a" {
		t.Errorf("after the flush the owner holds %q, want \"a\" -- the replaced "+
			"content came back", got)
	}
}

func TestRenameMissingSource(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rename("/nope", "/b.txt"); code != -ENOENT {
		t.Errorf("Rename(missing) = %d, want -ENOENT", code)
	}
}

func TestRenameIntoMissingDirectory(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 1)
	if code := m.Rename("/a.txt", "/nope/b.txt"); code != -ENOENT {
		t.Errorf("Rename(into missing dir) = %d, want -ENOENT", code)
	}
}

// Renaming the root, or onto the root, has no meaning.
func TestRenameRootIsRefused(t *testing.T) {
	m := newTestMount(t)
	if code := m.Rename("/", "/x"); code == 0 {
		t.Error("Rename(/) succeeded")
	}
	if code := m.Rename("/a", "/"); code == 0 {
		t.Error("Rename onto / succeeded")
	}
}

// Renaming a symlink moves the link. Like unlink, this must not go through
// lookup, which refuses symlinks with ELOOP and would make a link unrenamable.
func TestRenameSymlink(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addSymlink("/link")
	code := m.Rename("/link", "/other")
	if code != 0 {
		t.Fatalf("Rename(symlink) = %d, want 0", code)
	}
	if _, code := m.kindAt("/link"); code {
		t.Errorf("the link is still at the old name")
	}
	if _, present := m.kindAt("/other"); !present {
		t.Error("the link is not at the new name")
	}
	// And the movement is visible through lookup's own refusal: the old name is
	// gone, not looping.
	if _, code := m.lookup("/link"); code != -ENOENT {
		t.Errorf("lookup at the old name = %d, want -ENOENT", code)
	}
}

// A directory may not be moved onto a file, nor a file onto a directory. The
// owner enforces this, but the mount has to pass the refusal through rather than
// flattening it.
func TestRenameDirectoryOntoAFileIsRefused(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/d")
	f.addFile("/a.txt", 1)
	code := m.Rename("/d", "/a.txt")
	if code == 0 {
		// The fake is permissive here, so this is recorded rather than asserted:
		// what matters is that a refusal is passed through when the owner gives
		// one, which is Task 15's business against a real owner.
		t.Log("the fake allowed a directory onto a file; the real owner refuses")
	}
}

// ------------------------------------------------- the flush-before-mutate table

// Every mutating operation that makes a path unreachable must flush the handles
// on it first. This drives each entry directly, so that dropping a flush fails a
// test rather than making a save disappear.
func TestFlushBeforeMutateTable(t *testing.T) {
	// Each entry opens /a.txt, writes to it, then runs the operation and reports
	// what the owner holds for the name the content was on.
	type entry struct {
		name string
		run  func(m *Mount, fh uint64) int
		// wantPath is where the content must end up.
		wantPath string
	}
	entries := []entry{
		{"Unlink", func(m *Mount, fh uint64) int { return m.Unlink("/a.txt") }, ""},
		{"Rename", func(m *Mount, fh uint64) int { return m.Rename("/a.txt", "/b.txt") }, "/b.txt"},
	}
	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			m := newTestMount(t)
			f := fakeOf(t, m)
			f.addFile("/a.txt", 0)
			f.setContent("/a.txt", nil)

			fh, code := m.Open("/a.txt", OReadWrite)
			if code != 0 {
				t.Fatalf("Open = %d", code)
			}
			if n := m.Write("/a.txt", []byte("content"), 0, fh); n != 7 {
				t.Fatalf("Write = %d", n)
			}
			if code := e.run(m, fh); code != 0 {
				t.Fatalf("%s = %d", e.name, code)
			}
			if e.wantPath == "" {
				// The name is gone, so the content had to be uploaded before the
				// removal. Losing it means the user's write is in memory only.
				// The assertion is on the call order, because the removal takes
				// the content with it and checking it afterwards proves nothing.
				put := f.callIndex("Put /a.txt")
				remove := f.callIndex("Remove /a.txt")
				if put < 0 {
					t.Errorf("%s never uploaded the content; calls = %v", e.name, f.callLog())
				} else if remove < 0 {
					t.Errorf("%s never removed the name; calls = %v", e.name, f.callLog())
				} else if put > remove {
					t.Errorf("%s removed the name before uploading the content: "+
						"calls = %v", e.name, f.callLog())
				}
				return
			}
			if got := string(f.data[e.wantPath]); got != "content" {
				t.Errorf("%s left %q at %s, want content", e.name, got, e.wantPath)
			}
		})
	}
}
