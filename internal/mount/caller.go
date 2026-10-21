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

	"github.com/ispengya/peerfold/internal/node"
)

// Batch is one pushed index update: a group of nodes that arrived together, and
// the sequence number the transport gave the group.
//
// The sequence number is not part of the filesystem model, which is why it is not
// a field on node.Node: a node is a file's metadata, and it travels on listings
// and reads as well as on pushes. A batch is the thing the transport numbers.
//
// Seq is zero when the sender does not number its events. Zero therefore means
// "not numbered" rather than "sequence zero", and a consumer must not read it as
// a gap (D5).
type Batch struct {
	Seq   uint64
	Nodes []node.Node
}

// Caller is everything the operation core needs from a peerfold client. It is
// satisfied by *viewer.Viewer through viewerCaller (host.go); the tests use a
// fake instead, which is the point: the core has no dependency on the wire.
//
// Every method is called from the core's single goroutine, so an
// implementation does not need to be safe for concurrent use. That matches
// viewer.stream(), which allows exactly one request in flight.
//
// The signatures are deliberately the viewer's own, with one deliberate
// exception: Watch returns a channel of batches. The core needs to know when the
// tree changed so it can invalidate what the kernel has cached, and it needs the
// sequence number so it can tell that the channel dropped something on the way
// (the viewer's emit is non-blocking by design, so a drop is invisible
// otherwise). A Caller that had no watch would force the core to poll, and a
// polling core is a core that serves stale attributes for as long as its
// interval.
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
	Watch() <-chan Batch
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
	//
	// It is replaced by a reconnect, and that replacement happens on the
	// supervisor's goroutine while cgofuse is calling this package's methods from
	// several threads of its own. Every read of it therefore goes through call(),
	// which takes the lock: reading the field directly would be a data race, and
	// it is one this machine cannot detect, because there is no -race here.
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
	// fetchMu serialises cache-miss fills. The viewer connection carries one
	// request at a time, so this costs no network concurrency; its purpose is
	// the re-check after waiting. Concurrent FUSE reads of the same range all
	// miss before the first fill completes, and without this each one goes to
	// the wire for the same bytes.
	fetchMu sync.Mutex
	// loadMu serialises content loads for a handle's first write. Two writes
	// arriving together on one clean handle must not both fetch the whole file;
	// the second waits, re-checks loaded, and then writes into the loaded
	// buffer. It is separate from fetchMu because a load is a whole-file
	// sequence of reads, not a chunk fill.
	loadMu sync.Mutex
	// listingMu guards listings, the directory cache.
	//
	// lookup() resolves a path by listing its parent, so without this every
	// Getattr and Open transferred a whole directory. The cache is separate
	// from m.mu because a listing is taken over the wire and must never be held
	// across a network call.
	listingMu sync.Mutex
	listings  map[string]dirCacheEntry
	// listingInflight holds the directory fetches currently on the wire, so
	// concurrent misses share one answer instead of each transferring the whole
	// listing. Guarded by listingMu.
	listingInflight map[string]*listingCall

	// events is what the watch channel has told this mount, keyed by full path.
	//
	// It is not a copy of the federation's index and it is not a whole-tree
	// cache: it holds only the paths that were named by an event, and every other
	// path still goes to the caller. Its one job is to keep a push that arrived
	// from being contradicted by a listing the caller answers from its own
	// (older, for that path) index -- which is what D5's detection makes
	// survivable, because a listing that disagrees with a push is otherwise
	// indistinguishable from a listing that is simply right.
	//
	// A deleted path is kept here as node.KindDeleted rather than removed, so the
	// deletion continues to shadow the listing. Without the tombstone, the entry
	// would be indistinguishable from "never heard of it" and the next listing
	// would resurrect it.
	events map[string]node.Node

	// pendingPaths records names this mount itself created but whose owning
	// client has not pushed them back yet, and what each one is. It exists for
	// the immediate post-create window -- Windows revalidates a new path through
	// the mount before the client's watcher has reported it to the server -- and
	// for the adapter's immediate post-create callbacks. Every other path still
	// resolves through the caller.
	pendingPaths map[string]node.Kind

	// pendingContent holds the last whole-file body uploaded for a pending file.
	// It is bounded by the same write limits as ordinary handles and is dropped
	// when the watcher reports the path or the path is removed. It exists so a
	// just-created name can be read back immediately after the flush, before the
	// owner's watcher has updated the index.
	pendingContent map[string][]byte

	// lastSeq is the sequence number of the last numbered batch. A jump means the
	// transport dropped events, which is what isStale records.
	lastSeq uint64
	// isStale reports that events were lost, so the tree cannot be trusted. Task
	// 11 owns clearing it, by re-watching and invalidating.
	isStale bool
	// isDisconnected reports that the watch channel ended. It is not the same as
	// stale: a closed channel is a known-unknown, while stale is a tree that is
	// wrong in an unknown way.
	isDisconnected bool
	// handed records the paths whose attributes were given to the kernel, so a
	// later invalidation has something to walk. FUSE has no whole-filesystem
	// invalidate, which is why the table exists; the cap keeps it bounded.
	handed map[string]struct{}

	// watchDone and watchExited are the running watch loop: watchDone stops it and
	// watchExited is closed as it leaves, so a reconnect can wait for the loop
	// reading the old caller to be gone before it replaces that caller.
	//
	// They are set and cleared as a pair by startWatch and stopWatch rather than
	// guarded by a sync.Once. A Once would be wrong here: a reconnect has to be
	// able to start a *second* loop, on the new caller's channel, and a Once by
	// definition never runs again. Idempotence comes from the nil check instead.
	watchDone   chan struct{}
	watchExited chan struct{}

	// onGiveUp is called when the supervisor stops retrying, to unmount. It is
	// nil until the host adapter sets it, which is why every call is guarded.
	onGiveUp func()
	// onDiscard is told the path of every dirty handle dropped at disconnect.
	//
	// It exists because a discarded edit is otherwise invisible: the write
	// reported success, the mount is still there, and the content is gone. The
	// host adapter logs it. It is called once per path.
	onDiscard func(string)
	// invalidatePath asks the host to drop the kernel's cached attributes for one
	// path. FUSE has no whole-filesystem invalidate, so a refresh walks the
	// handed-out table and calls this per path.
	//
	// It reports whether the kernel accepted the request. A refusal is not an
	// error the mount can act on -- the remaining paths still have to be
	// invalidated and the reconnect has already happened -- but a run of them is
	// worth hearing about, because the alternative is a kernel serving attributes
	// the mount knows to be wrong. See invalidateAll.
	invalidatePath func(string) bool
}

// New builds the operation core over a caller, which is the layer that actually
// reaches the owner.
func New(caller Caller, opts Options) *Mount {
	return &Mount{
		caller:          caller,
		opts:            opts,
		files:           map[uint64]*openFile{},
		cache:           newChunkCache(opts.BufferLimit, opts.BufferTotal),
		events:          map[string]node.Node{},
		listings:        map[string]dirCacheEntry{},
		listingInflight: map[string]*listingCall{},
		pendingPaths:    map[string]node.Kind{},
		pendingContent:  map[string][]byte{},
		handed:          map[string]struct{}{},
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
	//
	// The default is 16 MiB, which leaves headroom over the 10 MB files this
	// version targets: the limit counts bytes held, and a file that is opened
	// for a partial edit also holds the untouched content loaded before the
	// write. A larger file is still refused visibly rather than silently
	// truncated; the disk-spooled write path that removes the ceiling is a
	// later change.
	WriteLimit int
	// ChunkSize is how many bytes one ranged read asks the owner for. It is
	// the knob that trades first-byte latency against sequential throughput on
	// a high-RTT link; zero means the package default.
	ChunkSize int
	// AttrTimeout and EntryTimeout are how long the kernel may trust an
	// attribute or a directory entry it was handed. Events invalidate a path
	// when it actually changes, so these bound only the staleness the kernel
	// can invent on its own; five seconds keeps Explorer's repeated stats off
	// the wire without hiding a change for long.
	AttrTimeout  time.Duration
	EntryTimeout time.Duration
	// ListingTimeout is how long a directory listing is reused. It is separate
	// from EntryTimeout on purpose: the kernel's entry cache is expired by
	// Explorer's refresh loop every few seconds, while the listing behind it
	// only changes when something in the directory changes. Events invalidate
	// it immediately, so this bounds staleness only when an event is lost.
	ListingTimeout time.Duration
	// RootMode is the permission bits reported for the mount root itself.
	//
	// The root is synthesised rather than listed: no client owns it, so there
	// is no index entry to read a mode from. It is a directory with the
	// conventional mode so that ordinary traversal works, and it is a field
	// rather than a constant because a test that wants to check the mode is
	// honoured needs to change it.
	RootMode uint32

	// InvalidateTable bounds how many paths are remembered as having been
	// handed to the kernel. FUSE has no whole-filesystem invalidate, so a
	// refresh has to walk what was given out; without a cap that set would grow
	// for the life of the mount. A path evicted from it is not remembered, and
	// the attr_timeout bounds how long the kernel can act on the stale
	// attribute -- so the cap trades a bounded amount of staleness for a
	// bounded amount of memory.
	InvalidateTable int

	// ReconnectMin and ReconnectMax bound the supervisor's backoff. The first
	// retry happens after ReconnectMin and each later one doubles, up to
	// ReconnectMax.
	//
	// The growth matters more than either number: an owner that is down for a
	// moment comes back after one short wait, while an owner that is gone for an
	// hour is not retried a thousand times. The two are fields rather than
	// constants so the tests can drive the whole schedule in milliseconds.
	ReconnectMin time.Duration
	ReconnectMax time.Duration

	// GiveUpAfter is how long a mount will keep trying before it unmounts
	// itself. A mount whose owner is gone must stop being a mount: leaving it up
	// means every program that touches the path blocks until its own timeout,
	// repeatedly, with no end.
	GiveUpAfter time.Duration
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
// 16 MiB of accumulated writes per open: this version targets files up to
// 10 MB, and the extra headroom covers the untouched content a partial edit
// loads into the same buffer. A larger file is refused with ENOSPC rather than
// silently truncated, so the failure is visible.
//
// 1 second for both timeouts: long enough that a walk of a directory does not
// re-ask for every attribute, short enough that a change made elsewhere is seen
// in about a second without this process having to be told.
// 4096 paths in the handed-out table: a working set larger than almost any
// interactive session's, small enough that the table is a few hundred kilobytes
// rather than a second copy of the tree.
//
// 1 second, 30 seconds and 2 minutes for the reconnect schedule: one second is
// long enough that a brief network hiccup is ridden out without a thundering
// herd of immediate reopen attempts, 30 seconds is short enough that a peer
// coming back is noticed within half a minute, and two minutes is long enough
// that a genuinely transient outage does not cost the user their mount.
func DefaultOptions() Options {
	return Options{
		BufferLimit:     64 << 20,
		BufferTotal:     256 << 20,
		WriteLimit:      16 << 20,
		ChunkSize:       chunkSize,
		AttrTimeout:     5 * time.Second,
		EntryTimeout:    5 * time.Second,
		ListingTimeout:  30 * time.Second,
		RootMode:        0o755,
		InvalidateTable: 4096,
		ReconnectMin:    time.Second,
		ReconnectMax:    30 * time.Second,
		GiveUpAfter:     2 * time.Minute,
	}
}

// readChunk is the ranged-read unit: the configured size, or the package
// default for a Mount built without one.
func (m *Mount) readChunk() int {
	if m.opts.ChunkSize > 0 {
		return m.opts.ChunkSize
	}
	return chunkSize
}

// call returns the current caller, under the lock.
//
// Every use of the caller goes through this rather than reading the field,
// because a reconnect replaces the field from the supervisor's goroutine while
// other threads are in the middle of operations. Taking the lock is what makes
// the swap safe; the alternative, an atomic holding an interface, buys nothing
// here because the lock is already held around the surrounding state changes.
func (m *Mount) call() Caller {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caller
}
