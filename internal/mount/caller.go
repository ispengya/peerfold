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
	"sync"
	"time"

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
// entries, the open-handle table, and the cache of what the kernel has been
// told, and it holds the one Caller it was built with.
//
// The fields are grouped by the task that introduced them, and later tasks
// extend the struct rather than replacing it: the watch and reconnect state
// (watch.go, reconnect.go).
//
// Its methods return an errno the way cgofuse wants it (0 or a negative value),
// not a Go error, because that is the shape of the interface on both platforms
// and converting once at the boundary is cheaper than converting at every call
// site.
type Mount struct {
	// caller is the only way this package reaches the federation. It is an
	// interface so the tests can drive the core without a wire.
	caller Caller

	// opts holds the tunables. Everything with a default is filled in by
	// DefaultOptions, so a zero Mount is not a valid one.
	opts Options

	// mu guards nextFh and files.
	//
	// cgofuse calls the filesystem's methods from several threads at once, so
	// Lookup on one path and Release on another arrive concurrently and both
	// touch the handle table. Every exported method takes this lock.
	//
	// Lock order, when both locks are wanted: Mount.mu first, then the cache's
	// own mutex. A method holding Mount.mu calls only the cache's *Locked
	// helpers; a method that wants only the cache takes just that lock and
	// never Mount.mu. Nothing takes them in the other order.
	mu sync.Mutex

	// fh numbers are never reused within a run: the kernel can hold a stale
	// number after a release, and handing the same number to a different file
	// would let that stale reference touch the wrong path.
	nextFh uint64
	// files is the open-handle table, keyed by fh.
	files map[uint64]*openFile
	// cache holds read chunks under the two D3 caps.
	cache *chunkCache
}

// New builds the operation core over a caller, which is the layer that actually
// reaches the owner.
func New(caller Caller, opts Options) *Mount {
	return &Mount{
		caller: caller,
		opts:   opts,
		files:  map[uint64]*openFile{},
		cache:  newChunkCache(opts.BufferLimit, opts.BufferTotal),
	}
}

// Options carries the tunables of a mount. It is a struct rather than a pile of
// arguments because the runner fills it from flags, the tests fill it from
// constants, and both need every field to have a defined default.
type Options struct {
	// BufferLimit is the read cache for a single open file, and BufferTotal is
	// the read cache for all open files together. The two are the D3 pair and
	// they bound different things: one alone is not a bound on the process,
	// because N handles each respecting a per-handle cap hold N times it.
	BufferLimit int
	BufferTotal int
	// WriteLimit is how many bytes of accumulated writing one open may hold.
	// Past it a write is refused with ENOSPC rather than truncated.
	WriteLimit int
	// AttrTimeout and EntryTimeout are how long the kernel may trust an
	// attribute or a directory entry it was handed. Both are short because the
	// index can change without this process knowing: a file the kernel believes
	// is 10 bytes may have been replaced by the owner.
	AttrTimeout  time.Duration
	EntryTimeout time.Duration
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
//
// Each number is a decision rather than a placeholder:
//
// 64 MiB per open and 256 MiB across all opens: large enough that a sequential
// reader of a big file is not re-fetching, small enough that the process cannot
// be made to exhaust memory by an ordinary workload.
//
// 8 MiB of accumulated writes per open: a file smaller than this is written
// back in one Put at release, which is the common case. A larger file is refused
// with ENOSPC rather than silently truncated, so the failure is visible.
//
// 1 second for both timeouts: long enough that a walk of a directory does not
// re-ask for every attribute, short enough that a change made elsewhere is seen
// in about a second without this process having to be told.
func DefaultOptions() Options {
	return Options{
		BufferLimit:  64 << 20,
		BufferTotal:  256 << 20,
		WriteLimit:   8 << 20,
		AttrTimeout:  time.Second,
		EntryTimeout: time.Second,
		RootMode:     0o755,
	}
}
