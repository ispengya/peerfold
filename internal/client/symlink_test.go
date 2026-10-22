package client_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/client"
	"github.com/ispengya/peerfold/internal/node"
)

// A symlink inside a share used to be indexed as a regular file, with Size set
// to the length of its target string.
//
// Two separate mistakes produced that. Scan called info.Mode().Perm(), which
// keeps only the permission bits and discards the type bits, and it chose the
// kind with d.IsDir(), which is false for a link. So a link named link.txt
// pointing at a 15-byte file appeared in the index as a 7-byte regular file —
// and the owner's ReadAt opens the path, which follows the link, so a read
// returned the target's contents. A read-modify-write through a mount would
// then write those bytes over the real file.
//
// The mount layer refuses symlinks with ELOOP, but it can only do that if the
// index tells the truth about what the entry is.
func TestScanIndexesSymlinkInsideShare(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "real.txt"), []byte("real content here"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link.txt")
	if err := os.Symlink(filepath.Join(tmp, "real.txt"), link); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}

	sh, err := client.NewShares([]string{"S=" + tmp}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	got := scanAll(t, sh, 10)

	linkNode, ok := got["S/link.txt"]
	if !ok {
		t.Fatalf("Scan did not emit S/link.txt; got %v", sortedKeys(got))
	}
	if linkNode.Kind != node.KindSymlink {
		t.Errorf("S/link.txt Kind = %d, want KindSymlink (%d)", linkNode.Kind, node.KindSymlink)
	}
	// 1<<27 is os.ModeSymlink. The index must carry it, or a consumer cannot
	// tell a link from a file at all.
	if linkNode.Mode&(1<<27) == 0 {
		t.Errorf("S/link.txt Mode = %#o, want the ModeSymlink bit (1<<27) set", linkNode.Mode)
	}
	// The size must not be the length of the target string.
	if linkNode.Size == int64(len(link)) {
		t.Errorf("S/link.txt Size = %d, which is the length of the target path, not a file size", linkNode.Size)
	}

	// The real file next to it must be untouched by the fix.
	realNode, ok := got["S/real.txt"]
	if !ok {
		t.Fatal("Scan did not emit S/real.txt")
	}
	if realNode.Kind != node.KindFile {
		t.Errorf("S/real.txt Kind = %d, want KindFile", realNode.Kind)
	}
	if realNode.Mode&(1<<27) != 0 {
		t.Errorf("S/real.txt Mode = %#o must not have the symlink bit", realNode.Mode)
	}
	if realNode.Size != 17 {
		t.Errorf("S/real.txt Size = %d, want 17", realNode.Size)
	}

	// A directory is still a directory, and now carries the directory type bit.
	sub, ok := got["S"]
	if !ok {
		t.Fatal("Scan did not emit the share root S")
	}
	if sub.Kind != node.KindDir {
		t.Errorf("S Kind = %d, want KindDir", sub.Kind)
	}
	if sub.Mode&(1<<31) == 0 {
		t.Errorf("S Mode = %#o, want the ModeDir bit (1<<31) set", sub.Mode)
	}
}

// Mode keeps the full FileMode now, so the permission bits must still be
// readable from it. A consumer that masks with 0777 has to get the same answer
// the filesystem gave, so the expectation is taken from the filesystem rather
// than written as a literal: Windows does not honour the mode a program asks
// for, and asserting 0644 there would test the host, not the code.
func TestScanModeStillCarriesPermissions(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, err := client.NewShares([]string{"S=" + tmp}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	got := scanAll(t, sh, 10)

	n, ok := got["S/a.txt"]
	if !ok {
		t.Fatal("Scan did not emit S/a.txt")
	}
	info, err := os.Stat(filepath.Join(tmp, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	want := uint32(info.Mode().Perm())
	if perm := n.Mode & 0o7777; perm != want {
		t.Errorf("S/a.txt Mode&07777 = %#o, want %#o (the filesystem's own answer)", perm, want)
	}
	// And the type bits must be present now; that is the whole point of the
	// change. A regular file carries none of them, which is correct and is
	// why the positive case is asserted on a directory in the test above.
	if n.Mode&(1<<27) != 0 {
		t.Errorf("S/a.txt Mode = %#o must not carry the symlink bit", n.Mode)
	}
}

// symlinkEscapeFixture builds a share with links that stay inside it and links
// that leave it, plus a file outside the share the escape attempts would touch.
//
//	<tmp>/share/inside.txt          "inside"
//	<tmp>/share/inside-link.txt -> inside.txt
//	<tmp>/share/subdir/             (directory)
//	<tmp>/share/subdir-link -> subdir
//	<tmp>/share/escape.txt -> <tmp>/outside/secret.txt
//	<tmp>/share/escape-dir -> <tmp>/outside
//	<tmp>/outside/secret.txt        "secret"
func symlinkEscapeFixture(t *testing.T) (*client.FS, string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "share")
	outside := filepath.Join(tmp, "outside")
	for _, dir := range []string{root, outside, filepath.Join(root, "subdir")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	links := map[string]string{
		"inside-link.txt": filepath.Join(root, "inside.txt"),
		"subdir-link":     filepath.Join(root, "subdir"),
		"escape.txt":      filepath.Join(outside, "secret.txt"),
		"escape-dir":      outside,
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("cannot create a symlink on this host: %v", err)
		}
	}

	sh, err := client.NewShares([]string{"S=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	return client.NewFS(sh), "S", root, outside
}

// A symlink that is the last component is followed by a read, so the file it
// points at must be inside the share — otherwise "cat" through the wire reads
// a file the share never included.
func TestReadFollowsALinkOnlyWhenItsTargetIsInsideTheShare(t *testing.T) {
	fs, share, _, _ := symlinkEscapeFixture(t)

	got, _, err := fs.ReadAt(share, "inside-link.txt", 0, -1)
	if err != nil {
		t.Fatalf("ReadAt(inside-link.txt): %v", err)
	}
	if string(got) != "inside" {
		t.Fatalf("ReadAt(inside-link.txt) = %q, want %q", got, "inside")
	}

	if _, _, err := fs.ReadAt(share, "escape.txt", 0, -1); err == nil {
		t.Fatal("ReadAt followed a link out of the share; the target is not part of the share")
	}
	if _, err := fs.Stat(share, "escape.txt"); err == nil {
		t.Fatal("Stat followed a link out of the share")
	}
}

// An intermediate link is the larger hole: every operation on a path *through*
// it would otherwise act on the link's target, which may be the whole home
// directory.
func TestOperationsThroughAnEscapingDirectoryLinkAreRefused(t *testing.T) {
	fs, share, root, outside := symlinkEscapeFixture(t)

	if _, _, err := fs.ReadAt(share, "escape-dir/secret.txt", 0, -1); err == nil {
		t.Error("ReadAt read through a directory link out of the share")
	}
	if _, err := fs.Stat(share, "escape-dir/secret.txt"); err == nil {
		t.Error("Stat looked through a directory link out of the share")
	}
	if err := fs.Mkdir(share, "escape-dir/newdir"); err == nil {
		t.Error("Mkdir created a directory outside the share")
	}
	if _, err := fs.WriteAll(share, "escape-dir/new.txt", strings.NewReader("payload")); err == nil {
		t.Error("WriteAll wrote a file outside the share")
	}
	if err := fs.Remove(share, "escape-dir/secret.txt"); err == nil {
		t.Error("Remove deleted a file outside the share")
	}
	if err := fs.Rename(share, "escape-dir/secret.txt", "moved.txt"); err == nil {
		t.Error("Rename moved a file out of the share")
	}
	if err := fs.Rename(share, "inside.txt", "escape-dir/moved.txt"); err == nil {
		t.Error("Rename moved a file to a link target outside the share")
	}

	for _, name := range []string{"newdir", "new.txt", "moved.txt"} {
		if _, err := os.Lstat(filepath.Join(outside, name)); err == nil {
			t.Errorf("%s was created outside the share", name)
		}
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(b) != "secret" {
		t.Fatalf("outside/secret.txt = %q, %v; the escape attempts must not have touched it", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "inside.txt")); err != nil || string(b) != "inside" {
		t.Fatalf("share/inside.txt = %q, %v; a refused rename must not have moved it", b, err)
	}
}

// A link that stays inside the share is a legitimate way to organise files,
// and must keep working.
func TestOperationsThroughAnInsideDirectoryLinkAreAllowed(t *testing.T) {
	fs, share, root, _ := symlinkEscapeFixture(t)

	if _, err := fs.WriteAll(share, "subdir-link/created.txt", strings.NewReader("via link")); err != nil {
		t.Fatalf("WriteAll through an inside link: %v", err)
	}
	got, _, err := fs.ReadAt(share, "subdir-link/created.txt", 0, -1)
	if err != nil {
		t.Fatalf("ReadAt through an inside link: %v", err)
	}
	if string(got) != "via link" {
		t.Fatalf("ReadAt = %q, want %q", got, "via link")
	}
	if _, err := os.Stat(filepath.Join(root, "subdir", "created.txt")); err != nil {
		t.Fatalf("the file was not created under the real directory: %v", err)
	}
}

// Writing a name whose last component is a link replaces the link, not its
// target: the operation names something inside the share, and a target outside
// it is not part of the share.
func TestWriteToALinkNameReplacesTheLinkNotItsTarget(t *testing.T) {
	fs, share, root, outside := symlinkEscapeFixture(t)

	if _, err := fs.WriteAll(share, "escape.txt", strings.NewReader("replacement")); err != nil {
		t.Fatalf("WriteAll(escape.txt): %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(b) != "secret" {
		t.Fatalf("outside/secret.txt = %q, %v; writing the link name must not overwrite its target", b, err)
	}
	info, err := os.Lstat(filepath.Join(root, "escape.txt"))
	if err != nil {
		t.Fatalf("Lstat(escape.txt): %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("escape.txt is still a link; the write must have replaced it with the new file")
	}
}
