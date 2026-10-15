package mount

import (
	"errors"
	"io"
	"sync"
	"testing"

	"peerfold/internal/node"
)

// fakeCaller is a Caller backed by two maps. It exists so the operation core
// can be tested without a wire, a server or a client, which is the whole reason
// Caller is an interface.
//
// It is deliberately not a mock in the strict sense: it holds real state
// (nodes and content), so a test can drive a whole sequence of operations and
// assert on the resulting tree. What it does not do is simulate latencies,
// partial writes or failures; a test that needs those drives a real client.
//
// The mutex is here because the core's watch goroutine and its request
// goroutine both touch the maps in later tasks. The core calls every Caller
// method from one goroutine, but the fake is also read by the test goroutine,
// so it has to be safe regardless.
type fakeCaller struct {
	mu    sync.Mutex
	nodes map[string]node.Node
	data  map[string][]byte
	calls []string

	// listErr, when set, is returned by List for listErrPath only (or for every
	// path when listErrPath is empty). It exists so a test can prove the mount
	// maps a failing caller rather than reading the nil children as an empty
	// directory -- the two are indistinguishable at the call site unless the
	// error is checked.
	//
	// The path restriction is not a convenience. lookup() reaches the caller to
	// resolve a path, so an error injected for every path is raised inside
	// lookup() and the operation returns before the code under test runs. A test
	// written that way passes whether or not the operation maps the error at all,
	// which is a test that cannot fail.
	listErr     error
	listErrPath string

	// readErrAt makes every read of a chunk starting at that offset fail with
	// errInjectedReadFailure.
	//
	// It is a sentinel rather than an injectable error because the fake cannot
	// tell a chunk read from the resolution calls the mount makes first: with one
	// error value assigned to every offset, the mount fails while resolving the
	// path and never reaches the read under test. A test written that way passes
	// whether or not the read path maps anything, which is a test that cannot
	// fail. Tests for the failure *mapping* use listErr, which fails resolution;
	// this exists for tests of the partial-read behaviour.
	readErrAt int64

	// watchCh is created on first use by Watch, so a test can hold the channel
	// before the core starts watching and drive it afterwards.
	watchCh chan []node.Node
}

// fakeCaller implements Caller. Asserted here rather than relied on implicitly
// so that a change to the interface fails in the file that owns the test double,
// with a message that says what is wrong, instead of somewhere in a test body.
var _ Caller = (*fakeCaller)(nil)

func newFakeCaller() *fakeCaller {
	return &fakeCaller{nodes: map[string]node.Node{}, data: map[string][]byte{}}
}

// ensureParents records the directories a path implies. A filesystem cannot
// hold /a/b.txt without /a existing, and a fake that allowed it would let a
// test build a tree the real federation cannot represent — which is how the
// `lookup("/a/nope")` case came out as ENOTDIR instead of ENOENT.
func (f *fakeCaller) ensureParents(p string) {
	for {
		parent, _, err := SplitParent(p)
		if err != nil {
			return
		}
		if _, ok := f.nodes[parent]; ok {
			return
		}
		f.nodes[parent] = node.Node{Path: parent, Kind: node.KindDir, Mode: 0o755}
		p = parent
	}
}

func (f *fakeCaller) addDir(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureParents(p)
	f.nodes[p] = node.Node{Path: p, Kind: node.KindDir, Mode: 0o755}
}

func (f *fakeCaller) addFile(p string, size int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureParents(p)
	f.nodes[p] = node.Node{Path: p, Kind: node.KindFile, Size: int64(size), Mode: 0o644}
}

func (f *fakeCaller) addSymlink(p string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureParents(p)
	// 1<<27 is os.ModeSymlink; the client records the full FileMode in Mode.
	// The node is KindSymlink as well as carrying the mode bit, because the
	// core branches on Kind and a fake that set only one of the two would let
	// a bug in either path go unnoticed.
	f.nodes[p] = node.Node{Path: p, Kind: node.KindSymlink, Mode: 1 << 27}
}

// readErrSentinel reports the error a read starting at off should fail with.
//
// The trigger is readErrAt == 0 meaning "off is set", because an offset of 0 is
// never itself the sentinel: a failure injected at offset 0 would make the
// fixture's own setup reads fail. Requiring a non-zero trigger offset therefore
// costs nothing and removes a way for a test to silently do nothing.
func (f *fakeCaller) readErrSentinel(off int64) error {
	if f.readErrAt == 0 || f.readErrAt != off {
		return nil
	}
	return errInjectedRead
}

// errInjectedRead is the failure errInjectedReadAt makes a chunk read return. It
// is not a token the mount knows, so errnoFor maps it to EIO, which is the same
// answer a broken connection gets.
var errInjectedRead = errors.New("test: injected read failure")

// setReadFailure makes the chunk read starting at off fail. See readErrAt for why
// only one offset is named.
func (f *fakeCaller) setReadFailure(off int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readErrAt = off
}

// setContent replaces a file's bytes without recording a call.
//
// It is separate from addContent so the two halves of a fixture can be stated
// separately: a size the *index* claims, and the bytes the *owner* actually has.
// Those differing is what a stale index looks like, and one helper that always
// made them agree could not express it.
func (f *fakeCaller) setContent(p string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[p] = append([]byte(nil), body...)
	if n, ok := f.nodes[p]; ok {
		n.Size = int64(len(body))
		f.nodes[p] = n
	}
}

// newHandleForTest allocates a handle the way the production callers do, taking
// the mount's lock. It exists so the handle tests keep working after newHandle
// was renamed to make its locking contract explicit, rather than leaving those
// tests reaching into fields directly.
func (m *Mount) newHandleForTest(path string, kind node.Kind) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.newHandleLocked(path, kind)
}

// setCacheForTest swaps the mount's read cache, so a test can size the two caps
// without rebuilding the mount. The content tests need a cache small enough that
// chunk boundaries are reachable.
func (m *Mount) setCacheForTest(c *chunkCache) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache = c
}

// addContent sets a file's bytes without recording a call, for tests that want
// content to be there before the sequence under test begins.
func (f *fakeCaller) addContent(p string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureParents(p)
	f.nodes[p] = node.Node{Path: p, Kind: node.KindFile, Size: int64(len(body)), Mode: 0o644}
	f.data[p] = append([]byte(nil), body...)
}

// fake returns the test double behind a Mount built by newTestMount.
//
// The fake is reached through the caller field rather than held in a field of
// its own. A `fake` field on Mount would have to be declared in a production
// file and typed `any`, since production code cannot name a _test.go type, and
// that is a test hook in production for a test's convenience. The caller field
// already holds the fake, and the core calls the fake through Caller exactly as
// it calls the viewer, so asserting it back is the honest route.
func fakeOf(t *testing.T, m *Mount) *fakeCaller {
	t.Helper()
	f, ok := m.caller.(*fakeCaller)
	if !ok {
		t.Fatalf("the mount's caller is %T, not the test double", m.caller)
	}
	return f
}

func (f *fakeCaller) List(path string) ([]node.Node, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "List "+path)
	defer f.mu.Unlock()
	// An injected failure is checked before the shape checks below, so a test can
	// prove the mount maps a caller error rather than discarding it. It is
	// restricted to one path so that resolving the path being operated on does
	// not itself fail: an error on every path is raised inside lookup(), and the
	// operation returns before the code under test runs.
	if f.listErr != nil && (f.listErrPath == "" || f.listErrPath == path) {
		return nil, f.listErr
	}
	// The answer here mirrors index.Children, and it has to: lookup() derives
	// its whole ENOENT-versus-ENOTDIR distinction from this call's error, so a
	// fake that answered differently would hide a real bug or, worse, make a
	// test demand behaviour the real stack cannot produce.
	//
	// The semantics are narrower than POSIX and were measured, not assumed:
	// Children reports ErrNotDir only when the queried path is itself a file,
	// and ErrNotFound otherwise. In particular "Documents/a.txt/x" is
	// ErrNotFound even though a.txt is a file, because the index answers "is
	// there a node at this path, and is that node a directory" and does not
	// walk the path's ancestors. That is the frozen v1 core's behaviour
	// (internal/index/index.go) and a mount cannot change it, so a deep
	// "file in the middle of the path" reports ENOENT rather than ENOTDIR. The
	// cases are pinned in TestFakeListMirrorsTheIndex below.
	if path == "/" {
		return f.childrenLocked("/"), nil
	}
	n, exists := f.nodes[path]
	if !exists {
		return nil, errNotFound
	}
	if n.Kind != node.KindDir {
		return nil, errNotDir
	}
	return f.childrenLocked(path), nil
}

// childrenLocked returns the direct children of a directory. The caller holds
// the mutex.
func (f *fakeCaller) childrenLocked(dir string) []node.Node {
	var out []node.Node
	for _, n := range f.nodes {
		parent, _, err := SplitParent(n.Path)
		if err != nil {
			continue
		}
		if parent == dir {
			out = append(out, n)
		}
	}
	return out
}

func (f *fakeCaller) Read(path string, off int64, n int) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "Read "+path)
	// The injected failure is checked and cleared under the same lock as the call
	// log, so a test that sets it cannot race the read that observes it. Clearing
	// would be wrong for a retry test, so it is not cleared: the same chunk fails
	// every time, which is what a genuinely broken link does.
	failWith := f.readErrSentinel(off)
	f.mu.Unlock()
	if failWith != nil {
		return nil, failWith
	}
	if _, ok := f.nodes[path]; !ok {
		return nil, errNotFound
	}
	// The offset must be a chunk boundary and the request must not span more than
	// one chunk, because that is the contract the mount's read loop relies on: it
	// has a fixed chunkSize and translates between the caller's offset and a
	// chunk index. A fake that served arbitrary ranges would accept an
	// accidentally unaligned request and answer it, hiding exactly the arithmetic
	// error this contract is meant to catch.
	if off < 0 {
		return nil, errStr("mount: negative offset")
	}
	if off%chunkSize != 0 {
		return nil, errStr("fake: read offset is not a chunk boundary")
	}
	if n > chunkSize {
		return nil, errStr("fake: read spans more than one chunk")
	}
	body := f.data[path]
	if off >= int64(len(body)) {
		// Matches viewer.Viewer: a read at or past the end is empty, not an
		// error.
		return nil, nil
	}
	rest := body[off:]
	if n >= 0 && n < len(rest) {
		rest = rest[:n]
	}
	return append([]byte(nil), rest...), nil
}

func (f *fakeCaller) Put(path string, r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Put "+path)
	f.data[path] = body
	f.nodes[path] = node.Node{Path: path, Kind: node.KindFile, Size: int64(len(body)), Mode: 0o644}
	return nil
}

func (f *fakeCaller) Mkdir(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Mkdir "+path)
	if _, ok := f.nodes[path]; ok {
		// The owner refuses this, so the fake has to as well: a fake that
		// allowed it would let a test assert a success the real thing cannot
		// produce.
		return errNotEmpty
	}
	f.nodes[path] = node.Node{Path: path, Kind: node.KindDir, Mode: 0o755}
	return nil
}

func (f *fakeCaller) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Remove "+path)
	if _, ok := f.nodes[path]; !ok {
		return errNotFound
	}
	// A directory with children is refused, as the owner refuses it. The
	// children are matched by parent so that removing a directory does not
	// leave orphans in the map, which would make a later list show files whose
	// directory is gone.
	for _, n := range f.nodes {
		if parent, _, err := SplitParent(n.Path); err == nil && parent == path {
			return errNotEmpty
		}
	}
	delete(f.nodes, path)
	delete(f.data, path)
	return nil
}

func (f *fakeCaller) Rename(oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "Rename "+oldPath+" "+newPath)
	if _, ok := f.nodes[oldPath]; !ok {
		return errNotFound
	}
	if _, ok := f.nodes[newPath]; ok {
		return errNotEmpty
	}
	if n, ok := f.nodes[oldPath]; ok {
		delete(f.nodes, oldPath)
		n.Path = newPath
		f.nodes[newPath] = n
	}
	if b, ok := f.data[oldPath]; ok {
		delete(f.data, oldPath)
		f.data[newPath] = b
	}
	return nil
}

// scriptedWatch makes Watch's behaviour controllable, so the reconnect tests
// can close the channel and assert on what the core does about it.
func (f *fakeCaller) Watch() <-chan []node.Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchCh == nil {
		f.watchCh = make(chan []node.Node, 16)
	}
	return f.watchCh
}

// emit pushes a change onto the watch channel without blocking for ever, which
// mirrors the viewer's own non-blocking emit.
func (f *fakeCaller) emit(nodes ...node.Node) bool {
	f.mu.Lock()
	ch := f.watchCh
	f.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- nodes:
		return true
	default:
		return false
	}
}

// dropWatch closes the watch channel, which is how the real viewer reports that
// the connection went away.
func (f *fakeCaller) dropWatch() {
	f.mu.Lock()
	ch := f.watchCh
	f.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (f *fakeCaller) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCaller) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// lastCall returns the most recent call, or "" if there were none.
func (f *fakeCaller) lastCall() string {
	log := f.callLog()
	if len(log) == 0 {
		return ""
	}
	return log[len(log)-1]
}

// TestFakeListMirrorsTheIndex pins the fake's error semantics against the real
// index's, and it exists because getting them wrong is silent. The first
// version of this fake answered "no" to everything that was not in its map,
// which made lookup() report ENOTDIR for a path whose parent simply did not
// exist; the second answered like POSIX and made a file deep in the path report
// ENOTDIR. Both were wrong, and both made lookup() look wrong when it was not.
//
// The semantics being pinned, measured against internal/index/index.go:
//
//   - a directory that exists lists its children;
//   - a path that is a file is ErrNotDir;
//   - a path with no node is ErrNotFound;
//   - a path under a file is ErrNotFound, NOT ErrNotDir, because the index
//     inspects only the node at the path and does not walk its ancestors.
//
// The last one is the case that is easy to get wrong and that a POSIX intuition
// gets wrong. It is asserted here rather than only described because a change
// to the fake that "fixed" it toward POSIX would start demanding behaviour from
// lookup() that the real stack cannot produce.
func TestFakeListMirrorsTheIndex(t *testing.T) {
	f := newFakeCaller()
	f.addDir("/")
	f.addFile("/a/b/c/d.txt", 4)

	cases := []struct {
		path string
		want error
	}{
		{"/", nil},
		{"/a", nil},
		{"/a/b/c", nil},
		{"/a/b/c/d.txt", errNotDir},
		{"/a/b/c/d.txt/x", errNotFound},   // under a file: NOT errNotDir
		{"/a/b/c/d.txt/x/y", errNotFound}, // deeper still
		{"/nope", errNotFound},
		{"/nope/deeper", errNotFound},
	}
	for _, c := range cases {
		_, err := f.List(c.path)
		if !errors.Is(err, c.want) {
			t.Errorf("fake List(%q) error = %v, want %v", c.path, err, c.want)
		}
	}
}

// newTestMount builds a Mount over a fresh fake, with the root present.
//
// It goes through New rather than filling the struct directly, so the tests get
// exactly what production gets: an initialised handle table and a real cache.
// A hand-built Mount would leave cache nil, and closeHandle would then panic on
// a nil pointer -- a test-only failure that says nothing about production.
func newTestMount(t *testing.T) *Mount {
	t.Helper()
	f := newFakeCaller()
	f.addDir("/")
	return New(f, DefaultOptions())
}
