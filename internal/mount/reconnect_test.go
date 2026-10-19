package mount

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/node"
)

// ------------------------------------------------------------------ the tests

// A reconnect has to do three things, and the test checks all three because a
// supervisor that only dials is not a reconnect: it must replace the caller, it
// must clear the offline state, and it must make the mount describe the *new*
// tree rather than the old one. The last is the whole point -- events were missed
// while the connection was down, so the tree cannot be assumed unchanged.
func TestReconnectResubscribesAndInvalidates(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	// The old connection's tree, which must not survive the reconnect.
	fakeOf(t, m).addFile("/stale.txt", 4)

	reconnected := make(chan struct{}, 1)
	var fresh *fakeCaller
	var mu sync.Mutex
	rc := func() (Caller, error) {
		f := newFakeCaller()
		f.addDir("/")
		f.addFile("/fresh.txt", 4)
		f.watchForTest()
		mu.Lock()
		fresh = f
		mu.Unlock()
		select {
		case reconnected <- struct{}{}:
		default:
		}
		return f, nil
	}

	// The kernel was holding /fresh.txt before the connection was lost, so the
	// refresh has somewhere to walk. Without a handed-out entry the invalidation
	// loop is empty and could not tell a working refresh from a missing one.
	m.noteInode(1, "/fresh.txt")

	m.setDisconnected(true)
	go m.supervise(rc, time.Second)

	waitFor(t, 2*time.Second, func() bool { return !m.disconnected() },
		"supervisor did not clear the disconnected flag after a successful reconnect")
	select {
	case <-reconnected:
	default:
		t.Fatal("supervisor never called the reconnect function")
	}

	// The new tree is visible.
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/fresh.txt")
		return code == 0
	}, "the new tree was not picked up after reconnecting")

	// And the old one is gone with it. An events table kept across a reconnect
	// would answer /stale.txt from the dead connection, which is the staleness
	// the reconnect exists to remove.
	mu.Lock()
	f := fresh
	mu.Unlock()
	if f == nil {
		t.Fatal("the reconnect function never produced a caller")
	}
	if _, code := m.lookup("/stale.txt"); code != -ENOENT {
		t.Errorf("a path from the dead connection = %d, want -ENOENT (%d)",
			code, -ENOENT)
	}
}

// The event table a reconnect clears must really be cleared. This is the narrow
// case the table exists for: a path named by a push that the caller's index has
// not caught up with. If it survives the reconnect, the mount keeps answering a
// path the new connection has never heard of.
func TestReconnectClearsTheEventTable(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	// A push names a path the fake's index does not have, which is exactly what
	// the event table is for.
	m.startWatch()
	m.watchForTest()
	m.emit(1, node.Node{Path: "/pushed.txt", Kind: node.KindFile, Size: 7})
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/pushed.txt")
		return code == 0
	}, "the pushed path was not recorded, so this test cannot observe a clear")
	m.stopWatch()

	rc := func() (Caller, error) {
		f := newFakeCaller()
		f.addDir("/")
		f.watchForTest()
		return f, nil
	}
	m.setDisconnected(true)
	go m.supervise(rc, time.Second)
	waitFor(t, 2*time.Second, func() bool { return !m.disconnected() },
		"never reconnected")

	if _, code := m.lookup("/pushed.txt"); code != -ENOENT {
		t.Errorf("a path pushed on the dead connection = %d, want -ENOENT (%d)",
			code, -ENOENT)
	}
}

// The sequence counter is the *old* connection's and must go with the table it
// counted for. A new connection numbers its events from wherever it likes, so a
// counter kept across the reconnect makes the new stream's first batch look like
// the tail of a gap -- and the mount then reports itself stale for ever, having
// missed events that were never sent.
//
// This is why the counter is cleared rather than carried: a fresh counter that
// starts low is not the problem, it is the reason.
func TestReconnectRestartsTheSequenceCounter(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	// The old connection had counted up to a high number, which is the realistic
	// case after any uptime at all.
	m.startWatch()
	old := fakeOf(t, m)
	old.watchForTest()
	if !old.emit(5000, node.Node{Path: "/old.txt", Kind: node.KindFile, Size: 3}) {
		t.Fatal("the old batch could not be sent")
	}
	waitFor(t, 2*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastSeq == 5000
	}, "the old counter did not advance, so this test cannot observe a reset")

	var fresh *fakeCaller
	rc := func() (Caller, error) {
		f := newFakeCaller()
		f.addDir("/")
		f.watchForTest()
		fresh = f
		return f, nil
	}
	m.setDisconnected(true)
	go m.supervise(rc, time.Second)
	waitFor(t, 2*time.Second, func() bool { return !m.disconnected() }, "never reconnected")

	if fresh == nil {
		t.Fatal("the reconnect function never produced a caller")
	}
	// A mount that has just reconnected has no reason to believe it missed
	// anything: the refresh it ran is the whole story so far.
	if m.stale() {
		t.Error("the mount came back from a reconnect already marked stale")
	}

	// The new connection's own first batch. If the old counter survived, this
	// reads as event 5001 of 1 and the mount claims a gap it never had.
	if !fresh.emit(1, node.Node{Path: "/new.txt", Kind: node.KindFile, Size: 3}) {
		t.Fatal("the new caller's watch channel was not started")
	}
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/new.txt")
		return code == 0
	}, "the new connection's first batch was never applied")
	if m.stale() {
		t.Error("the new connection's first batch was read as a gap; the old " +
			"sequence counter survived the reconnect")
	}
}

// Offline must be EIO, never ENOENT. A program that sees ENOENT concludes the
// file was deleted and may act destructively on that conclusion; EIO makes it
// retry.
func TestOfflineReadsAreEioNotEnoent(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)
	m.setDisconnected(true)

	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != -EIO {
		t.Errorf("offline Getattr = %d, want -EIO (%d)", code, -EIO)
	}
	if _, code := m.Open("/a.txt", OReadOnly); code != -EIO {
		t.Errorf("offline Open = %d, want -EIO", code)
	}
	if code := m.Mkdir("/d", 0o755); code != -EIO {
		t.Errorf("offline Mkdir = %d, want -EIO", code)
	}
}

// Every operation that can reach the wire is refused while offline, and every one
// of them is refused with EIO rather than with an errno that means something else.
// The distinction that matters is against ENOENT: a program retries EIO and gives
// up on ENOENT.
func TestOfflineRefusesEveryOperationWithEio(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)
	fakeOf(t, m).addDir("/d")
	m.setDisconnected(true)

	var st Stat
	var sfs Statfs
	checks := []struct {
		name string
		code int
	}{
		{"Getattr", m.Getattr("/a.txt", &st, 0)},
		{"Readdir", m.Readdir("/", func(string, *Stat, int64) bool { return true }, 0, 0)},
		{"Open", second(m.Open("/a.txt", OReadOnly))},
		{"Opendir", second(m.Opendir("/d"))},
		{"Read", m.Read("/a.txt", make([]byte, 8), 0, 0)},
		{"Create", second(m.Create("/new.txt", 0o644, OReadWrite))},
		{"Write", m.Write("/a.txt", []byte("hello"), 0, 0)},
		{"Truncate", m.Truncate("/a.txt", 0, 0)},
		{"Mkdir", m.Mkdir("/e", 0o755)},
		{"Unlink", m.Unlink("/a.txt")},
		{"Rmdir", m.Rmdir("/d")},
		{"Rename", m.Rename("/a.txt", "/b.txt")},
	}
	for _, c := range checks {
		if c.code != -EIO {
			t.Errorf("offline %s = %d, want -EIO (%d)", c.name, c.code, -EIO)
		}
	}

	// A path that does not exist is still EIO rather than ENOENT: the mount does
	// not know what exists, so it cannot honestly report absence.
	if code := m.Getattr("/never-existed", &st, 0); code != -EIO {
		t.Errorf("offline Getattr on an unknown path = %d, want -EIO (%d)", code, -EIO)
	}
	// The root is refused too, and that is the point rather than a wart: a mount
	// that answered *some* paths while the connection was gone would force every
	// program above it to invent a rule for which answers to trust, and the rule
	// it would invent -- "a successful getattr means the path is there" -- would
	// be false. One rule, EIO, is a rule a caller can act on.
	if code := m.Getattr("/", &st, 0); code != -EIO {
		t.Errorf("offline Getattr on the root = %d, want -EIO (%d)", code, -EIO)
	}
	// Statfs goes the other way and is the single exception, because it is
	// constants rather than a lookup: there is no wire call to fail and no stale
	// answer to give, and cgofuse's fsstat callback cannot report an error at
	// all. A tool that statfs'es the mount point before doing anything else gets
	// the numbers and then EIO from its first real operation.
	if code := m.Statfs("/", &sfs); code != 0 {
		t.Errorf("offline Statfs = %d, want 0: it is synthetic and touches no wire", code)
	}
}

// second extracts the errno from an operation that also returns a handle.
func second(fh uint64, code int) int { return code }

// A dirty open that is discarded must be reported by path. The write reported
// success, the mount is still there, and the content is gone: a discard with
// nothing in the log is indistinguishable from a save that worked.
func TestDisconnectDiscardsDirtyOpensAndReportsThem(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if n := m.Write("/a.txt", []byte("xyz"), 0, fh); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}

	var reported []string
	var mu sync.Mutex
	m.mu.Lock()
	m.onDiscard = func(p string) { mu.Lock(); reported = append(reported, p); mu.Unlock() }
	m.mu.Unlock()

	m.setDisconnected(true)
	m.discardDirtyOnDisconnect()

	mu.Lock()
	got := append([]string(nil), reported...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "/a.txt" {
		t.Errorf("discarded paths = %v, want [/a.txt]", got)
	}

	// The handle really is gone, not merely marked. A handle left in the table
	// would be flushed later -- over whatever the owner's copy became while the
	// connection was down -- which is the data loss the discard exists to
	// prevent.
	m.mu.Lock()
	_, still := m.files[fh]
	cleanHandles := len(m.files)
	m.mu.Unlock()
	if still {
		t.Error("the discarded handle is still in the table")
	}
	if cleanHandles != 0 {
		t.Errorf("the handle table holds %d entries after the discard, want 0",
			cleanHandles)
	}

	// And a flush of that handle is a no-op success rather than an upload: it is
	// gone, and an unknown handle is a success.
	if c := m.flush(fh); c != 0 {
		t.Errorf("flush of a discarded handle = %d, want 0", c)
	}
}

// A clean open is left alone by the discard. It holds no content that could be
// lost, and dropping it would take a file descriptor's identity away from the
// kernel for no reason.
func TestDisconnectKeepsCleanOpens(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)
	fh, code := m.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}

	m.setDisconnected(true)
	m.discardDirtyOnDisconnect()

	m.mu.Lock()
	_, present := m.files[fh]
	m.mu.Unlock()
	if !present {
		t.Error("a clean handle was discarded; only dirty ones lose content")
	}
}

// A dirty flush while offline must fail, and must leave the handle dirty. A flush
// that reported success while the connection was gone would be the lost save this
// package exists to prevent -- and the handle staying dirty is what lets Release
// report the failure.
func TestFlushOfflineFailsAndLeavesTheHandleDirty(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)
	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if n := m.Write("/a.txt", []byte("xyz"), 0, fh); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}

	m.setDisconnected(true)
	if c := m.flush(fh); c != -EIO {
		t.Errorf("offline flush = %d, want -EIO (%d)", c, -EIO)
	}
	m.mu.Lock()
	h, ok := m.files[fh]
	dirty := ok && !h.isClean()
	m.mu.Unlock()
	if !dirty {
		t.Error("the handle was marked clean by a flush that never reached the wire")
	}
	// No upload was attempted, which is the other half of the promise: the check
	// is before the Put, not after it.
	if n := fakeOf(t, m).putCount(); n != 0 {
		t.Errorf("%d uploads were attempted while offline, want 0", n)
	}
}

// Release must free its handle even while offline. It reports the failure, which
// is what makes the lost save visible, but the handle is released: the kernel has
// finished with the descriptor, and leaving the entry would leak it for the life
// of the mount.
func TestReleaseFreesTheHandleWhileOffline(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)
	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if n := m.Write("/a.txt", []byte("xyz"), 0, fh); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}

	m.setDisconnected(true)
	if c := m.Release("/a.txt", fh); c != -EIO {
		t.Errorf("offline Release = %d, want -EIO (%d): the content did not land", c, -EIO)
	}
	m.mu.Lock()
	_, present := m.files[fh]
	m.mu.Unlock()
	if present {
		t.Error("Release left its handle in the table while offline; it would leak")
	}
}

// Offline is a state, not a verdict. The same mount serves reads again once the
// supervisor has a caller, which is what makes EIO the right answer to give while
// it lasts: the program that retried is rewarded.
func TestOperationsWorkAgainAfterReconnect(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/stale.txt", 4)
	m.setDisconnected(true)

	var st Stat
	if code := m.Getattr("/stale.txt", &st, 0); code != -EIO {
		t.Fatalf("offline Getattr = %d, want -EIO", code)
	}

	m.adopt(fakeOf(t, m))

	if code := m.Getattr("/stale.txt", &st, 0); code != 0 {
		t.Errorf("Getattr after a reconnect = %d, want 0", code)
	}
	if st.Size != 4 {
		t.Errorf("size after a reconnect = %d, want 4", st.Size)
	}
}

// The backoff has to be a real delay and a real retry. A supervisor that retried
// instantly would spin, and one that gave up on the first failure would never
// reconnect at all.
func TestReconnectBacksOff(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 40 * time.Millisecond

	attempts := 0
	var stamps []time.Time
	rc := func() (Caller, error) {
		attempts++
		stamps = append(stamps, time.Now())
		if attempts < 4 {
			return nil, errors.New("still down")
		}
		f := newFakeCaller()
		f.addDir("/")
		f.watchForTest()
		return f, nil
	}

	m.setDisconnected(true)
	done := make(chan struct{})
	go func() { m.supervise(rc, time.Second); close(done) }()
	waitFor(t, 3*time.Second, func() bool { return !m.disconnected() }, "never reconnected")

	if len(stamps) < 2 {
		t.Fatalf("only %d attempts; backoff was not retried", len(stamps))
	}
	first := stamps[1].Sub(stamps[0])
	if first < 4*time.Millisecond {
		t.Errorf("retry after %v, want at least the minimum backoff (5ms)", first)
	}
}

// The delay has to grow, not merely repeat: a peer that is gone for an hour must
// not be dialled once per minimum interval. The assertion is loose on purpose --
// a loaded machine can stretch any single sleep -- but it distinguishes a schedule
// that backs off from one that does not.
func TestReconnectBackoffGrows(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 40 * time.Millisecond

	var mu sync.Mutex
	var stamps []time.Time
	rc := func() (Caller, error) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		n := len(stamps)
		mu.Unlock()
		if n < 5 {
			return nil, errors.New("still down")
		}
		f := newFakeCaller()
		f.addDir("/")
		f.watchForTest()
		return f, nil
	}

	m.setDisconnected(true)
	go m.supervise(rc, 2*time.Second)
	waitFor(t, 3*time.Second, func() bool { return !m.disconnected() }, "never reconnected")

	mu.Lock()
	s := append([]time.Time(nil), stamps...)
	mu.Unlock()
	if len(s) < 4 {
		t.Fatalf("only %d attempts, want at least 4", len(s))
	}
	// gaps: min, 2*min, 4*min. The third gap should be clearly larger than the
	// first; with min = 5ms that is 5ms against 20ms.
	first := s[1].Sub(s[0])
	third := s[3].Sub(s[2])
	if third <= first {
		t.Errorf("the retry gap did not grow: first %v, third %v", first, third)
	}
}

// The backoff is capped. Without the cap an outage of a day would end with a
// retry interval of hours, so the peer coming back would go unnoticed for hours
// after it did.
func TestReconnectBackoffIsCapped(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 4 * time.Millisecond
	m.opts.ReconnectMax = 12 * time.Millisecond

	var mu sync.Mutex
	var stamps []time.Time
	rc := func() (Caller, error) {
		mu.Lock()
		stamps = append(stamps, time.Now())
		n := len(stamps)
		mu.Unlock()
		if n < 7 {
			return nil, errors.New("still down")
		}
		f := newFakeCaller()
		f.addDir("/")
		f.watchForTest()
		return f, nil
	}

	m.setDisconnected(true)
	go m.supervise(rc, 3*time.Second)
	waitFor(t, 4*time.Second, func() bool { return !m.disconnected() }, "never reconnected")

	mu.Lock()
	s := append([]time.Time(nil), stamps...)
	mu.Unlock()
	if len(s) < 7 {
		t.Fatalf("only %d attempts, want 7", len(s))
	}
	// By the fifth gap the schedule is at the cap: 4, 8, 12, 12... so the gap
	// from attempt 5 to 6 is 12ms and not 64ms. The bound is generous because the
	// point is to catch an uncapped doubling, not to measure a sleep.
	late := s[6].Sub(s[5])
	if late > 60*time.Millisecond {
		t.Errorf("the retry gap grew to %v; the cap is 12ms", late)
	}
}

// A mount whose owner is gone for minutes must stop being a mount, rather than
// hanging every program that touches it forever.
func TestGiveUpAfterThreshold(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	gaveUp := make(chan struct{})
	m.mu.Lock()
	m.onGiveUp = func() { close(gaveUp) }
	m.mu.Unlock()

	rc := func() (Caller, error) { return nil, errors.New("gone") }

	m.setDisconnected(true)
	done := make(chan struct{})
	go func() { m.supervise(rc, 60*time.Millisecond); close(done) }()

	select {
	case <-gaveUp:
	case <-time.After(3 * time.Second):
		t.Fatal("the supervisor never gave up; a dead mount would hang forever")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the supervisor called the give-up hook but never returned")
	}
	// Giving up does not silently pretend the mount is healthy.
	if !m.disconnected() {
		t.Error("the supervisor gave up but left the mount looking connected")
	}
}

// A give-up deadline of zero means "use the option", and an option of zero means
// "do not wait" rather than "wait forever". A zero that meant forever would be a
// mount that outlives its owner, which is the failure the give-up exists for.
func TestGiveUpWithAZeroDeadlineUsesTheOption(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond
	m.opts.GiveUpAfter = 50 * time.Millisecond

	gaveUp := make(chan struct{}, 1)
	m.mu.Lock()
	m.onGiveUp = func() { gaveUp <- struct{}{} }
	m.mu.Unlock()

	m.setDisconnected(true)
	rc := func() (Caller, error) { return nil, errors.New("gone") }
	// giveUp = 0, so the option decides.
	go m.supervise(rc, 0)

	select {
	case <-gaveUp:
	case <-time.After(3 * time.Second):
		t.Fatal("a zero deadline did not fall back to Options.GiveUpAfter")
	}
}

// A reconnect function that returns no client and no error is a bug in the
// dialler. Installing the nil caller would turn every later operation into a
// nil-pointer panic far from the cause, so it is treated as a failure.
func TestReconnectRefusesANilCaller(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond
	m.opts.GiveUpAfter = 80 * time.Millisecond

	gaveUp := make(chan struct{}, 1)
	m.mu.Lock()
	m.onGiveUp = func() { gaveUp <- struct{}{} }
	m.mu.Unlock()

	calls := 0
	rc := func() (Caller, error) {
		calls++
		return nil, nil
	}

	m.setDisconnected(true)
	go m.supervise(rc, 0)

	select {
	case <-gaveUp:
	case <-time.After(3 * time.Second):
		t.Fatal("a nil caller with no error was accepted, or the supervisor never stopped")
	}
	if calls < 2 {
		t.Errorf("the supervisor made %d attempts before giving up on a nil caller; "+
			"a transient problem should be retried", calls)
	}
	// The nil was never installed, so the mount is still offline rather than
	// panicking on its next operation.
	if !m.disconnected() {
		t.Error("the mount looks connected after a reconnect that produced nothing")
	}
	var st Stat
	if code := m.Getattr("/a.txt", &st, 0); code != -EIO {
		t.Errorf("Getattr after a failed reconnect = %d, want -EIO (%d)", code, -EIO)
	}
}

// A reconnect has to restart the watch on the *new* caller's channel. If the
// watch machinery were one-shot -- which a sync.Once would have made it -- the new
// connection would be live for reads but deaf to every push, and the tree would
// drift with nothing to notice it.
func TestReconnectWatchesTheNewCaller(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	// The mount starts watching the old caller, and then loses it.
	m.startWatch()
	m.watchForTest()

	var fresh *fakeCaller
	rc := func() (Caller, error) {
		f := newFakeCaller()
		f.addDir("/")
		f.watchForTest()
		fresh = f
		return f, nil
	}

	m.setDisconnected(true)
	go m.supervise(rc, time.Second)
	waitFor(t, 2*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.isDisconnected
	}, "never reconnected")

	if fresh == nil {
		t.Fatal("the reconnect function never produced a caller")
	}

	// A push on the new channel must be recorded. The path is not in any index,
	// so only the event table can answer it -- which means the answer proves the
	// batch was consumed rather than that the fake happened to know the path.
	if !fresh.emit(1, node.Node{Path: "/pushed-after-reconnect.txt", Kind: node.KindFile, Size: 5}) {
		t.Fatal("the new caller's watch channel was not started; the emit was refused")
	}
	waitFor(t, 2*time.Second, func() bool {
		_, code := m.lookup("/pushed-after-reconnect.txt")
		return code == 0
	}, "a push on the new connection was never applied")
}

// Once adopt has returned, nothing the dead connection sends can reach the table.
//
// The test queues stale batches *before* the reconnect, which is what makes it a
// test rather than a formality: the old loop is parked on that channel when the
// swap happens, so if anything were still consuming it the batches would land. It
// deliberately does not try to observe *when* the old loop stopped -- that is the
// wait inside stopWatch, and a probe showed it is not black-box observable, since
// a retired goroutine always exits before anything can look at it. What is
// observable, and what the mount actually promises, is that the dead connection's
// view never survives.
func TestTheOldWatchIsRetiredBeforeTheNewCallerIsInstalled(t *testing.T) {
	m := newTestMount(t)
	m.opts.ReconnectMin = 5 * time.Millisecond
	m.opts.ReconnectMax = 20 * time.Millisecond

	m.startWatch()
	oldFake := fakeOf(t, m)
	oldFake.watchForTest()
	// Let the loop park on the channel, so the batches below are genuinely
	// waiting for a reader.
	time.Sleep(5 * time.Millisecond)

	nf := newFakeCaller()
	nf.addDir("/")
	nf.watchForTest()

	// Queued before the swap, so a loop that is still consuming the dead
	// connection will pick them up.
	queued := 0
	for i := 0; i < 32; i++ {
		if oldFake.emit(uint64(100+i), node.Node{
			Path: "/from-the-dead" + string(rune('a'+i%26)) + ".txt",
			Kind: node.KindFile, Size: 3,
		}) {
			queued++
		}
	}
	if queued == 0 {
		t.Fatal("no stale batch could be queued, so this test cannot observe anything")
	}

	m.adopt(nf)

	// A batch sent *after* adopt has returned is the case the dropped-connection
	// guarantee is really about, and it must not be read either.
	oldFake.emit(999, node.Node{Path: "/after-adopt.txt", Kind: node.KindFile, Size: 3})
	time.Sleep(100 * time.Millisecond)

	for _, p := range []string{"/from-the-deada.txt", "/after-adopt.txt"} {
		if _, code := m.lookup(p); code != -ENOENT {
			t.Errorf("%s resolves after a reconnect (%d); a batch from the dead "+
				"connection survived the swap", p, code)
		}
	}
	// The other queued names are checked too, since which one a surviving loop
	// would have applied depends on which batch it happened to read first.
	for i := 0; i < 32; i++ {
		p := "/from-the-dead" + string(rune('a'+i%26)) + ".txt"
		if _, code := m.lookup(p); code != -ENOENT {
			t.Errorf("%s resolves after a reconnect (%d); a batch queued on the "+
				"dead connection was applied after the swap", p, code)
		}
	}
}

// A reconnect invalidates every path the kernel may be caching. This is the point
// of reconnecting: the kernel's cached attributes describe the tree as it was
// before the outage, and nothing else will tell it they are wrong.
func TestReconnectInvalidatesEveryHandedOutPath(t *testing.T) {
	m := newTestMount(t)

	var mu sync.Mutex
	seen := map[string]int{}
	m.mu.Lock()
	m.invalidatePath = func(p string) bool {
		mu.Lock()
		seen[p]++
		mu.Unlock()
		return true
	}
	m.mu.Unlock()

	m.noteInode(1, "/a.txt")
	m.noteInode(2, "/dir")
	m.noteInode(3, "/dir/b.txt")

	fakeOf(t, m).addDir("/")
	m.adopt(fakeOf(t, m))

	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"/a.txt", "/dir", "/dir/b.txt"} {
		if seen[want] == 0 {
			t.Errorf("%s was handed to the kernel but was not invalidated on reconnect", want)
		}
	}
}

// A path that has been released is still invalidated. The kernel's attribute cache
// outlives the file descriptor that put the path there -- a program can stat a
// path it never opened -- so invalidating only the currently-open files would
// leave the most recently closed ones stale, which are exactly the ones a "save
// and close" leaves behind.
func TestInvalidationCoversReleasedPaths(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)

	fh, code := m.Open("/a.txt", OReadOnly)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if code := m.Release("/a.txt", fh); code != 0 {
		t.Fatalf("Release = %d, want 0", code)
	}

	var mu sync.Mutex
	seen := 0
	m.mu.Lock()
	m.invalidatePath = func(p string) bool {
		if p == "/a.txt" {
			mu.Lock()
			seen++
			mu.Unlock()
		}
		return true
	}
	m.mu.Unlock()

	m.invalidateAll()

	mu.Lock()
	n := seen
	mu.Unlock()
	if n == 0 {
		t.Error("a released path was not invalidated; the kernel keeps its attributes")
	}
}

// The refresh loop is bounded by the invalidation table rather than walking an
// unbounded set, and the bound is a real one: the table is capped at construction
// and paths fall out of it as new ones go in.
func TestInvalidateAllWalksABoundedTable(t *testing.T) {
	m := newTestMount(t)
	m.opts.InvalidateTable = 4

	for i := 0; i < 32; i++ {
		m.noteInode(uint64(i+1), "/p"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}

	var mu sync.Mutex
	n := 0
	m.mu.Lock()
	m.invalidatePath = func(string) bool { mu.Lock(); n++; mu.Unlock(); return true }
	m.mu.Unlock()

	m.invalidateAll()

	mu.Lock()
	got := n
	mu.Unlock()
	if got == 0 {
		t.Fatal("nothing was invalidated")
	}
	if got > 32 {
		t.Errorf("the refresh walked %d paths for 32 inserts; the table is unbounded", got)
	}
}

// A dirty handle must not survive into a later flush, and the discard must be what
// the reconnect itself does -- not something the test does for it.
//
// This is the data-loss case the discard exists for: the buffer holds content the
// owner never received, and replaying it after the connection came back would
// overwrite whatever the owner's copy became while the mount was down.
func TestDiscardedContentIsNeverUploaded(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)
	fakeOf(t, m).setContent("/a.txt", []byte("old"))

	fh, code := m.Open("/a.txt", OReadWrite)
	if code != 0 {
		t.Fatalf("Open = %d, want 0", code)
	}
	if n := m.Write("/a.txt", []byte("NEW"), 0, fh); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}

	m.setDisconnected(true)

	// The reconnect discards. Calling discardDirtyOnDisconnect from here would be
	// the test doing the work it is supposed to be checking: an adopt that forgot
	// the discard would then still pass.
	fresh := newFakeCaller()
	fresh.addDir("/")
	fresh.watchForTest()
	// addFile creates the entry; setContent only sets its bytes. A fake given
	// bytes but no node would answer ENOENT from Read, and this test would then
	// be asserting on a fixture it had built wrongly rather than on the discard.
	fresh.addFile("/a.txt", 3)
	fresh.setContent("/a.txt", []byte("old"))
	m.adopt(fresh)

	// Nothing is left to flush.
	if n := m.flushAll(); n != 0 {
		t.Errorf("%d handles failed to flush after a discard; none should remain", n)
	}

	// The owner's copy is untouched. This is the assertion that makes the test
	// worth having: "the write was dropped" and "the write was applied" are the
	// two outcomes, and only this one tells them apart.
	if n := fresh.putCount(); n != 0 {
		t.Errorf("%d uploads were made from discarded content, want 0", n)
	}
	body, _ := fresh.Read("/a.txt", 0, -1)
	if string(body) != "old" {
		t.Errorf("the owner's content is %q, want %q: discarded content was replayed",
			body, "old")
	}

	// The write really did reach the mount first, so a "nothing was uploaded"
	// result cannot be explained by the write having been refused.
	m.mu.Lock()
	_, still := m.files[fh]
	m.mu.Unlock()
	if still {
		t.Error("the discarded handle is still in the table after the reconnect")
	}
}

// A discarded path is reported once per path, not once per handle. Two handles on
// one path are one lost file as far as the user is concerned, and a log that names
// it twice suggests two.
func TestDiscardReportsEachPathOnce(t *testing.T) {
	m := newTestMount(t)
	fakeOf(t, m).addFile("/a.txt", 3)

	fh1, _ := m.Open("/a.txt", OReadWrite)
	fh2, _ := m.Open("/a.txt", OReadWrite)
	if n := m.Write("/a.txt", []byte("111"), 0, fh1); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}
	if n := m.Write("/a.txt", []byte("222"), 0, fh2); n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}

	var reported []string
	m.mu.Lock()
	m.onDiscard = func(p string) { reported = append(reported, p) }
	m.mu.Unlock()

	m.discardDirtyOnDisconnect()

	// Both handles are gone...
	m.mu.Lock()
	left := len(m.files)
	m.mu.Unlock()
	if left != 0 {
		t.Errorf("%d handles survived the discard, want 0", left)
	}
	// ...and the path is reported twice, once per handle. That is the honest
	// count: two unsaved buffers really were lost, and collapsing them would hide
	// the second write from whoever reads the log.
	if len(reported) != 1 {
		t.Errorf("discard reported %v, want one entry for the path", reported)
	}
}

// ---------------------------------------------------------------- the plumbing

// disconnected and setDisconnected are production methods, in watch.go: the watch
// loop is what sets the flag when the transport's channel closes, so they belong
// to the code rather than to the tests that drive them.

// watchForTest returns the fake's watch channel, creating it if needed.
//
// It is needed because fakeCaller.Watch creates the channel on its first call, so
// an emit before the mount has started watching is silently refused -- a test that
// emitted first would assert on a batch that was never sent and could not fail.
func (m *Mount) watchForTest() chan Batch { return fakeOfT(m).watchForTest() }

// emit pushes one batch on the fake's watch channel and reports whether it was
// accepted, so a test can fail loudly rather than assert on a batch that was
// dropped for want of a reader.
func (m *Mount) emit(seq uint64, nodes ...node.Node) bool {
	return fakeOfT(m).emit(seq, nodes...)
}

// fakeOfT reaches the fake without a *testing.T, for the helpers above, which are
// called from tests that have already established that the caller is a fake.
func fakeOfT(m *Mount) *fakeCaller {
	c := m.call()
	f, ok := c.(*fakeCaller)
	if !ok {
		panic("mount: the caller is not a fakeCaller")
	}
	return f
}
