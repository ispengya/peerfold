// The cgofuse adapter: the thin layer between a filesystem interface designed
// for a kernel and the operation core in this package.
//
// It is thin on purpose. Everything in it is a translation of one of three
// things -- a return convention, a struct, or a callback -- and none of it makes
// a decision the core has not already made. Where a translation could hide a
// mistake (the byte counts, the handle order, the mode bits) the adapter passes
// the value through untouched and a test asserts the crossing.
package mount

import (
	"io"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"peerfold/internal/node"
	"peerfold/internal/viewer"
)

// cgofuseHost adapts the operation core to fuse.FileSystemInterface.
//
// fuse.FileSystemBase is embedded for its defaults, not for a method set to
// build on. It implements every method of the interface: Init and Destroy do
// nothing and the rest return -ENOSYS (fsop.go:423-427). That is exactly the
// answer the mount wants for the operations a peerfold share has no concept of
// -- mknod, link, symlink, chmod, chown, utimens, access, and the xattr writes --
// so embedding it is the honest way to say "not supported" rather than writing
// those characters out thirty times.
//
// The methods below are the ones that must NOT be -ENOSYS: the operations the
// mount actually serves, plus Getxattr, which refuses for a different reason.
type cgofuseHost struct {
	fuse.FileSystemBase
	m *Mount
}

// The compile-time proof that the adapter is usable as a filesystem. It is the
// only assertion worth making about the method set, because the reflection count
// that looks like a stronger one is satisfied by the embedded base and would hold
// even if nothing were implemented here.
var _ fuse.FileSystemInterface = (*cgofuseHost)(nil)

// intToStat translates the core's attribute struct into cgofuse's.
//
// Mode crosses unchanged. It is a bitfield in both, the core already set the
// S_IFMT bits for the entry's kind, and translating it here would be a second
// chance to get it wrong -- a file described with the directory bit set makes the
// kernel try to list it.
//
// All three timestamps come from the one Mtime the index carries. A federation has
// no notion of when a file was last read, and inventing one would be inventing a
// fact; reporting the modification time for all three is what a filesystem with no
// atime support reports.
func intToStat(in *Stat) fuse.Stat_t {
	// Mtime is SECONDS since the epoch, not nanoseconds. Building it with
	// time.Unix(0, mtime) would be off by a factor of a billion and date every file
	// to 1970, which is the kind of mistake a test that only checks the field is
	// non-zero would not catch -- so the test checks the value.
	t := fuse.NewTimespec(time.Unix(in.Mtime, 0))
	return fuse.Stat_t{
		Mode:  in.Mode,
		Nlink: in.Nlink,
		Size:  in.Size,
		Atim:  t,
		Mtim:  t,
		Ctim:  t,
	}
}

// NewHost builds the cgofuse host around the operation core, and returns both.
//
// The core comes back with the host rather than being hidden inside it because
// the runner needs it: it starts the supervisor after the mount is up, and it
// flushes the last of the buffered writes on the way out. A caller that only had
// the host would have to build a second Mount to do either, and that second Mount
// would not be the one the kernel is talking to.
func NewHost(caller Caller, opts Options) (*fuse.FileSystemHost, *Mount) {
	m := New(caller, opts)
	h := &cgofuseHost{m: m}
	host := fuse.NewFileSystemHost(h)

	// Windows caches directory entries for the life of the handle, so a peer that
	// connects or disconnects is invisible until the cache is dropped. ReaddirPlus
	// hands the kernel each entry's attributes along with its name, which is both
	// a round trip saved per file and the only way the listing can carry a mode.
	host.SetCapReaddirPlus(true)

	// The invalidation hook. This is the one kernel-cache primitive FUSE offers,
	// and it is the reason the core keeps a table of the paths it has handed out:
	// there is no "everything is stale" call, so a reconnect has to name each path
	// it wants the kernel to forget.
	//
	// The per-path action is NOTIFY_CREATE|NOTIFY_UNLINK. That combination reads
	// oddly for a single path and is deliberate: it is the pair that makes the
	// kernel revalidate an entry rather than use its cached attributes, and it does
	// not assert whether the node was created, removed or changed -- which the
	// mount does not know, because a peer's tree can have done any of the three
	// while the connection was down.
	//
	// Notify's bool return says whether the OS accepted the notification, and it
	// is reported rather than dropped: the core counts the refusals across a whole
	// refresh and logs one line, because a refused notification means the kernel
	// goes on serving an attribute the mount knows to be wrong.
	m.invalidatePath = func(p string) bool {
		return host.Notify(p, fuse.NOTIFY_CREATE|fuse.NOTIFY_UNLINK)
	}

	return host, m
}

// viewerCaller adapts a *viewer.Viewer to the Caller interface this package is
// written against.
//
// The adaptation is almost entirely mechanical. The one part that is not is
// Watch: the viewer publishes batches of events as it receives them, and the core
// wants the event's own explanation of a sequence gap, so the channel is
// translated rather than reinterpreted.
type viewerCaller struct {
	v *viewer.Viewer

	// watchOnce makes the translation start exactly once, so a second call to
	// Watch returns the same channel instead of a second consumer racing the
	// first for the same batches.
	watchOnce sync.Once
	batches   chan Batch
}

// NewViewerCaller wraps a viewer so the mount can be built over a real
// connection.
func NewViewerCaller(v *viewer.Viewer) Caller { return &viewerCaller{v: v} }

func (c *viewerCaller) List(path string) ([]node.Node, error) { return c.v.List(path) }

func (c *viewerCaller) Read(path string, off int64, n int) ([]byte, error) {
	return c.v.Read(path, off, n)
}

func (c *viewerCaller) Put(path string, r io.Reader) error { return c.v.Put(path, r) }

func (c *viewerCaller) Mkdir(path string) error { return c.v.Mkdir(path) }

func (c *viewerCaller) Remove(path string) error { return c.v.Remove(path) }

func (c *viewerCaller) Rename(oldPath, newPath string) error {
	return c.v.Rename(oldPath, newPath)
}

// Watch translates the viewer's event channel into the core's batch channel.
//
// viewer's Event and this package's Batch carry the same two things, and they are
// deliberately separate types: node.Node is the filesystem document, and a
// sequence number is transport metadata that has no business in it. The
// translation is therefore a copy of the number and a pass-through of the nodes.
//
// The goroutine ends when the viewer's channel closes, which it does when the
// viewer is closed. Nothing here closes the viewer's channel -- that lifetime
// belongs to the viewer -- and a channel closed by its owner is what lets a
// ranging consumer leave rather than sit on a channel nobody will ever send to.
func (c *viewerCaller) Watch() <-chan Batch {
	c.watchOnce.Do(func() {
		events := c.v.Watch()
		c.batches = make(chan Batch, cap(events))
		go func() {
			defer close(c.batches)
			for e := range events {
				c.batches <- Batch{Seq: e.Seq, Nodes: e.Nodes}
			}
		}()
	})
	return c.batches
}

// Init is the mount coming up, which is when the watch loop starts.
//
// It is not in the core's New on purpose: a mount that has not been mounted yet
// has nothing to watch, and a watch loop started before Mount() would consume the
// connection's pushed updates into a table no kernel can see.
func (h *cgofuseHost) Init() {
	h.m.startWatch()
}

// Destroy is the mount going away, by unmount or by signal.
//
// Both of those are chances to save rather than crashes, so the last thing this
// does is write out everything still buffered. The order matters: the watch is
// stopped first, so an incoming push cannot land in the table while the flush is
// walking the handles, and then the flush runs while the connection is still
// usable.
func (h *cgofuseHost) Destroy() {
	h.m.stopWatch()
	h.m.flushAllHandles()
}

func (h *cgofuseHost) Getattr(path string, st *fuse.Stat_t, fh uint64) int {
	var core Stat
	if code := h.m.Getattr(path, &core, fh); code != 0 {
		return code
	}
	*st = intToStat(&core)
	return 0
}

// Readdir translates the fill callback.
//
// The core's callback takes a *Stat and cgofuse's takes a *fuse.Stat_t, so each
// entry's attributes are converted on the way through rather than dropped: with
// ReaddirPlus enabled the kernel takes the mode and size from this call, and a
// listing that passed nil for every entry would describe a directory of
// attribute-less files.
//
// A nil *Stat from the core means "." or "..", which cgofuse wants a nil for.
// Passing a zero Stat_t there instead would claim both entries are mode-0 files.
//
// The core is given the caller's offset and returns 0 whether or not fill stopped
// early: stopping early is "the buffer is full", not a failure.
func (h *cgofuseHost) Readdir(path string, fill func(name string, st *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	return h.m.Readdir(path, func(name string, st *Stat, off int64) bool {
		if st == nil {
			return fill(name, nil, off)
		}
		fs := intToStat(st)
		return fill(name, &fs, off)
	}, ofst, fh)
}

func (h *cgofuseHost) Statfs(path string, st *fuse.Statfs_t) int {
	var core Statfs
	if code := h.m.Statfs(path, &core); code != 0 {
		return code
	}
	*st = fuse.Statfs_t{
		Bsize:   core.Bsize,
		Frsize:  core.Bsize,
		Blocks:  core.Blocks,
		Bfree:   core.Bfree,
		Bavail:  core.Bavail,
		Files:   core.Files,
		Ffree:   core.Ffree,
		Favail:  core.Ffree,
		Namemax: core.Namemax,
	}
	return 0
}

// The handle-returning calls unpack the core's order into cgofuse's, which is the
// reverse. On failure cgofuse expects ^uint64(0), and that is what the core
// already returns, so it crosses unchanged -- returning 0 instead would collide
// with a real handle.
//
// The names below are deliberately not the same as the core's. A local named fh
// bound to the core's *second* result reads as a handle, and the mistake is
// invisible: both orders compile when the types happen to agree, which they do
// for the handle alone and do not for the pair.
func (h *cgofuseHost) Open(path string, flags int) (int, uint64) {
	handle, code := h.m.Open(path, isReadOnlyFlags(flags))
	return code, failureHandle(code, handle)
}

func (h *cgofuseHost) Opendir(path string) (int, uint64) {
	handle, code := h.m.Opendir(path)
	return code, failureHandle(code, handle)
}

func (h *cgofuseHost) Create(path string, flags int, mode uint32) (int, uint64) {
	handle, code := h.m.Create(path, mode)
	return code, failureHandle(code, handle)
}

// noHandle is cgofuse's "there is no handle" value: it returns ^uint64(0) from
// every call that fails, and the convention is visible in the library rather than
// documented. The core returns 0 instead, which is not merely a different choice --
// 0 is a value a real handle can have, so a caller that checks the handle and not
// the status would take a failed open as handle zero.
const noHandle = ^uint64(0)

// failureHandle replaces a failed call's handle with cgofuse's sentinel. It reads
// the status rather than the handle, so a failure is a failure whatever handle the
// core happened to return beside it.
func failureHandle(code int, handle uint64) uint64 {
	if code < 0 {
		return noHandle
	}
	return handle
}

// isReadOnlyFlags answers whether a cgofuse open mode allows writing.
//
// The core's Open takes a mode of its own rather than cgofuse's flags, because
// the read-only/read-write distinction is the only part a federation can honour:
// an append, a truncate-on-open and an exclusive create are all decisions about
// the owner's copy, and the owner is the one that makes them. Reading the access
// bits out of the flag word is therefore the whole translation.
func isReadOnlyFlags(flags int) int {
	if flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0 {
		return OReadWrite
	}
	return OReadOnly
}

// Read and Write pass the core's byte count through untouched, because cgofuse
// uses the same convention: a positive return is a count and a negative one is a
// negated errno. Re-encoding either as a status would turn "read 5 of 16 bytes"
// into "success", which a caller cannot tell from a full buffer.
func (h *cgofuseHost) Read(path string, buff []byte, ofst int64, fh uint64) int {
	return h.m.Read(path, buff, ofst, fh)
}

func (h *cgofuseHost) Write(path string, buff []byte, ofst int64, fh uint64) int {
	return h.m.Write(path, buff, ofst, fh)
}

func (h *cgofuseHost) Truncate(path string, size int64, fh uint64) int {
	return h.m.Truncate(path, size, fh)
}

func (h *cgofuseHost) Fsync(path string, datasync bool, fh uint64) int {
	return h.m.Fsync(path, datasync, fh)
}

func (h *cgofuseHost) Flush(path string, fh uint64) int { return h.m.Flush(path, fh) }

func (h *cgofuseHost) Release(path string, fh uint64) int { return h.m.Release(path, fh) }

// Releasedir shares the core's Release.
//
// The core keys handles by number in one table, so a directory handle and a file
// handle are the same kind of thing to it and releasing either closes the entry
// and frees whatever it was holding. There is no separate directory state to drop.
func (h *cgofuseHost) Releasedir(path string, fh uint64) int { return h.m.Release(path, fh) }

// Fsyncdir shares the core's Flush.
//
// A directory has no content of its own, so there is nothing for a directory sync
// to write. It must still not fall through to the base class's ENOSYS: a tool that
// synchronises a directory before renaming into it treats a refusal as "the rename
// may not be durable" and reports a failure that did not happen.
func (h *cgofuseHost) Fsyncdir(path string, datasync bool, fh uint64) int {
	return h.m.Flush(path, fh)
}

func (h *cgofuseHost) Unlink(path string) int { return h.m.Unlink(path) }

func (h *cgofuseHost) Rmdir(path string) int { return h.m.Rmdir(path) }

func (h *cgofuseHost) Mkdir(path string, mode uint32) int { return h.m.Mkdir(path, mode) }

func (h *cgofuseHost) Rename(oldpath string, newpath string) int {
	return h.m.Rename(oldpath, newpath)
}

// Getxattr answers "no such attribute" rather than the base class's ENOSYS.
//
// The difference is not pedantic. Some tools treat ENOSYS from getxattr as
// evidence that the filesystem is broken and refuse to copy into it; ENOATTR
// tells them the same thing -- "this filesystem has no extended attributes" --
// in the form they are prepared to accept.
//
// The name is cgofuse's own. Its value is 120 on every platform it builds for,
// which is not the number a Linux kernel gives that condition, so the constant is
// the library's rather than one chosen here.
func (h *cgofuseHost) Getxattr(path string, name string) (int, []byte) {
	return -ENOATTR, nil
}

// Listxattr reports that there are no attributes to list.
//
// It does not call fill: fill exists to hand over names, and there are none. A
// zero return with no calls is the empty list, and it is what keeps a copying tool
// from concluding the filesystem cannot be read from.
func (h *cgofuseHost) Listxattr(path string, fill func(name string) bool) int { return 0 }
