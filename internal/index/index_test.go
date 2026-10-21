package index_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ispengya/peerfold/internal/index"
	"github.com/ispengya/peerfold/internal/node"
)

// dir and file build the two node kinds the index stores, so the tests say
// what they mean about kind without repeating struct literals.
func dir(path string) node.Node { return node.Node{Path: path, Kind: node.KindDir} }
func file(path string, size int64) node.Node {
	return node.Node{Path: path, Kind: node.KindFile, Size: size}
}

// tree is the fixture every test starts from: one client with a directory, a
// file inside it, and a nested directory with its own file.
func tree() (*index.Index, []node.Node) {
	nodes := []node.Node{
		dir("Documents"),
		file("Documents/a.txt", 5),
		dir("Documents/sub"),
		file("Documents/sub/b.txt", 6),
	}
	ix := &index.Index{}
	ix.Apply("win", nodes)
	return ix, nodes
}

func TestApplyAndLen(t *testing.T) {
	ix, _ := tree()
	if got := ix.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
	// Len counts every client's nodes, so a second client's node adds to the
	// total rather than being tracked separately.
	ix.Apply("mac", []node.Node{dir("Documents")})
	if got := ix.Len(); got != 5 {
		t.Fatalf("Len() after a second client = %d, want 5", got)
	}
	if got := ix.Clients(); len(got) != 2 {
		t.Fatalf("Clients() = %v, want two clients", got)
	}
}

func TestChildrenListsOnlyDirectChildren(t *testing.T) {
	ix, _ := tree()
	got, err := ix.Children("win", "Documents")
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	want := []string{"Documents/a.txt", "Documents/sub"}
	if len(got) != len(want) {
		t.Fatalf("Children(Documents) = %v, want %v", paths(got), want)
	}
	// Ascending by Path, and specifically not Documents/sub/b.txt.
	for i, w := range want {
		if got[i].Path != w {
			t.Fatalf("Children(Documents)[%d] = %q, want %q (got %v)", i, got[i].Path, w, paths(got))
		}
	}
	for _, n := range got {
		if n.Path == "Documents/sub/b.txt" {
			t.Fatal("Children returned a grandchild; it must list only direct children")
		}
	}
}

// TestChildrenDoesNotMatchASiblingByPrefix pins that a component is compared as
// a whole component. "Documents/subway" shares the byte prefix "Documents/sub"
// but is not under "Documents/sub" at all; a raw prefix comparison would let it
// through, and Remove-by-prefix would have the same shape of bug in the
// destructive direction.
func TestChildrenDoesNotMatchASiblingByPrefix(t *testing.T) {
	var ix index.Index
	ix.Apply("win", []node.Node{
		dir("Documents"),
		dir("Documents/sub"),
		file("Documents/sub/b.txt", 5),
		dir("Documents/subway"),
		file("Documents/subway/c.txt", 6),
	})

	got, err := ix.Children("win", "Documents")
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	want := []string{"Documents/sub", "Documents/subway"}
	if len(got) != len(want) {
		t.Fatalf("Children(Documents) = %v, want %v — both siblings are direct children", paths(got), want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Fatalf("Children(Documents) = %v, want %v", paths(got), want)
		}
	}

	// The hazard lives one level down, and only this query reaches it. A
	// comparison written as "the candidate's raw text starts with the parent's,
	// then trim the separator" passes the check above — both siblings really are
	// direct children — and leaks "Documents/subway" into this listing. Asking
	// for the deeper parent is the only way to see it, because that is the only
	// case where the queried prefix is followed by a letter rather than a
	// separator.
	sub, err := ix.Children("win", "Documents/sub")
	if err != nil {
		t.Fatalf("Children(Documents/sub): %v", err)
	}
	if len(sub) != 1 || sub[0].Path != "Documents/sub/b.txt" {
		t.Fatalf("Children(Documents/sub) = %v, want only [Documents/sub/b.txt]: "+
			"a sibling matched by prefix is not a child", paths(sub))
	}
}

func TestChildrenOnFileIsNotADirectory(t *testing.T) {
	ix, _ := tree()
	if _, err := ix.Children("win", "Documents/a.txt"); err != index.ErrNotDir {
		t.Fatalf("Children(file) error = %v, want ErrNotDir", err)
	}
}

func TestChildrenOnMissingPath(t *testing.T) {
	ix, _ := tree()
	if _, err := ix.Children("win", "Nope"); err != index.ErrNotFound {
		t.Fatalf("Children(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := ix.Children("ghost", "Documents"); err != index.ErrNotFound {
		t.Fatalf("Children(unknown client) error = %v, want ErrNotFound", err)
	}
}

func TestChildrenOfRootIsTopLevel(t *testing.T) {
	ix, _ := tree()
	got, err := ix.Children("win", "")
	if err != nil {
		t.Fatalf("Children(\"\") : %v", err)
	}
	if len(got) != 1 || got[0].Path != "Documents" {
		t.Fatalf("Children(\"\") = %v, want [Documents]", paths(got))
	}
}

func TestRemoveClient(t *testing.T) {
	ix, _ := tree()
	ix.Apply("mac", []node.Node{dir("Documents")})
	ix.RemoveClient("win")
	if got := ix.Len(); got != 1 {
		t.Fatalf("Len() after RemoveClient = %d, want 1 (the other client)", got)
	}
	if _, ok := ix.Lookup("win", "Documents"); ok {
		t.Error("Lookup found a node after RemoveClient")
	}
	if _, ok := ix.Lookup("mac", "Documents"); !ok {
		t.Error("RemoveClient removed the wrong client's nodes")
	}
	ix.RemoveClient("ghost") // must not panic
}

func TestApplyOverwritesSameKey(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{file("Documents/a.txt", 1)})
	ix.Apply("win", []node.Node{file("Documents/a.txt", 2)})
	if got := ix.Len(); got != 1 {
		t.Fatalf("Len() after re-applying one path = %d, want 1", got)
	}
	n, ok := ix.Lookup("win", "Documents/a.txt")
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if n.Size != 2 {
		t.Fatalf("Lookup size = %d, want 2 (the later Apply must win)", n.Size)
	}
}

func TestRemoveDeletesSubtree(t *testing.T) {
	ix, _ := tree()
	ix.Remove("win", "Documents/sub")
	if _, ok := ix.Lookup("win", "Documents/sub"); ok {
		t.Error("Remove left the directory itself behind")
	}
	if _, ok := ix.Lookup("win", "Documents/sub/b.txt"); ok {
		t.Error("Remove left a descendant behind; it must take the whole subtree")
	}
	if _, ok := ix.Lookup("win", "Documents/a.txt"); !ok {
		t.Error("Remove deleted a sibling that was not under the removed path")
	}
	if _, ok := ix.Lookup("win", "Documents"); !ok {
		t.Error("Remove deleted an ancestor that contains the removed path")
	}
}

func TestRemoveDoesNotMatchSiblingByPrefix(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{
		dir("Documents/sub"),
		file("Documents/sub.bak", 1),
		dir("Documents/subway"),
		file("Documents/subway/x", 1),
	})
	ix.Remove("win", "Documents/sub")
	for _, p := range []string{"Documents/sub.bak", "Documents/subway", "Documents/subway/x"} {
		if _, ok := ix.Lookup("win", p); !ok {
			t.Errorf("Remove(%q) also deleted %q: subtree removal must be path-component aware, not a raw prefix match", "Documents/sub", p)
		}
	}
}

func TestApplyDeletedNodeRemoves(t *testing.T) {
	ix, _ := tree()
	ix.Apply("win", []node.Node{
		node.Node{Path: "Documents/sub", Kind: node.KindDeleted},
	})
	if _, ok := ix.Lookup("win", "Documents/sub/b.txt"); ok {
		t.Error("applying a KindDeleted node left a descendant behind; it must behave as Remove")
	}
	if _, ok := ix.Lookup("win", "Documents/a.txt"); !ok {
		t.Error("applying a KindDeleted node removed an unrelated path")
	}
}

func TestPathsAreCaseSensitive(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{dir("Documents"), dir("documents")})
	if got := ix.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2: Documents and documents are different keys", got)
	}
	if _, ok := ix.Lookup("win", "Documents"); !ok {
		t.Error("Lookup(Documents) not found")
	}
	if _, ok := ix.Lookup("win", "documents"); !ok {
		t.Error("Lookup(documents) not found")
	}
	if _, ok := ix.Lookup("win", "DOCUMENTS"); ok {
		t.Error("Lookup(DOCUMENTS) found a node; the index must not fold case")
	}
}

func TestLookupMissing(t *testing.T) {
	ix, _ := tree()
	if _, ok := ix.Lookup("win", "Nope"); ok {
		t.Error("Lookup found a path that was never applied")
	}
	if _, ok := ix.Lookup("ghost", "Documents"); ok {
		t.Error("Lookup found a client that was never applied")
	}
}

func TestClientsAreSorted(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{dir("Documents")})
	ix.Apply("mac", []node.Node{dir("Documents")})
	ix.Apply("bsd", []node.Node{dir("Documents")})
	got := ix.Clients()
	want := []string{"bsd", "mac", "win"}
	if len(got) != len(want) {
		t.Fatalf("Clients() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Clients() = %v, want %v (ascending)", got, want)
		}
	}
}

// A path that could escape its share, or address a different one, must never
// enter the index: the index is keyed by share-relative path, and a ".." would
// make a remove reach outside the subtree it names.
func TestApplyRejectsUnsafePaths(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{
		file("Documents/../../etc/passwd", 1),
		file("..", 1),
		file("../x", 1),
		file("/etc/passwd", 1),
		file("", 1),
		// Windows separators. These arrive as one opaque component because
		// there is no "/" to split on, so the ".." check above does not see
		// them; a backslash-bearing key would later be joined onto a share root
		// by a file operation and escape it.
		file(`Documents\..\..\etc\passwd`, 1),
		file(`Documents\..\..\etc`, 1),
		file(`\etc\passwd`, 1),
		// Empty components: a trailing slash makes a path its own parent's
		// "child", and a doubled slash mis-parents what follows.
		file("Documents/", 1),
		file("Documents//a.txt", 1),
		file("/Documents", 1),
		file(".", 1),
		file("Documents/.", 1),
	})
	if got := ix.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0: unsafe paths must be dropped, got %v", got, ix.Clients())
	}
	// A legitimate path still goes in afterwards.
	ix.Apply("win", []node.Node{file("Documents/a.txt", 5)})
	if got := ix.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 after a valid Apply", got)
	}
}

// A trailing-slash key would be returned by Children as a child of the
// directory it names, which is not a child at all. It must never be stored.
func TestChildrenNeverReturnsSelfFromTrailingSlash(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{dir("Documents"), file("Documents/", 1), file("Documents/a.txt", 5)})
	got, err := ix.Children("win", "Documents")
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	for _, n := range got {
		if n.Path == "Documents/" || n.Path == "Documents" {
			t.Fatalf("Children returned %q, which is not a child of Documents: %v", n.Path, paths(got))
		}
	}
	if len(got) != 1 || got[0].Path != "Documents/a.txt" {
		t.Fatalf("Children = %v, want [Documents/a.txt]", paths(got))
	}
}

// Review Focus 5: a zero-byte file and an empty directory are ordinary entries.
// Neither may be mistaken for "absent", which is what a size- or child-based
// existence test would do.
func TestEmptyDirAndZeroByteFileAreRealEntries(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{dir("Empty"), file("zero.bin", 0)})

	if _, ok := ix.Lookup("win", "Empty"); !ok {
		t.Error("an empty directory was not indexed")
	}
	kids, err := ix.Children("win", "Empty")
	if err != nil {
		t.Fatalf("Children(Empty) = %v, want an empty slice and no error", err)
	}
	if len(kids) != 0 {
		t.Fatalf("Children(Empty) = %v, want no children", paths(kids))
	}

	n, ok := ix.Lookup("win", "zero.bin")
	if !ok {
		t.Fatal("a zero-byte file was not indexed; Size == 0 must not mean absent")
	}
	if n.Size != 0 || n.Kind != node.KindFile {
		t.Fatalf("zero.bin = {Size:%d Kind:%d}, want {Size:0 Kind:KindFile}", n.Size, n.Kind)
	}
	// And it shows up as a child of the top level, not swallowed.
	roots, err := ix.Children("win", "")
	if err != nil {
		t.Fatalf(`Children("") : %v`, err)
	}
	if len(roots) != 2 {
		t.Fatalf(`Children("") = %v, want both Empty and zero.bin`, paths(roots))
	}
}

// A client that has had every node removed is gone as a key, not present and
// empty: Clients() must not advertise a share-less client.
func TestRemoveLastNodeDropsClient(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{dir("Documents")})
	ix.Remove("win", "Documents")
	if got := ix.Clients(); len(got) != 0 {
		t.Fatalf("Clients() = %v, want empty after the last node was removed", got)
	}
	if got := ix.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if kids, err := ix.Children("win", ""); err != nil || len(kids) != 0 {
		t.Fatalf(`Children("") = (%v, %v), want (empty, nil)`, paths(kids), err)
	}
}

// Re-applying after a full removal must work: the client key has to come back,
// not stay deleted.
func TestApplyAfterFullRemovalRecreatesClient(t *testing.T) {
	ix := &index.Index{}
	ix.Apply("win", []node.Node{dir("Documents")})
	ix.Remove("win", "Documents")
	ix.Apply("win", []node.Node{dir("Documents")})
	if _, ok := ix.Lookup("win", "Documents"); !ok {
		t.Fatal("Apply after a full removal did not recreate the client's nodes")
	}
	if got := ix.Clients(); len(got) != 1 || got[0] != "win" {
		t.Fatalf("Clients() = %v, want [win]", got)
	}
}

// TestChildrenIndexFollowsSubtreeRemoval pins that the incremental child sets
// do not keep answering with a directory that was removed, or with a duplicate
// after the same path is applied twice.
func TestChildrenIndexFollowsSubtreeRemoval(t *testing.T) {
	ix, _ := tree()
	ix.Remove("win", "Documents/sub")

	got, err := ix.Children("win", "Documents")
	if err != nil {
		t.Fatalf("Children(Documents): %v", err)
	}
	if len(got) != 1 || got[0].Path != "Documents/a.txt" {
		t.Fatalf("Children(Documents) after removal = %v, want [Documents/a.txt]", paths(got))
	}
	if kids, err := ix.Children("win", "Documents/sub"); err != index.ErrNotFound {
		t.Fatalf("Children(removed dir) = (%v, %v), want ErrNotFound", paths(kids), err)
	}

	// Applying an existing path again must update the node, not add a second
	// child entry; a set makes that structural, and the count pins it.
	ix.Apply("win", []node.Node{file("Documents/a.txt", 9)})
	got, err = ix.Children("win", "Documents")
	if err != nil {
		t.Fatalf("Children(Documents) after re-apply: %v", err)
	}
	if len(got) != 1 || got[0].Size != 9 {
		t.Fatalf("Children(Documents) after re-apply = %v, want one node of size 9", got)
	}
}

func TestConcurrentApplyAndLookup(t *testing.T) {
	ix := &index.Index{}
	const n = 1000
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ix.Apply("win", []node.Node{file(fmt.Sprintf("Documents/f%d", i), int64(i))})
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ix.Lookup("win", fmt.Sprintf("Documents/f%d", i))
			ix.Len()
			ix.Children("win", "Documents")
			ix.Clients()
		}(i)
	}
	wg.Wait()
	if got := ix.Len(); got != n {
		t.Fatalf("Len() = %d, want %d", got, n)
	}
}

// paths is for failure messages: it turns a node slice into just its paths.
func paths(nodes []node.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Path
	}
	return out
}
