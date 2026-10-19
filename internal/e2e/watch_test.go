package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/client"
	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/viewer"
)

// joinExcluding dials a client whose shares skip the named subtrees, and starts
// its watcher.
//
// The exclusion is the mechanism that keeps a viewer's own mount point out of
// the index when it happens to sit inside somebody's share, and it is only the
// watcher that can honour it after the initial scan: the scan never visits an
// excluded tree, and the watcher has to do the same or the tree would grow a
// subtree the scan will never report again.
func joinExcluding(t *testing.T, addr, name string, exclude []string, shares ...string) *client.Client {
	t.Helper()
	sh, err := client.NewShares(shares, exclude)
	if err != nil {
		t.Fatalf("NewShares(%v, exclude %v): %v", shares, exclude, err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: name, Shares: sh})
	if err != nil {
		t.Fatalf("Dial(%s): %v", name, err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree(%s): %v", name, err)
	}
	return c
}

// watchClient starts a client's watcher, which is what makes its changes reach
// the index at all.
func watchClient(t *testing.T, c *client.Client) {
	t.Helper()
	w, err := c.Watch()
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
}

// listPaths asks for one directory and returns the paths it holds.
func listPaths(t *testing.T, v *viewer.Viewer, path string) []string {
	t.Helper()
	nodes, err := v.List(path)
	if err != nil {
		t.Fatalf("List(%q): %v", path, err)
	}
	return pathsOf(nodes)
}

// hasPath reports whether a listing of dir contains want.
//
// A listing returns paths relative to the client: asking for "win/Documents"
// answers "Documents/a.txt". Only a pushed update is fully qualified, because it
// travels on a connection that may be watching several clients.
func hasPath(t *testing.T, v *viewer.Viewer, dir, want string) bool {
	t.Helper()
	for _, p := range listPaths(t, v, dir) {
		if p == want {
			return true
		}
	}
	return false
}

// TestE2EListFollowsAFileAppearing is the core promise of the whole feature: a
// file created on a supplying machine shows up in a listing of the aggregated
// tree while the connection stays open.
//
// Nothing here reconnects, rescans, or re-lists the client. The only thing that
// changed is the file.
func TestE2EListFollowsAFileAppearing(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)
	if hasPath(t, v, "win/Documents", "Documents/new.txt") {
		t.Fatal("new.txt is listed before it exists")
	}

	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("fresh"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	waitFor(t, "new.txt to appear in the listing", func() bool {
		return hasPath(t, v, "win/Documents", "Documents/new.txt")
	})
	// The listing has to be right about what it did report, not only include the
	// new name: a size read from the wrong file would pass the check above.
	nodes, err := v.List("win/Documents")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, n := range nodes {
		if n.Path == "Documents/new.txt" {
			if n.Kind != node.KindFile || n.Size != 5 {
				t.Fatalf("new.txt = %+v, want a 5-byte file", n)
			}
		}
	}
}

// TestE2EListFollowsADeletion pins the other direction: a file removed on the
// supplying machine leaves the listing.
//
// The index has to be told, not merely fail to be told: an index only ever grows
// otherwise, and every deleted file would be listed for ever.
func TestE2EListFollowsADeletion(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"doomed.txt": "bye"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)
	if !hasPath(t, v, "win/Documents", "Documents/doomed.txt") {
		t.Fatal("doomed.txt is not listed, so there is nothing to delete")
	}

	if err := os.Remove(filepath.Join(root, "doomed.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitFor(t, "doomed.txt to leave the listing", func() bool {
		return !hasPath(t, v, "win/Documents", "Documents/doomed.txt")
	})
}

// TestE2ECatFollowsAReplacedFile pins that a change reaches the content, not
// only the name: the file is replaced and a later read returns the new bytes.
func TestE2ECatFollowsAReplacedFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"live.txt": "first"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)
	if got, err := v.Cat("win/Documents/live.txt"); err != nil || string(got) != "first" {
		t.Fatalf("Cat before the change = %q, %v; want %q", got, err, "first")
	}

	if err := os.WriteFile(filepath.Join(root, "live.txt"), []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// A read always goes to the owner, so the content is right immediately; the
	// listing's size is what has to catch up, and that is the index's job.
	waitFor(t, "the listing to carry the new size", func() bool {
		nodes, err := v.List("win/Documents")
		if err != nil {
			return false
		}
		for _, n := range nodes {
			if n.Path == "Documents/live.txt" {
				return n.Size == int64(len("second"))
			}
		}
		return false
	})
	if got, err := v.Cat("win/Documents/live.txt"); err != nil || string(got) != "second" {
		t.Fatalf("Cat after the change = %q, %v; want %q", got, err, "second")
	}
}

// TestE2EListFollowsANewDirectory pins a directory created after the scan, and a
// file created inside it.
//
// The second half is the one a non-recursive watcher loses silently: the
// directory would be listed and its contents never would be, because the events
// would not be late, they would never arrive.
func TestE2EListFollowsANewDirectory(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)

	fresh := filepath.Join(root, "fresh")
	if err := os.Mkdir(fresh, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	waitFor(t, "fresh to appear in the listing", func() bool {
		return hasPath(t, v, "win/Documents", "Documents/fresh")
	})

	if err := os.WriteFile(filepath.Join(fresh, "inside.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	waitFor(t, "inside.txt to appear under the new directory", func() bool {
		return hasPath(t, v, "win/Documents/fresh", "Documents/fresh/inside.txt")
	})
}

// TestE2EViewerReceivesPushedUpdates pins the push itself, rather than the
// listing it is meant to keep current.
//
// The two are different claims. A listing reads the server's index; a push is
// the update travelling to the viewer unprompted. A server that applied changes
// but broadcast nothing would pass every listing test above and fail this one,
// and a viewer whose client never called Watch would fail it in the other
// direction.
func TestE2EViewerReceivesPushedUpdates(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)
	events := v.Watch()

	if err := os.WriteFile(filepath.Join(root, "pushed.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case batch, ok := <-events:
			if !ok {
				t.Fatal("the event channel closed unexpectedly")
			}
			for _, n := range batch.Nodes {
				// Fully qualified here, unlike a listing: the push names the
				// client because the viewer may be watching several of them.
				if n.Path == "win/Documents/pushed.txt" && n.Kind == node.KindFile && n.Size == 5 {
					return
				}
			}
		case <-deadline:
			t.Fatal("no pushed update naming win/Documents/pushed.txt within 10s")
		}
	}
}

// TestE2EViewerSeesAPushedDeletion pins that a deletion is pushed as an
// instruction to drop the path, carrying the marker kind rather than a kind a
// filesystem could report.
func TestE2EViewerSeesAPushedDeletion(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"doomed.txt": "bye"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)
	events := v.Watch()

	if err := os.Remove(filepath.Join(root, "doomed.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case batch, ok := <-events:
			if !ok {
				t.Fatal("the event channel closed unexpectedly")
			}
			for _, n := range batch.Nodes {
				if n.Path == "win/Documents/doomed.txt" && n.Kind == node.KindDeleted {
					return
				}
			}
		case <-deadline:
			t.Fatal("no pushed deletion of win/Documents/doomed.txt within 10s")
		}
	}
}

// TestE2EIndexOnlyHoldsLiveClients pins that the index holds what is connected.
//
// It is asserted through the watcher because that is where the two halves of the
// feature meet: a change was pushed to the index, and then the client that made
// it left, so the index has to drop everything that client owned — including the
// entries the pushes had just created. A version that cleaned up on disconnect
// by whatever the scan had uploaded would leak exactly the pushed entries.
func TestE2EIndexOnlyHoldsLiveClients(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)

	if err := os.WriteFile(filepath.Join(root, "pushed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	waitFor(t, "pushed.txt to appear", func() bool {
		return hasPath(t, v, "win/Documents", "Documents/pushed.txt")
	})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "win to leave the top level", func() bool {
		for _, p := range listPaths(t, v, "") {
			if p == "win" {
				return false
			}
		}
		return true
	})
	// The path is gone with the client, not merely hidden behind it. The
	// listing of a name nobody holds is a refusal, which is itself the answer:
	// there is no such client any more.
	if _, err := v.List("win/Documents"); err == nil {
		t.Fatal("List of a departed client succeeded, want a refusal")
	}
}

// TestE2EWatcherSkipsTheExcludedSubtree pins the exclusion against a live
// watcher.
//
// A viewer's mount point sits inside somebody's share in the deployment this
// exists for, and the aggregated tree must not index itself. The initial scan
// honours the exclusion; this is the half that cannot be checked by reconnecting,
// because the offending file is created while the watcher is already running.
func TestE2EWatcherSkipsTheExcludedSubtree(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	mount := filepath.Join(root, "Mount")
	if err := os.Mkdir(mount, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	c := joinExcluding(t, addr, "win", []string{mount}, "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)

	// Something outside the exclusion, so a quiet index is not mistaken for a
	// broken watcher.
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	waitFor(t, "outside.txt to appear", func() bool {
		return hasPath(t, v, "win/Documents", "Documents/outside.txt")
	})

	// And something inside it, which must never appear.
	if err := os.WriteFile(filepath.Join(mount, "hidden.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile in the excluded subtree: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	for _, p := range listPaths(t, v, "win/Documents") {
		if strings.HasPrefix(p, "Documents/Mount") {
			t.Fatalf("an excluded path reached the index: %q", p)
		}
	}
}

// TestE2ERenameFollowsThroughTheIndex pins that a rename is seen as what it is:
// the old path leaves and the new one arrives, both without a rescan.
func TestE2ERenameFollowsThroughTheIndex(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"before.txt": "body"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	v := dialViewer(t, addr)
	if !hasPath(t, v, "win/Documents", "Documents/before.txt") {
		t.Fatal("before.txt is not listed")
	}

	if err := os.Rename(filepath.Join(root, "before.txt"), filepath.Join(root, "after.txt")); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	waitFor(t, "after.txt to appear and before.txt to go", func() bool {
		paths := listPaths(t, v, "win/Documents")
		var hasAfter, hasBefore bool
		for _, p := range paths {
			if p == "Documents/after.txt" {
				hasAfter = true
			}
			if p == "Documents/before.txt" {
				hasBefore = true
			}
		}
		return hasAfter && !hasBefore
	})
}

// TestE2EChangesReachEveryViewer pins that a change is broadcast rather than
// sent to whoever asked: two viewers, one change, both learn of it.
func TestE2EChangesReachEveryViewer(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	c := join(t, addr, "win", "Documents="+root)
	watchClient(t, c)

	first := dialViewer(t, addr)
	second := dialViewer(t, addr)
	firstEvents := first.Watch()
	secondEvents := second.Watch()

	if err := os.WriteFile(filepath.Join(root, "both.txt"), []byte("z"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	waitFor(t, "both viewers to be told", func() bool {
		return drainedFrom(firstEvents, "win/Documents/both.txt") &&
			drainedFrom(secondEvents, "win/Documents/both.txt")
	})
}

// drainedFrom reports whether the path has already arrived on the channel,
// consuming whatever is there without waiting.
//
// It is how a poll-based wait is used against a channel: the events stay on the
// channel until they are read, so a batch that arrived between two polls is
// still found.
func drainedFrom(ch <-chan viewer.Event, path string) bool {
	for {
		select {
		case batch, ok := <-ch:
			if !ok {
				return false
			}
			for _, n := range batch.Nodes {
				if n.Path == path {
					return true
				}
			}
		default:
			return false
		}
	}
}
