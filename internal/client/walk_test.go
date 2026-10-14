package client_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"peerfold/internal/client"
	"peerfold/internal/node"
)

// buildTree creates the fixture both the share tests and Scan use:
//
//	<tmp>/Documents/a.txt      content "hello"
//	<tmp>/Documents/sub/b.txt
//	<tmp>/Documents/empty/     an empty directory
//	<tmp>/Documents/zero.bin   a zero-byte file
//	<tmp>/mnt/peerfold/loop.txt  a mountpoint inside the shared tree
func buildTree(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	mustMkdir(t, filepath.Join(tmp, "Documents", "sub"))
	mustMkdir(t, filepath.Join(tmp, "Documents", "empty"))
	mustMkdir(t, filepath.Join(tmp, "mnt", "peerfold"))
	mustWrite(t, filepath.Join(tmp, "Documents", "a.txt"), []byte("hello"))
	mustWrite(t, filepath.Join(tmp, "Documents", "sub", "b.txt"), []byte("nested"))
	mustWrite(t, filepath.Join(tmp, "Documents", "zero.bin"), nil)
	mustWrite(t, filepath.Join(tmp, "mnt", "peerfold", "loop.txt"), []byte("loop"))
	mustWrite(t, filepath.Join(tmp, "mnt", "peerfold", "deeper"), []byte("deeper"))
	return tmp
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
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

// sortedKeys is the map-shaped equivalent, for messages about a whole scan.
func sortedKeys(m map[string]node.Node) []string {
	out := make([]string, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// scanAll runs Scan and returns every node it emitted, keyed by path.
func scanAll(t *testing.T, sh client.Shares, batch int) map[string]node.Node {
	t.Helper()
	got := make(map[string]node.Node)
	batches := 0
	err := client.Scan(sh, batch, func(nodes []node.Node) error {
		batches++
		if len(nodes) > batch {
			t.Errorf("Scan emitted a batch of %d nodes with batch=%d", len(nodes), batch)
		}
		for _, n := range nodes {
			if _, dup := got[n.Path]; dup {
				t.Errorf("Scan emitted %q twice", n.Path)
			}
			got[n.Path] = n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// With a batch this small, more than one node means more than one batch.
	// The check is skipped when a caller asks for a batch big enough to hold
	// the whole fixture in one go, which is a legitimate use of this helper.
	if batch <= 2 && len(got) > batch && batches < 2 {
		t.Errorf("Scan with batch=%d used %d batches for %d nodes, want more than one", batch, batches, len(got))
	}
	return got
}

func TestNewSharesRejectsBadInput(t *testing.T) {
	tmp := t.TempDir()
	mustMkdir(t, filepath.Join(tmp, "Documents"))

	if _, err := client.NewShares([]string{"Bad=/does/not/exist"}, nil); err == nil {
		t.Error("NewShares accepted a share root that does not exist")
	}
	if _, err := client.NewShares([]string{"A=" + tmp, "A=" + tmp}, nil); err == nil {
		t.Error("NewShares accepted a duplicate share name")
	}
	if _, err := client.NewShares([]string{"=" + tmp}, nil); err == nil {
		t.Error("NewShares accepted an empty share name")
	}
	if _, err := client.NewShares([]string{"NoEquals"}, nil); err == nil {
		t.Error("NewShares accepted a pair without '='")
	}
	file := filepath.Join(tmp, "Documents", "a.txt")
	mustWrite(t, file, []byte("x"))
	if _, err := client.NewShares([]string{"File=" + file}, nil); err == nil {
		t.Error("NewShares accepted a share root that is a file, not a directory")
	}
}

func TestSharesRootAndNames(t *testing.T) {
	tmp := buildTree(t)
	docs := filepath.Join(tmp, "Documents")
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	root, ok := sh.Root("Documents")
	if !ok {
		t.Fatal("Root(\"Documents\") not found")
	}
	if root != docs {
		t.Errorf("Root(\"Documents\") = %q, want %q", root, docs)
	}
	if _, ok := sh.Root("Nope"); ok {
		t.Error("Root(\"Nope\") reported a share that was never declared")
	}
	names := sh.Names()
	if len(names) != 1 || names[0] != "Documents" {
		t.Errorf("Names() = %v, want [Documents]", names)
	}
}

func TestNewSharesRejectsBadNames(t *testing.T) {
	tmp := buildTree(t)
	docs := filepath.Join(tmp, "Documents")
	// A share name becomes the first component of every indexed path, so a
	// separator or a dot component in it would make the index hold a path that
	// does not mean what it says.
	for _, name := range []string{".", "..", "a/b", `a\b`, "../x"} {
		if _, err := client.NewShares([]string{name + "=" + docs}, nil); err == nil {
			t.Errorf("NewShares accepted share name %q", name)
		}
	}
}

// Scan walks with WalkDir, which does not follow a symlinked root, so accepting
// one would advertise a share whose contents are silently missing.
func TestNewSharesRejectsSymlinkRoot(t *testing.T) {
	tmp := buildTree(t)
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(filepath.Join(tmp, "Documents"), link); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	if _, err := client.NewShares([]string{"Link=" + link}, nil); err == nil {
		t.Error("NewShares accepted a symlinked share root, which Scan cannot walk")
	}
}

// An excluded directory *inside* a share is the case that makes the SkipDir
// branch run. When the share root is itself the excluded path, Scan skips the
// share before walking at all, so that branch is never reached.
func TestScanSkipsExcludedDirInsideShare(t *testing.T) {
	tmp := buildTree(t)
	mount := filepath.Join(tmp, "mnt", "peerfold")

	// The share is the parent that CONTAINS the excluded mountpoint.
	sh, err := client.NewShares(
		[]string{"Mnt=" + filepath.Join(tmp, "mnt")},
		[]string{mount},
	)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	if !sh.Excluded(mount) {
		t.Fatalf("Excluded(%q) = false, want true", mount)
	}

	got := scanAll(t, sh, 3)

	// The mountpoint itself and everything under it must be absent, while the
	// share root is still advertised.
	if _, ok := got["Mnt"]; !ok {
		t.Error("Scan did not emit the share root Mnt")
	}
	for p := range got {
		if p == "Mnt/peerfold" || strings.HasPrefix(p, "Mnt/peerfold/") {
			t.Errorf("Scan emitted %q; an excluded directory and its contents must be skipped", p)
		}
	}
	if len(got) != 1 {
		t.Fatalf("Scan emitted %v, want only the share root Mnt", sortedKeys(got))
	}
}

func TestScanEmitsWholeTree(t *testing.T) {
	tmp := buildTree(t)
	sh, err := client.NewShares([]string{"Documents=" + filepath.Join(tmp, "Documents")}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	got := scanAll(t, sh, 2)
	// Mode and Mtime are part of the node model, so they must come from the
	// filesystem rather than being left zero.
	info, err := os.Stat(filepath.Join(tmp, "Documents", "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	a := got["Documents/a.txt"]
	if a.Mtime != info.ModTime().UnixNano() {
		t.Errorf("Documents/a.txt Mtime = %d, want %d", a.Mtime, info.ModTime().UnixNano())
	}
	if a.Mode != uint32(info.Mode().Perm()) {
		t.Errorf("Documents/a.txt Mode = %o, want %o", a.Mode, info.Mode().Perm())
	}

	// The share root itself, plus every entry beneath it. An empty directory
	// and a zero-byte file are both real entries and must be present.
	want := []string{
		"Documents",
		"Documents/a.txt",
		"Documents/empty",
		"Documents/sub",
		"Documents/sub/b.txt",
		"Documents/zero.bin",
	}
	for _, p := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("Scan did not emit %q", p)
		}
	}
	if len(got) != len(want) {
		paths := make([]string, 0, len(got))
		for p := range got {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		t.Errorf("Scan emitted %d nodes %v, want %d %v", len(got), paths, len(want), want)
	}

	if n := got["Documents"]; n.Kind != node.KindDir {
		t.Errorf("Documents kind = %d, want KindDir", n.Kind)
	}
	if n := got["Documents/empty"]; n.Kind != node.KindDir {
		t.Errorf("Documents/empty kind = %d, want KindDir (an empty directory is still an entry)", n.Kind)
	}
	zero := got["Documents/zero.bin"]
	if zero.Kind != node.KindFile {
		t.Errorf("Documents/zero.bin kind = %d, want KindFile", zero.Kind)
	}
	if zero.Size != 0 {
		t.Errorf("Documents/zero.bin size = %d, want 0", zero.Size)
	}
	if n := got["Documents/a.txt"]; n.Size != int64(len("hello")) {
		t.Errorf("Documents/a.txt size = %d, want %d", n.Size, len("hello"))
	}
}

func TestScanSkipsExcludedSubtree(t *testing.T) {
	tmp := buildTree(t)
	mount := filepath.Join(tmp, "mnt", "peerfold")
	sh, err := client.NewShares(
		[]string{"Documents=" + filepath.Join(tmp, "Documents"), "Loop=" + mount},
		[]string{mount},
	)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	// The exclusion has to cover the subtree, not just the mountpoint path:
	// matching only the root would let every file under it be indexed, which is
	// exactly the feedback loop the exclusion exists to prevent.
	if !sh.Excluded(filepath.Join(mount, "loop.txt")) {
		t.Errorf("Excluded(%q) = false, want true: exclusion must cover the subtree", filepath.Join(mount, "loop.txt"))
	}
	if !sh.Excluded(mount) {
		t.Errorf("Excluded(%q) = false, want true", mount)
	}
	if !sh.Excluded(filepath.Join(mount, "nested", "still")) {
		t.Error("Excluded = false for a path deep inside an excluded subtree")
	}

	got := scanAll(t, sh, 4)

	for p := range got {
		if strings.HasPrefix(p, "Loop/") {
			t.Errorf("Scan emitted %q; nothing under an excluded mountpoint may be indexed", p)
		}
	}
	// The exclusion is a subtree skip, not a whole-share skip: Loop itself is
	// still advertised, just not its contents.
	if _, ok := got["Loop"]; !ok {
		t.Error("Scan did not emit the excluded share's own root node")
	}
	if _, ok := got["Documents/a.txt"]; !ok {
		t.Error("excluding one subtree suppressed an unrelated share")
	}
}

// A nil exclude list and an empty one must behave the same, since cmd/client
// builds the list from flags and can produce either.
func TestExcludedWithNoExcludes(t *testing.T) {
	tmp := buildTree(t)
	sh, err := client.NewShares([]string{"Documents=" + filepath.Join(tmp, "Documents")}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	if sh.Excluded(filepath.Join(tmp, "Documents")) {
		t.Error("Excluded reported true when nothing was excluded")
	}
}

func TestScanPropagatesEmitError(t *testing.T) {
	tmp := buildTree(t)
	sh, err := client.NewShares([]string{"Documents=" + filepath.Join(tmp, "Documents")}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	wantErr := errors.New("stop scanning")
	calls := 0
	err = client.Scan(sh, 2, func([]node.Node) error {
		calls++
		if calls == 2 {
			return wantErr
		}
		return nil
	})
	// Scan documents that an emit error is returned unchanged, so this is identity
	// rather than errors.Is: wrapping it would still satisfy Is and would hide a
	// change to the error the caller is promised.
	if err != wantErr {
		t.Fatalf("Scan error = %v, want the emit callback's error returned unchanged (%v)", err, wantErr)
	}
	if calls != 2 {
		t.Errorf("Scan called emit %d times after an error on call 2, want it to stop at 2", calls)
	}
}
