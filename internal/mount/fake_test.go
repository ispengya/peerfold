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
	f.mu.Unlock()
	if _, ok := f.nodes[path]; !ok {
		return nil, errNotFound
	}
	body := f.data[path]
	if off < 0 {
		// A negative offset is out of range. n < 0 is the one that means "to
		// the end"; off does not carry that meaning, so it is refused.
		return nil, errStr("mount: negative offset")
	}
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
func newTestMount(t *testing.T) *Mount {
	t.Helper()
	f := newFakeCaller()
	f.addDir("/")
	return &Mount{caller: f, opts: DefaultOptions()}
}
