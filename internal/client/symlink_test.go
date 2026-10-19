package client_test

import (
	"os"
	"path/filepath"
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
