package client_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"peerfold/internal/client"
	"peerfold/internal/node"
)

// collector gathers watch events so a test can wait for the one it expects
// rather than sleeping and hoping.
//
// Waiting for a condition with a deadline is the whole point: a test that slept
// for a fixed time would be both slow when it passes and flaky when the machine
// is busy, and neither failure would say anything about the code.
type collector struct {
	mu     sync.Mutex
	events []node.Node
}

func (c *collector) emit(nodes []node.Node) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, nodes...)
	return nil
}

// waitFor blocks until a collected event matches want, and returns it.
func (c *collector) waitFor(t *testing.T, what string, want func(node.Node) bool) node.Node {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var seen []node.Node
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, n := range c.events {
			if want(n) {
				c.mu.Unlock()
				return n
			}
		}
		seen = append(seen[:0], c.events...)
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no event %s within 5s; saw %v", what, seen)
	return node.Node{}
}

// snapshot returns everything collected so far.
func (c *collector) snapshot() []node.Node {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]node.Node(nil), c.events...)
}

// watchFixture builds a share directory with one subdirectory and starts a
// watcher on it.
func watchFixture(t *testing.T) (root string, c *collector, closer interface{ Close() error }) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"sub", "empty"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("Mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "existing.txt"), []byte("here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sh, err := client.NewShares([]string{"Documents=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c = &collector{}
	w, err := client.Watch(sh, c.emit)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return root, c, w
}

// TestWatchReportsANewFile pins the basic case: a file that appears is reported
// with its path inside the share, as a file, with the size it actually has.
func TestWatchReportsANewFile(t *testing.T) {
	root, c, _ := watchFixture(t)

	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := c.waitFor(t, "for Documents/x.txt", func(n node.Node) bool { return n.Path == "Documents/x.txt" })
	if got.Kind != node.KindFile {
		t.Fatalf("kind = %d, want KindFile", got.Kind)
	}
	if got.Size != 5 {
		t.Fatalf("size = %d, want 5", got.Size)
	}
	if got.Mtime == 0 {
		t.Fatal("mtime is zero, want the file's modification time")
	}
}

// TestWatchReportsADeletion pins that a removed file is an instruction to drop
// it, not a node with stale metadata.
func TestWatchReportsADeletion(t *testing.T) {
	root, c, _ := watchFixture(t)
	target := filepath.Join(root, "gone.txt")
	if err := os.WriteFile(target, []byte("bye"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for the file to appear", func(n node.Node) bool { return n.Path == "Documents/gone.txt" })

	if err := os.Remove(target); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := c.waitFor(t, "for Documents/gone.txt to be deleted", func(n node.Node) bool {
		return n.Path == "Documents/gone.txt" && n.Kind == node.KindDeleted
	})
	if got.Kind != node.KindDeleted {
		t.Fatalf("kind = %d, want KindDeleted", got.Kind)
	}
}

// TestWatchReportsAChangedSize pins that a modification is reported again and
// carries the new size, so an index that trusts the event is not left holding
// the old one.
func TestWatchReportsAChangedSize(t *testing.T) {
	root, c, _ := watchFixture(t)
	target := filepath.Join(root, "grow.txt")
	if err := os.WriteFile(target, []byte("short"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for the file to appear", func(n node.Node) bool {
		return n.Path == "Documents/grow.txt" && n.Size == 5
	})

	if err := os.WriteFile(target, []byte("a much longer body"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for the new size", func(n node.Node) bool {
		return n.Path == "Documents/grow.txt" && n.Size == 18
	})
}

// TestWatchSeesInsideANewDirectory pins the case a non-recursive watcher
// silently loses.
//
// fsnotify watches one directory and does not descend. A watcher that only
// registered the share roots would report the new directory and then never
// report anything created inside it — the events would not be late, they would
// never arrive, and the aggregated tree would stay wrong for the life of the
// connection. So both the directory and the file inside it are asserted.
func TestWatchSeesInsideANewDirectory(t *testing.T) {
	root, c, _ := watchFixture(t)

	fresh := filepath.Join(root, "fresh")
	if err := os.Mkdir(fresh, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	c.waitFor(t, "for Documents/fresh to appear", func(n node.Node) bool {
		return n.Path == "Documents/fresh" && n.Kind == node.KindDir
	})

	if err := os.WriteFile(filepath.Join(fresh, "inside.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got := c.waitFor(t, "for Documents/fresh/inside.txt", func(n node.Node) bool {
		return n.Path == "Documents/fresh/inside.txt"
	})
	if got.Kind != node.KindFile || got.Size != 4 {
		t.Fatalf("node = %+v, want a 4-byte file", got)
	}
}

// TestWatchSeesInsideADeeplyNestedNewDirectory pins that the recursion is a
// walk and not one level: a directory created inside a directory that was
// itself created after the watcher started is also picked up.
func TestWatchSeesInsideADeeplyNestedNewDirectory(t *testing.T) {
	root, c, _ := watchFixture(t)

	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	c.waitFor(t, "for Documents/a/b/c to appear", func(n node.Node) bool {
		return n.Path == "Documents/a/b/c" && n.Kind == node.KindDir
	})

	if err := os.WriteFile(filepath.Join(deep, "leaf.txt"), []byte("leaf"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for Documents/a/b/c/leaf.txt", func(n node.Node) bool {
		return n.Path == "Documents/a/b/c/leaf.txt"
	})
}

// TestWatchReportsChangesInAnExistingSubdirectory pins that the initial walk
// registered what was already there, not only what appears later.
func TestWatchReportsChangesInAnExistingSubdirectory(t *testing.T) {
	root, c, _ := watchFixture(t)

	if err := os.WriteFile(filepath.Join(root, "sub", "added.txt"), []byte("new"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for Documents/sub/added.txt", func(n node.Node) bool {
		return n.Path == "Documents/sub/added.txt"
	})
}

// TestWatchCloseStopsEvents pins that closing is final: no further events and no
// panic, whatever the filesystem does afterwards.
func TestWatchCloseStopsEvents(t *testing.T) {
	root, c, closer := watchFixture(t)
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing twice is specified as safe, and a caller that deferred a close
	// after an explicit one is exactly how that happens.
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "after.txt"), []byte("late"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, n := range c.snapshot() {
		if n.Path == "Documents/after.txt" {
			t.Fatalf("an event arrived after Close: %+v", n)
		}
	}
}

// TestWatchSkipsExcludedSubtrees pins the exclusion, which is the mechanism that
// keeps the aggregated tree from indexing itself when a viewer's mount point
// sits inside a share.
func TestWatchSkipsExcludedSubtrees(t *testing.T) {
	root := t.TempDir()
	skip := filepath.Join(root, "Mount")
	if err := os.Mkdir(skip, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	sh, err := client.NewShares([]string{"Documents=" + root}, []string{skip})
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c := &collector{}
	w, err := client.Watch(sh, c.emit)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(skip, "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile in the excluded subtree: %v", err)
	}
	// Something outside it, so the test can tell "the watcher is quiet" apart
	// from "the watcher is not working at all".
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for Documents/outside.txt", func(n node.Node) bool {
		return n.Path == "Documents/outside.txt"
	})

	for _, n := range c.snapshot() {
		if strings.HasPrefix(n.Path, "Documents/Mount") {
			t.Fatalf("an excluded path was reported: %+v", n)
		}
	}
}

// TestWatchReportsTwoSharesSeparately pins that each share is mapped back to its
// own name, so a path never lands under the wrong share.
func TestWatchReportsTwoSharesSeparately(t *testing.T) {
	docs := t.TempDir()
	photos := t.TempDir()

	sh, err := client.NewShares([]string{"Documents=" + docs, "Photos=" + photos}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c := &collector{}
	w, err := client.Watch(sh, c.emit)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(docs, "d.txt"), []byte("d"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(photos, "p.txt"), []byte("p"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for Documents/d.txt", func(n node.Node) bool { return n.Path == "Documents/d.txt" })
	c.waitFor(t, "for Photos/p.txt", func(n node.Node) bool { return n.Path == "Photos/p.txt" })
}

// TestWatchReportsARenameAsADeletion pins that a rename out of a watched name
// reports the old name gone. fsnotify has no portable rename event: the old name
// arrives as a rename or a remove, and the new name arrives as a create.
func TestWatchReportsARenameAsADeletion(t *testing.T) {
	root, c, _ := watchFixture(t)
	old := filepath.Join(root, "before.txt")
	if err := os.WriteFile(old, []byte("body"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c.waitFor(t, "for Documents/before.txt", func(n node.Node) bool { return n.Path == "Documents/before.txt" })

	if err := os.Rename(old, filepath.Join(root, "after.txt")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	c.waitFor(t, "for Documents/before.txt to be deleted", func(n node.Node) bool {
		return n.Path == "Documents/before.txt" && n.Kind == node.KindDeleted
	})
	c.waitFor(t, "for Documents/after.txt to appear", func(n node.Node) bool {
		return n.Path == "Documents/after.txt" && n.Kind == node.KindFile
	})
}

// TestWatchRefusesNoEmitter pins the one argument that cannot be defaulted.
func TestWatchRefusesNoEmitter(t *testing.T) {
	sh, err := client.NewShares([]string{"Documents=" + t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	if _, err := client.Watch(sh, nil); err == nil {
		t.Fatal("Watch with no emitter = nil error, want a refusal")
	}
}
