package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/viewer"
)

// dialViewer connects a viewer to addr.
func dialViewer(t *testing.T, addr string) *viewer.Viewer {
	t.Helper()
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return v
}

// onDisk reads a file on the supplying machine. Every write test asserts against
// this rather than against the index, because the index is the server's opinion
// and the whole point of a write is that the owner's disk changed.
func onDisk(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", rel, err)
	}
	return string(b)
}

// TestE2EPutLandsOnTheOwnersDisk is the core write assertion: a file written
// through the aggregated view exists on the machine that shares the directory.
func TestE2EPutLandsOnTheOwnersDisk(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Put("win/Documents/new.txt", strings.NewReader("from the viewer")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := onDisk(t, root, "new.txt"); got != "from the viewer" {
		t.Fatalf("new.txt = %q, want %q", got, "from the viewer")
	}
}

// TestE2EPutOverwritesAnExistingFile pins the replace case, including that a
// shorter write leaves no tail of the longer file behind.
func TestE2EPutOverwritesAnExistingFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": strings.Repeat("x", 400)})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Put("win/Documents/a.txt", strings.NewReader("short")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := onDisk(t, root, "a.txt"); got != "short" {
		t.Fatalf("a.txt = %q (%d bytes), want %q with no leftover tail", got, len(got), "short")
	}
}

// TestE2EPutAZeroByteFile pins that writing nothing is a real file of zero
// bytes, which is a different outcome from a write that did not happen.
func TestE2EPutAZeroByteFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Put("win/Documents/empty.bin", strings.NewReader("")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "empty.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("Size = %d, want 0", info.Size())
	}
}

// TestE2EPutAFrameCrossingFile pins that a file larger than one frame's payload
// survives the trip whole.
//
// One frame is the unit the protocol can carry, so a file that crosses it is the
// first case where the chunking is exercised at all.
func TestE2EPutAFrameCrossingFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	// Comfortably past one frame, with a pattern that shows a shift or a
	// repeated chunk rather than just a wrong length.
	want := make([]byte, 3_000_000)
	for i := range want {
		want[i] = byte(i * 7)
	}
	if err := v.Put("win/Documents/big.bin", strings.NewReader(string(want))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "big.bin"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("wrote %d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d: the content was corrupted in transit", i, got[i], want[i])
		}
	}
}

// TestE2EPutExclusiveRefusesAnExistingFile pins that a create does not clobber
// what is already there.
func TestE2EPutExclusiveRefusesAnExistingFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "original"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	err := v.PutExclusive("win/Documents/a.txt", strings.NewReader("replacement"))
	if err == nil {
		t.Fatal("PutExclusive over an existing file = nil, want a failure")
	}
	if got := onDisk(t, root, "a.txt"); got != "original" {
		t.Fatalf("a.txt = %q, want the original %q left alone", got, "original")
	}
}

// TestE2EPutExclusiveCreatesANewFile pins that the exclusive form still writes
// when the name is free.
func TestE2EPutExclusiveCreatesANewFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.PutExclusive("win/Documents/fresh.txt", strings.NewReader("mine")); err != nil {
		t.Fatalf("PutExclusive: %v", err)
	}
	if got := onDisk(t, root, "fresh.txt"); got != "mine" {
		t.Fatalf("fresh.txt = %q, want %q", got, "mine")
	}
}

// TestE2EMkdirLandsOnTheOwnersDisk pins directory creation.
func TestE2EMkdirLandsOnTheOwnersDisk(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Mkdir("win/Documents/sub2"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "sub2"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("IsDir = false, want a directory")
	}
}

// TestE2EMkdirTwiceFails pins that the second call reports the name is taken.
func TestE2EMkdirTwiceFails(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Mkdir("win/Documents/once"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := v.Mkdir("win/Documents/once"); err == nil {
		t.Fatal("the second Mkdir = nil, want a failure")
	}
}

// TestE2ERenameMovesTheFileOnTheOwnersDisk pins the whole rename: the old name
// is gone and the new one holds the same bytes.
func TestE2ERenameMovesTheFileOnTheOwnersDisk(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Rename("win/Documents/a.txt", "win/Documents/renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old name still exists (err = %v)", err)
	}
	if got := onDisk(t, root, "renamed.txt"); got != "aaaa" {
		t.Fatalf("renamed.txt = %q, want %q", got, "aaaa")
	}
}

// TestE2ERemoveDeletesTheFileOnTheOwnersDisk pins deletion.
func TestE2ERemoveDeletesTheFileOnTheOwnersDisk(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"zero.bin": ""})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Remove("win/Documents/zero.bin"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "zero.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file still exists (err = %v)", err)
	}
}

// TestE2ERemoveRefusesANonEmptyDirectory pins that a remove is not a recursive
// delete, which is the difference between one wrong path and a destroyed tree.
func TestE2ERemoveRefusesANonEmptyDirectory(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"sub/b.txt": "bb"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Remove("win/Documents/sub"); err == nil {
		t.Fatal("Remove of a non-empty directory = nil, want a failure")
	}
	if got := onDisk(t, root, "sub/b.txt"); got != "bb" {
		t.Fatalf("the child was deleted: content = %q, want %q", got, "bb")
	}
}

// TestE2ERenameAcrossClientsIsRefused pins that a cross-client rename is an error
// naming the condition, not a copy dressed up as a rename.
//
// The refusal is the requester's own, made before any frame is sent, so it keeps
// its identity: errors.Is works here where it cannot for a reason that came back
// from an owner. That distinction is the point of viewer.ErrCrossClient, and it
// is this test's job to hold it — the reason the value exists is that the next
// plan's mount layer has to map it to the filesystem's own cross-device error,
// which is impossible from a string.
func TestE2ERenameAcrossClientsIsRefused(t *testing.T) {
	_, addr := startServer(t)
	winRoot := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	linuxRoot := share(t, "Documents", map[string]string{"c.txt": "cccc"})
	join(t, addr, "win", "Documents="+winRoot)
	join(t, addr, "linux", "Documents="+linuxRoot)
	v := dialViewer(t, addr)

	err := v.Rename("win/Documents/a.txt", "linux/Documents/a.txt")
	if err == nil {
		t.Fatal("Rename across clients = nil, want a refusal")
	}
	if !errors.Is(err, viewer.ErrCrossClient) {
		t.Fatalf("Rename across clients = %v, want it to be viewer.ErrCrossClient", err)
	}
	// The message still has to say what happened: the sentinel is for callers,
	// and a user reads the text.
	if !strings.Contains(err.Error(), "win") || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("Rename across clients = %v, want it to name both clients", err)
	}
	// Neither machine may have changed: a refusal must not leave a copy behind
	// on the far side, and it must not move the original either.
	if got := onDisk(t, winRoot, "a.txt"); got != "aaaa" {
		t.Fatalf("win/a.txt = %q, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(linuxRoot, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a copy appeared on the other client (err = %v)", err)
	}
}

// TestE2ERenameIntoAnotherShareIsRefused pins the rule one level down: a rename
// stays inside one share, since a share is a directory and no two of them are
// the same directory.
func TestE2ERenameIntoAnotherShareIsRefused(t *testing.T) {
	_, addr := startServer(t)
	docsRoot := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	photosRoot := share(t, "Photos", map[string]string{"p.jpg": "jpg"})
	join(t, addr, "win", "Documents="+docsRoot, "Photos="+photosRoot)
	v := dialViewer(t, addr)

	err := v.Rename("win/Documents/a.txt", "win/Photos/a.txt")
	if err == nil {
		t.Fatal("Rename into another share = nil, want a refusal")
	}
	if got := onDisk(t, docsRoot, "a.txt"); got != "aaaa" {
		t.Fatalf("the original moved: Documents/a.txt = %q, want %q", got, "aaaa")
	}
	if _, err := os.Stat(filepath.Join(photosRoot, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a copy appeared in the other share (err = %v)", err)
	}
}

// TestE2EPutIntoAMissingDirectoryFails pins that a mistyped parent is reported
// rather than invented.
func TestE2EPutIntoAMissingDirectoryFails(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	err := v.Put("win/Documents/nope/x.txt", strings.NewReader("data"))
	if err == nil {
		t.Fatal("Put into a missing directory = nil, want a failure")
	}
	if _, err := os.Stat(filepath.Join(root, "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the missing directory was created (err = %v)", err)
	}
}

// TestE2EWritesRejectUnknownClientsAndPaths pins the refusals that never reach a
// machine.
func TestE2EWritesRejectUnknownClientsAndPaths(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	cases := []struct {
		what string
		do   func() error
	}{
		{"put to an unknown client", func() error { return v.Put("nobody/Documents/x.txt", strings.NewReader("x")) }},
		{"mkdir on an unknown client", func() error { return v.Mkdir("nobody/Documents/d") }},
		{"remove on an unknown client", func() error { return v.Remove("nobody/Documents/a.txt") }},
		{"put to a path outside any share", func() error { return v.Put("win/Other/x.txt", strings.NewReader("x")) }},
		{"put with no path", func() error { return v.Put("win", strings.NewReader("x")) }},
		{"mkdir with no path", func() error { return v.Mkdir("win") }},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			if err := tc.do(); err == nil {
				t.Fatalf("%s = nil, want a failure", tc.what)
			}
		})
	}
}

// TestE2EWritesRefuseAPathThatEscapesTheShare pins that the escape check is
// applied to writes at the far end and not just to reads.
//
// A read that escaped would disclose a file; a write that escaped would modify
// one, and the check has to survive the trip through the server even though the
// server never looks inside the path itself.
func TestE2EWritesRefuseAPathThatEscapesTheShare(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	outside := filepath.Join(filepath.Dir(root), "escaped.txt")
	if err := v.Put("win/Documents/../escaped.txt", strings.NewReader("owned")); err == nil {
		t.Fatal("Put through an escaping path = nil, want a refusal")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a write escaped the share root (err = %v)", err)
	}
}

// TestE2ERefusedWritesLeaveTheClientAliveAndInTheTree pins the blast radius of a
// refusal.
//
// A refusal that does not consume the content is not a local failure. The data
// frames stay queued in front of the client's answer, so the client's read loop
// reads a data frame where it expects a control frame, exits, and the server
// withdraws every node that client published — for a mistyped path, a name that
// already exists, or a path that escapes the share. The three cases below are
// ordinary user errors, and each is followed by a request that has to still work.
func TestE2ERefusedWritesLeaveTheClientAliveAndInTheTree(t *testing.T) {
	_, addr := startServer(t)
	docsRoot := share(t, "Documents", map[string]string{"keep.txt": "keep"})
	join(t, addr, "win", "Documents="+docsRoot)
	v := dialViewer(t, addr)

	cases := []struct {
		what string
		do   func() error
	}{
		{"an unknown share", func() error {
			return v.Put("win/NoSuchShare/file.txt", strings.NewReader("data"))
		}},
		{"a path that escapes the share", func() error {
			return v.Put("win/Documents/../escaped.txt", strings.NewReader("data"))
		}},
		{"--exclusive on a name that is taken", func() error {
			return v.PutExclusive("win/Documents/keep.txt", strings.NewReader("data"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			if err := tc.do(); err == nil {
				t.Fatalf("Put with %s = nil, want a refusal", tc.what)
			}
			// The client is still connected and still in the tree: the refusal
			// was about one request, not about this client existing.
			if _, err := v.List("win/Documents"); err != nil {
				t.Fatalf("List after a refused put (%s): %v: the refusal cost the client its connection", tc.what, err)
			}
			if _, err := v.List(""); err != nil {
				t.Fatalf("List(\"\") after a refused put (%s): %v", tc.what, err)
			}
			// And a write that should work still does, on the same connection.
			if err := v.Put("win/Documents/after.txt", strings.NewReader("after")); err != nil {
				t.Fatalf("Put after a refused put (%s): %v: the connection is out of step", tc.what, err)
			}
			if got := onDisk(t, docsRoot, "after.txt"); got != "after" {
				t.Fatalf("after.txt = %q, want %q", got, "after")
			}
		})
	}

	// The file that --exclusive refused must be untouched.
	if got := onDisk(t, docsRoot, "keep.txt"); got != "keep" {
		t.Fatalf("keep.txt = %q, want it untouched by the refused exclusive put", got)
	}
}

// TestE2EWritesKeepTheConnectionInStep pins that a write answer is read whole.
//
// Every write is answered with a control frame, a stream and a closing frame. A
// version that stopped after the control frame would leave its stream and tail
// to be read as the next answer, so the next request would be answered with the
// previous one's leftover. Four writes in a row on one viewer is what makes that
// visible.
func TestE2EWritesKeepTheConnectionInStep(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Put("win/Documents/one.txt", strings.NewReader("one")); err != nil {
		t.Fatalf("Put one: %v", err)
	}
	if err := v.Mkdir("win/Documents/two"); err != nil {
		t.Fatalf("Mkdir two: %v", err)
	}
	if err := v.Rename("win/Documents/one.txt", "win/Documents/three.txt"); err != nil {
		t.Fatalf("Rename three: %v", err)
	}
	if err := v.Remove("win/Documents/three.txt"); err != nil {
		t.Fatalf("Remove three: %v", err)
	}
	// And a read after a write, since the two share the connection and a
	// leftover frame from either would be read as the other's.
	got, err := v.Cat("win/Documents/a.txt")
	if err != nil {
		t.Fatalf("Cat after the writes: %v", err)
	}
	if string(got) != "aaaa" {
		t.Fatalf("Cat = %q, want %q", got, "aaaa")
	}
	if got := onDisk(t, root, "a.txt"); got != "aaaa" {
		t.Fatalf("a.txt = %q, want it untouched by the writes around it", got)
	}
}

// TestE2EPutFailsWhenTheReaderDies pins what happens when the content stops
// half-way: the call fails, and no partial file is left at the target where
// every later reader would take it for the whole file.
//
// The reader here dies on the *requester's* side, which is the case the end
// marker cannot describe: the bytes that did arrive are a well-formed stream of
// three bytes, so the owner would rename the prefix over the target unless the
// server tells it the content was cut short. That notice is what this pins, and
// it is why the second call is here rather than only the stat: the put returns
// as soon as the local reader fails, while the owner is still receiving, so
// asserting on the file straight afterwards would be asserting on a race — the
// old version of this test passed by checking before the write landed, and kept
// passing when the write published a truncated file.
//
// The follow-up call is what makes it deterministic: the viewer holds one
// connection, so a round trip to the server can only complete after the server
// has finished with the put, which means after the owner has answered it.
func TestE2EPutFailsWhenTheReaderDies(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	err := v.Put("win/Documents/partial.bin", &dyingReader{failAfter: 3})
	if err == nil {
		t.Fatal("Put with a dying reader = nil, want a failure")
	}
	if _, err := v.List(""); err != nil {
		t.Fatalf("List after a failed put: %v: the connection is out of step", err)
	}
	if _, err := os.Stat(filepath.Join(root, "partial.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a partial file was left at the target (err = %v)", err)
	}
}

// TestE2EPutFailsWhenTheReaderDiesOverAnExistingFile is the case that loses data
// rather than merely leaving a stray file: the target already holds a whole
// file, and a put whose content stops part-way must not replace it with the
// prefix that arrived.
//
// A prefix is the dangerous shape because it is indistinguishable from a whole
// file to every later reader — there is no size to check against and no
// trailing byte that says otherwise. The old behaviour truncated the target to
// the three bytes that arrived, or to zero bytes when the reader died before
// the first chunk.
func TestE2EPutFailsWhenTheReaderDiesOverAnExistingFile(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"keep.txt": "the original contents"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	if err := v.Put("win/Documents/keep.txt", &dyingReader{failAfter: 3}); err == nil {
		t.Fatal("Put with a dying reader = nil, want a failure")
	}
	if _, err := v.List(""); err != nil {
		t.Fatalf("List after a failed put: %v: the connection is out of step", err)
	}
	if got := onDisk(t, root, "keep.txt"); got != "the original contents" {
		t.Fatalf("keep.txt = %q, want %q: a truncated put overwrote it", got, "the original contents")
	}
}

// TestE2EPutFailsWhenTheOwnerLeavesMidWrite pins the other side of a broken
// transfer: the machine that owns the path goes away while its content is still
// arriving.
//
// What is pinned is what is actually guaranteed — the put fails, and nothing is
// left at the target for a later reader to mistake for a whole file. The reason
// is not pinned, and the test says why rather than asserting something that only
// usually holds: when the owner disappears mid-transfer the two ends race, and
// the viewer may fail to write to the server's socket before the server has
// finished failing to write to the owner's. Both are real failures of the same
// operation, and which one is reported first depends on the size of a socket
// buffer. The offline reason itself is pinned deterministically by
// TestE2EPutAfterTheOwnerLeaves, where nothing is in flight.
func TestE2EPutFailsWhenTheOwnerLeavesMidWrite(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	owner := join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	// A reader that closes the owner's connection when the transfer begins and
	// keeps producing, so the content really is in flight when the server finds
	// out the owner is gone.
	r := &cutOffReader{
		onChunk: func(int) {
			_ = owner.Close()
		},
	}
	err := v.Put("win/Documents/cut.bin", r)
	if err == nil {
		t.Fatal("Put across a leaving owner = nil, want a failure")
	}
	if _, err := os.Stat(filepath.Join(root, "cut.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a partial file was left at the target (err = %v): a half-written file is worse than none", err)
	}
	if got := onDisk(t, root, "a.txt"); got != "aaaa" {
		t.Fatalf("a.txt = %q, want it untouched by the failed write", got)
	}
}

// TestE2EPutAfterTheOwnerLeaves pins the offline reason deterministically: a
// write to a client that is not connected, with nothing in flight to race
// against, fails as ErrClientOffline.
func TestE2EPutAfterTheOwnerLeaves(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	owner := join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	// One read first, so the connection is known to be good before it is cut.
	if _, err := v.Cat("win/Documents/a.txt"); err != nil {
		t.Fatalf("Cat before the owner leaves: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Wait for the server to notice, so this tests the offline path rather than
	// a race with the disconnect.
	waitForServerToForget(t, addr, "win")

	err := v.Put("win/Documents/after.txt", strings.NewReader("data"))
	if err == nil {
		t.Fatal("Put to a client that has left = nil, want a failure")
	}
	// The reason crosses the wire as a string, so the error's identity does not
	// survive it and the assertion is on the wording ErrClientOffline produces.
	// What matters either way is that the write is refused rather than queued or
	// silently dropped.
	if !strings.Contains(err.Error(), "client is not connected") {
		t.Fatalf("Put = %v, want it to say the client is not connected", err)
	}
	if _, err := os.Stat(filepath.Join(root, "after.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was created by a write to an absent owner (err = %v)", err)
	}
	// The connection has to be in step afterwards, and this is the assertion
	// that caught it not being: the put sent its content, the server refused
	// before forwarding any of it, and nothing read those frames off the
	// connection, so this next request was answered with a stale data frame
	// ("expected a control frame, got frame kind 2") or, when the mismatch was
	// noticed at close, with a transport error.
	if _, err := v.List(""); err != nil {
		t.Fatalf("List after a refused put: %v: the connection is out of step", err)
	}
}

// waitForServerToForget blocks until a second viewer can no longer see the named
// client, which is what the server's registry looks like once it has processed
// the disconnect.
func waitForServerToForget(t *testing.T, addr, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		probe, err := viewer.Dial(addr)
		if err != nil {
			t.Fatalf("probe viewer: %v", err)
		}
		nodes, err := probe.List("")
		probe.Close()
		if err == nil {
			gone := true
			for _, n := range nodes {
				if n.Path == name {
					gone = false
				}
			}
			if gone {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the server still lists %q after its client left", name)
}

// dyingReader yields failAfter bytes and then fails.
type dyingReader struct {
	failAfter int
	sent      int
}

func (r *dyingReader) Read(p []byte) (int, error) {
	if r.sent >= r.failAfter {
		return 0, errors.New("reader failed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'z'
	r.sent++
	return 1, nil
}

// cutOffReader produces data for ever and calls onChunk once. It stands for a
// transfer that is under way when the far side disappears.
type cutOffReader struct {
	onChunk func(int)
	chunk   int
}

func (r *cutOffReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.chunk++
	if r.onChunk != nil && r.chunk == 1 {
		cb := r.onChunk
		r.onChunk = nil
		cb(r.chunk)
	}
	for i := range p {
		p[i] = 'q'
	}
	return len(p), nil
}

// TestE2EPutAndCatAgree pins the round trip: what a put writes is what a cat
// reads back through the same aggregated view.
func TestE2EPutAndCatAgree(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	content := "round trip\nwith two lines\n"
	if err := v.Put("win/Documents/rt.txt", strings.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := v.Cat("win/Documents/rt.txt")
	if err != nil {
		t.Fatalf("Cat: %v", err)
	}
	if string(got) != content {
		t.Fatalf("Cat = %q, want %q", got, content)
	}
}

// TestE2EWritesFromTwoViewers pins that two callers writing different files do
// not interfere, since each has its own connection and the server routes each
// request to the owner independently.
func TestE2EWritesFromTwoViewers(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	join(t, addr, "win", "Documents="+root)
	v1 := dialViewer(t, addr)
	v2 := dialViewer(t, addr)

	done := make(chan error, 2)
	go func() { done <- v1.Put("win/Documents/one.txt", strings.NewReader("one")) }()
	go func() { done <- v2.Put("win/Documents/two.txt", strings.NewReader("two")) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent Put: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("a concurrent Put did not finish")
		}
	}

	if got := onDisk(t, root, "one.txt"); got != "one" {
		t.Fatalf("one.txt = %q, want %q", got, "one")
	}
	if got := onDisk(t, root, "two.txt"); got != "two" {
		t.Fatalf("two.txt = %q, want %q", got, "two")
	}
}
