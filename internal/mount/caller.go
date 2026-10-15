// Package mount presents a peerfold federation as an ordinary directory tree
// through a real kernel mount point.
//
// The package is split in two. This file and ops.go, path.go, errno.go,
// budget.go, handle.go, watch.go and reconnect.go are the portable operation
// core: they do not import a FUSE library, so they build and are tested on any
// platform. host.go is the adapter that presents the core to cgofuse, and the
// runner_*.go files carry the only genuinely platform-specific behaviour.
package mount

import (
	"io"

	"peerfold/internal/node"
)

// Caller is everything the operation core needs from a peerfold client. It is
// satisfied by *viewer.Viewer through viewerCaller (host.go); the tests use a
// fake instead, which is the point: the core has no dependency on the wire.
//
// Every method is called from the core's single goroutine, so an
// implementation does not need to be safe for concurrent use. That matches
// viewer.stream(), which allows exactly one request in flight.
//
// The signatures are deliberately the viewer's own, with one deliberate
// exception: Watch returns a channel of node slices. The core needs to know
// when the tree changed so it can invalidate what the kernel has cached, and
// the viewer already delivers exactly that. A Caller that had no watch would
// force the core to poll, and a polling core is a core that serves stale
// attributes for as long as its interval.
type Caller interface {
	// List returns the immediate children of a directory.
	List(path string) ([]node.Node, error)
	// Read returns up to n bytes at off. n < 0 means "to the end of the
	// file", which is viewer.Viewer's convention and is what the core passes.
	// A read at or past the end returns no bytes and no error.
	Read(path string, off int64, n int) ([]byte, error)
	// Put writes a whole file. It replaces the target atomically.
	Put(path string, r io.Reader) error
	// Mkdir creates one directory. It does not create parents.
	Mkdir(path string) error
	// Remove deletes one file or one empty directory.
	Remove(path string) error
	// Rename moves a path within the federation.
	Rename(oldPath, newPath string) error
	// Watch returns a channel that is closed when the underlying connection
	// goes away. The core treats closure as "reconnect or give up", never as
	// "the tree is now empty".
	Watch() <-chan []node.Node
}

// errRoot is returned for requests about the mount root, which has no parent
// and cannot be renamed, removed or created.
//
// It is a string type rather than errors.New because it is compared by value in
// a couple of places and never needs to be wrapped; the alternative would be
// another exported sentinel for a condition only this package can produce.
var errRoot = errStr("mount: the root has no parent")

type errStr string

func (e errStr) Error() string { return string(e) }

// Mount is the operation core. It owns the mapping from FUSE paths to index
// entries and the cache of what the kernel has been told, and it holds the one
// Caller it was built with.
//
// The fields are grouped by the task that introduced them, and later tasks
// extend the struct rather than replacing it: the open-file table (handle.go),
// the buffer budget (budget.go), the watch and reconnect state (watch.go,
// reconnect.go).
type Mount struct {
	// caller is the only way this package reaches the federation. It is an
	// interface so the tests can drive the core without a wire.
	caller Caller

	// opts holds the tunables. Everything with a default is filled in by
	// DefaultOptions, so a zero Mount is not a valid one.
	opts Options
}

// Options carries the tunables of a mount. It is a struct rather than a pile of
// arguments because the runner fills it from flags, the tests fill it from
// constants, and both need every field to have a defined default.
//
// Task 5 extends this with the buffer caps; the fields present here are the
// ones path resolution needs, which is why the struct exists at all — without
// it there is no Mount to hang a lookup on.
type Options struct {
	// RootMode is the permission bits reported for the mount root itself.
	//
	// The root is synthesised rather than listed: no client owns it, so there
	// is no index entry to read a mode from. It is a directory with the
	// conventional mode so that ordinary traversal works, and it is a field
	// rather than a constant because a test that wants to check the mode is
	// honoured needs to change it.
	RootMode uint32
}

// DefaultOptions returns the options a mount uses when the caller sets nothing.
//
// It exists so that "the default" has exactly one definition. Every field is
// set explicitly here, including the ones whose zero value would happen to
// work, because the point of a defaults function is that a reader can see what
// all the defaults are without knowing which zero values are meaningful.
func DefaultOptions() Options {
	return Options{
		RootMode: 0o755,
	}
}
