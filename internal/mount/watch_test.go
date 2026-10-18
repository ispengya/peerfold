package mount

import (
	"fmt"
	"testing"
	"time"

	"peerfold/internal/node"
)

// waitFor polls a condition until it holds or the deadline passes. The watch loop
// is a goroutine, so every observation of it is eventually-consistent rather than
// immediate.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatal(msg)
	}
}

// ------------------------------------------------------------ applying events

func TestEventUpdatesTheIndex(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "/new.txt", Kind: node.KindFile, Size: 3})

	waitFor(t, 2*time.Second, func() bool {
		n, code := m.lookup("/new.txt")
		return code == 0 && n.Size == 3
	}, "the index did not pick up the event")
}

func TestDeletedEventRemovesTheEntry(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 3)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "/a.txt", Kind: node.KindDeleted})

	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/a.txt")
		return code == -ENOENT
	}, "a deleted event did not remove the entry")
}

// A batch is applied entry by entry, so one event that both creates a directory
// and fills it works. The order within a batch is the client's.
func TestABatchAppliesEveryEntry(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1,
		node.Node{Path: "/d", Kind: node.KindDir, Mode: 0o755},
		node.Node{Path: "/d/a.txt", Kind: node.KindFile, Size: 1},
	)

	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/d/a.txt")
		return code == 0
	}, "the batch was not fully applied")
}

// An event for a path whose parent was never seen must still be recorded: the
// index is keyed by full path, and a later listing rebuilds the view rather than
// being derived from a parent entry that may not exist.
func TestAnEventForAnUnknownParentIsStillRecorded(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "/never/heard/of.txt", Kind: node.KindFile, Size: 2})

	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/never/heard/of.txt")
		return code == 0
	}, "an event under an unseen parent was dropped")
}

// localIndex reports a path from the event cache without touching the caller.
func TestLocalIndexIsConsultedBeforeTheCaller(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	// The caller does not know this path at all: only the event does. If lookup
	// reaches the caller it will answer from the caller's own index and see
	// nothing, so a hit here can only come from the event table.
	f.emit(1, node.Node{Path: "/fromevent.txt", Kind: node.KindFile, Size: 7})

	waitFor(t, 2*time.Second, func() bool {
		n, code := m.lookup("/fromevent.txt")
		return code == 0 && n.Size == 7
	}, "lookup did not see the event")
}

// A path the event cache has never heard of still resolves, through the caller.
// Without this the cache would turn a correct listing into ENOENT.
func TestLookupFallsThroughToTheCaller(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/only-at-the-owner.txt", 4)

	n, code := m.lookup("/only-at-the-owner.txt")
	if code != 0 {
		t.Fatalf("lookup = %d, want 0", code)
	}
	if n.Size != 4 {
		t.Errorf("size = %d, want 4", n.Size)
	}
}

// An event that removes a path must be visible through lookup even though no
// caller listing has ever mentioned it.
func TestDeletedEventShadowsAnUnknownDiscovery(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/gone.txt", 1)
	if _, code := m.lookup("/gone.txt"); code != 0 {
		t.Fatal("the fixture did not resolve")
	}
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "/gone.txt", Kind: node.KindDeleted})

	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/gone.txt")
		return code == -ENOENT
	}, "a deleted event did not shadow the earlier discovery")
}

// A deleted entry in the event table must not be resurrected by a caller listing
// taken afterwards, because the deletion is newer information than the caller's
// index can be.
func TestDeletedEntryIsNotResurrectedByAListing(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/gone.txt", 1)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	// The caller's index still has it: the deletion came over the watch, not
	// through the caller.
	f.emit(1, node.Node{Path: "/gone.txt", Kind: node.KindDeleted})
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/gone.txt")
		return code == -ENOENT
	}, "the deletion was not applied")

	// Listing the parent must not put it back.
	children, err := m.call().List("/")
	if err != nil {
		t.Fatalf("List = %v", err)
	}
	for _, c := range children {
		if c.Path == "/gone.txt" {
			// The caller still reports it, which is exactly the case under test.
			if _, code := m.lookup("/gone.txt"); code != -ENOENT {
				t.Error("a later listing resurrected a path the watch deleted")
			}
			return
		}
	}
	t.Log("the fake no longer reports the path; the shadow case was not reachable")
}

// ------------------------------------------------------------------- drops

// An owner tombstone is the server saying that a whole client tree has been
// withdrawn. It must release the mount point, not mark the connection lost: the
// server is still reachable, so a reconnect would succeed and leave the mount up
// with nothing to serve.
func TestOwnerWithdrawalReleasesTheMount(t *testing.T) {
	f := newFakeCaller()
	f.watchForTest()
	m := New(f, DefaultOptions())

	released := make(chan struct{})
	m.mu.Lock()
	m.onGiveUp = func() { close(released) }
	m.mu.Unlock()

	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "owner", Kind: node.KindDeleted})

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("an owner tombstone did not release the mount; the mount would " +
			"stay mounted forever after its owner disappeared")
	}
}

// A tombstone for one owner must not take down a mount that is still serving
// another. The mount is a view of the whole federation, not of one client; only
// when the last owner's tree is withdrawn is there nothing left to serve.
func TestOwnerWithdrawalKeepsServingOtherOwners(t *testing.T) {
	f := newFakeCaller()
	f.addDir("/alice")
	f.addDir("/bob")
	f.watchForTest()
	m := New(f, DefaultOptions())

	released := make(chan struct{})
	m.mu.Lock()
	m.onGiveUp = func() { close(released) }
	m.mu.Unlock()

	m.startWatch()
	defer m.stopWatch()

	// One owner goes away; the other still has a tree.
	f.emit(1, node.Node{Path: "alice", Kind: node.KindDeleted})

	select {
	case <-released:
		t.Fatal("the mount released after one owner's tombstone, although another " +
			"owner is still being served")
	case <-time.After(300 * time.Millisecond):
	}

	// The last owner goes away, and now nothing is left to serve.
	f.emit(2, node.Node{Path: "bob", Kind: node.KindDeleted})

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the mount did not release after the last owner's tree was withdrawn")
	}
}

// The whole point of counting events: if the transport drops some, nothing else
// in the system notices, because a drop is not an error.
func TestASequenceGapIsDetected(t *testing.T) {
	m := newTestMount(t)

	// A first, unexpected sequence is adopted rather than treated as a gap: a
	// fresh mount has no idea what the last sequence was before it attached.
	m.applyEvent(Batch{Seq: 5, Nodes: []node.Node{{Path: "/a.txt", Kind: node.KindFile}}})
	if m.stale() {
		t.Error("the first sequence number was treated as a gap")
	}

	m.applyEvent(Batch{Seq: 9, Nodes: []node.Node{{Path: "/b.txt", Kind: node.KindFile}}})
	if !m.stale() {
		t.Error("a sequence gap was not detected: the tree can now be silently wrong for ever")
	}
}

func TestAContiguousSequenceIsNotAGap(t *testing.T) {
	m := newTestMount(t)
	m.applyEvent(Batch{Seq: 1, Nodes: []node.Node{{Path: "/a.txt"}}})
	m.applyEvent(Batch{Seq: 2, Nodes: []node.Node{{Path: "/b.txt"}}})
	m.applyEvent(Batch{Seq: 3, Nodes: []node.Node{{Path: "/c.txt"}}})
	if m.stale() {
		t.Error("a contiguous sequence was reported as a gap")
	}
}

// A batch with no sequence number -- a client that does not number its events --
// must not be read as a gap. Zero means "not numbered", not "sequence zero".
func TestAnUnnumberedBatchIsNotAGap(t *testing.T) {
	m := newTestMount(t)
	m.applyEvent(Batch{Seq: 1, Nodes: []node.Node{{Path: "/a.txt"}}})
	m.applyEvent(Batch{Nodes: []node.Node{{Path: "/b.txt"}}})
	m.applyEvent(Batch{Seq: 2, Nodes: []node.Node{{Path: "/c.txt"}}})
	if m.stale() {
		t.Error("an unnumbered batch was treated as a sequence gap")
	}
}

// Once stale, always stale until something deliberately clears it: Task 11 owns
// the clearing, and a later contiguous batch must not do it by accident.
func TestStaleIsNotClearedByALaterEvent(t *testing.T) {
	m := newTestMount(t)
	m.applyEvent(Batch{Seq: 1})
	m.applyEvent(Batch{Seq: 5})
	if !m.stale() {
		t.Fatal("the gap was not detected")
	}
	m.applyEvent(Batch{Seq: 6})
	if !m.stale() {
		t.Error("a later contiguous batch cleared the stale flag; the tree was " +
			"never actually refreshed")
	}
}

// --------------------------------------------------------- the handed-out table

func TestHandedOutTracksInodes(t *testing.T) {
	m := newTestMount(t)
	fh, _ := m.Open("/a.txt", OReadWrite)
	m.noteInode(fh, "/a.txt")
	if got := m.handedOut(); len(got) != 1 || got[0] != "/a.txt" {
		t.Errorf("handedOut = %v, want [/a.txt]", got)
	}
}

// Closing every handle on a path does not mean the kernel stopped caching its
// attributes, so the entry must survive a release. Dropping it on release would
// make a refresh skip the path the kernel is holding the stale attribute for,
// which is exactly the invalidation the table exists to drive.
func TestHandedOutSurvivesAHandleRelease(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 0)
	f.setContent("/a.txt", nil)

	fh, _ := m.Open("/a.txt", OReadOnly)
	m.noteInode(fh, "/a.txt")
	if len(m.handedOut()) != 1 {
		t.Fatalf("handedOut = %v, want one entry", m.handedOut())
	}
	m.mu.Lock()
	m.closeHandle(fh)
	m.mu.Unlock()
	if got := m.handedOut(); len(got) != 1 {
		t.Errorf("handedOut after close = %v, want the path kept: the kernel can "+
			"still hold the attribute", got)
	}
	// Only an explicit forget removes it.
	m.forgotInode("/a.txt")
	if got := m.handedOut(); len(got) != 0 {
		t.Errorf("handedOut after forgotInode = %v, want empty", got)
	}
}

// FUSE has no whole-filesystem-invalidate primitive, so invalidation has to walk
// this table. It must therefore be bounded: caching every path ever seen would
// grow without limit on a long-running mount.
func TestHandedOutIsBounded(t *testing.T) {
	m := newTestMount(t)
	m.opts.InvalidateTable = 8
	for i := 0; i < 100; i++ {
		m.noteInode(uint64(i+1), "/f")
	}
	if got := len(m.handedOut()); got > m.opts.InvalidateTable {
		t.Errorf("handedOut holds %d entries, want at most %d", got, m.opts.InvalidateTable)
	}
}

// Every path handed out is reported, and a path handed out through several
// handles is still one path: invalidation walks paths, not handles.
func TestHandedOutReportsDistinctPaths(t *testing.T) {
	m := newTestMount(t)
	m.noteInode(1, "/a")
	m.noteInode(2, "/a")
	m.noteInode(3, "/b")
	got := m.handedOut()
	seen := map[string]int{}
	for _, p := range got {
		seen[p]++
	}
	if seen["/a"] != 1 {
		t.Errorf("/a reported %d times, want once", seen["/a"])
	}
	if seen["/b"] != 1 {
		t.Errorf("/b reported %d times, want once", seen["/b"])
	}
}

// The default table is a real bound rather than zero, which would make the table
// useless and every invalidation a no-op.
func TestDefaultInvalidateTableIsARealBound(t *testing.T) {
	if DefaultOptions().InvalidateTable <= 0 {
		t.Fatalf("DefaultOptions().InvalidateTable = %d, want a positive bound",
			DefaultOptions().InvalidateTable)
	}
}

// ------------------------------------------------------------- the watch loop

func TestWatchClosureMarksDisconnected(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()

	f.dropWatch()
	waitFor(t, 2*time.Second, func() bool { return m.disconnected() },
		"a closed watch channel did not mark the mount disconnected")
}

// Closing the channel is the transport saying "no more updates", which is not the
// same as "the tree is empty". Nothing is thrown away when the closure is noticed,
// and the tree is readable again the moment a caller is installed.
//
// The lookup itself cannot succeed while the mount is offline, and that is Task
// 11's contract rather than something this test can evade: offline is EIO for
// every path, because a program that is told ENOENT concludes the file was
// deleted and may act destructively on that conclusion. So "the index survived"
// is asserted the only way it can honestly be asserted -- by reconnecting and
// finding the tree still there -- rather than by demanding an answer the mount
// must not give.
func TestWatchClosureKeepsTheIndex(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a.txt", 3)
	f.watchForTest()
	m.startWatch()

	f.dropWatch()
	waitFor(t, 2*time.Second, func() bool { return m.disconnected() },
		"the closure was not noticed")

	// The proof that nothing was emptied: while offline the mount refuses to
	// answer at all, and once a caller is back the same path resolves.
	if _, code := m.lookup("/a.txt"); code != -EIO {
		t.Errorf("lookup while offline = %d, want -EIO (%d); a lookup must not "+
			"answer from an index it knows may be stale", code, -EIO)
	}
	m.adopt(f)
	if _, code := m.lookup("/a.txt"); code != 0 {
		t.Error("a closed watch channel emptied the index: /a.txt is gone after " +
			"the mount came back, though the caller never stopped having it")
	}
}

// startWatch is idempotent: two connections replaced in a row must not leave two
// goroutines consuming the channel.
func TestStartWatchIsIdempotent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "/a.txt", Kind: node.KindFile, Size: 1})
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/a.txt")
		return code == 0
	}, "the event was not applied")
}

// stopWatch must stop the loop. Without it, a reconnect would accumulate
// goroutines that all apply events to the same mount.
//
// The contract is about batches that were already queued, not about batches
// emitted afterwards. The interesting case -- and the one that was wrong -- is a
// loop that has already received a batch, or is about to, when the stop arrives:
// its select then has two ready cases, and Go picks one at random. The first
// event is used as a barrier, to prove the loop is running and parked;
// the rest are queued immediately before the stop, while the test's own goroutine
// is still running and the loop has had no chance to schedule. So the stop
// happens with a non-empty channel, which is exactly the situation the re-check in
// watchLoop exists for.
//
// Emitting after stopWatch returns would not reach that case at all: the channel
// is empty at the moment done closes, so the loop has only one ready case and
// leaves either way. That version of this test passed against a mutant with the
// re-check deleted, which is why the events are queued up front.
func TestStopWatchStopsApplyingEvents(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()

	// Barrier: prove the loop is up and consuming, so the stop below is not
	// racing the goroutine's own startup.
	f.emit(1000, node.Node{Path: "/barrier.txt", Kind: node.KindFile, Size: 1})
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/barrier.txt")
		return code == 0
	}, "the loop did not start consuming")

	// Queue these without yielding, then stop. They are the batches the loop must
	// not apply.
	const late = 8
	for i := 1; i <= late; i++ {
		f.emit(uint64(i), node.Node{
			Path: fmt.Sprintf("/late-%d.txt", i),
			Kind: node.KindFile,
			Size: 1,
		})
	}
	m.stopWatch()

	time.Sleep(50 * time.Millisecond)
	for i := 1; i <= late; i++ {
		if _, code := m.lookup(fmt.Sprintf("/late-%d.txt", i)); code == 0 {
			t.Errorf("event %d was applied after stopWatch", i)
		}
	}
}

// stopWatch before startWatch, and twice in a row, must be safe: the reconnect
// path can reach a stop with no loop to stop.
func TestStopWatchIsIdempotentAndSafeBeforeStart(t *testing.T) {
	m := newTestMount(t)
	m.stopWatch()
	m.startWatch()
	m.stopWatch()
	m.stopWatch()
}

// A gap detected through the live loop, rather than through applyEvent directly.
// This is the end-to-end shape of the drop.
func TestASequenceGapThroughTheLoop(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.watchForTest()
	m.startWatch()
	defer m.stopWatch()

	f.emit(1, node.Node{Path: "/a.txt", Kind: node.KindFile})
	f.emit(4, node.Node{Path: "/b.txt", Kind: node.KindFile})

	waitFor(t, 2*time.Second, func() bool { return m.stale() },
		"a gap in the live stream was not detected")
	// Both events still apply: noticing the gap is not a reason to discard the
	// batch, which is newer information than anything else on hand.
	if _, code := m.lookup("/b.txt"); code != 0 {
		t.Error("the batch carrying the gap was discarded")
	}
}
