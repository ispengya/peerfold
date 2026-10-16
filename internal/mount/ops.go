package mount

import (
	stdpath "path"
	"sort"
	"strings"
	"time"

	"peerfold/internal/node"
)

// The type bits, matching cgofuse's S_IFDIR, S_IFREG and S_IFLNK, which are the
// standard POSIX values. They are declared here rather than imported because the
// operation core does not import a FUSE library, and a test asserts they equal
// the octal constants so a typo cannot make directories appear as files.
const (
	modeDir     = 0o040000
	modeRegular = 0o100000
	modeSymlink = 0o120000

	// modeTypeMask covers the type field, which is one field rather than three
	// independent bits and so cannot be tested bit by bit.
	//
	// This trips people up because the numbers are not disjoint bit sets:
	// 0o120000 contains 0o100000, so `mode&modeRegular != 0` is true for a
	// symlink as well as for a regular file. The only sound question is whether
	// `mode&modeTypeMask` equals one particular type exactly, and that is what
	// every test about kinds asks.
	modeTypeMask = modeDir | modeRegular | modeSymlink
)

// Stat is the four fields the core needs. The host adapter copies them into
// whatever the platform's stat structure looks like.
//
// The core has its own type rather than using cgofuse's because that one has
// fifteen fields and differs between platforms. Four fields are all that can be
// derived from an index entry, and keeping the core's view small is what lets its
// tests run on any platform without the FUSE library.
type Stat struct {
	Mode  uint32
	Size  int64
	Mtime int64
	Nlink uint32
}

// Statfs is synthetic. There is no single true capacity for a federation of
// other machines' disks -- the total changes as peers come and go, and no one
// owns the number -- so the figures only have to be self-consistent and non-zero.
//
// A zero Bsize makes some tools divide by zero, and a zero Files makes some
// refuse to write, so the values are deliberately optimistic: reporting a full
// disk when the federation has room would break writes for no reason. The
// figures are also ordered the way a real filesystem orders them -- free does not
// exceed total, and Bavail does not exceed Bfree -- because a tool that reads
// them can reasonably assume that.
type Statfs struct {
	Bsize   uint64
	Blocks  uint64
	Bfree   uint64
	Bavail  uint64
	Files   uint64
	Ffree   uint64
	Namemax uint64
}

// typeBits maps an index kind onto the mode bits that say what the entry is.
//
// The default is the regular-file bit rather than the directory bit: claiming
// something is a directory makes the kernel try to list it, so an unknown kind is
// safer described as a file that will fail to read than as a directory whose
// listing will fail.
// toIndexPath converts a FUSE absolute path into the index format that
// splitPath() in internal/viewer expects. Clean() and lookup() work in FUSE's
// world (absolute, leading slash), and the test fake Caller works in the same
// world, but viewerCaller's methods are the real boundary where paths must be
// converted to index format (no leading slash) before reaching the wire.
//
// The two conventions meet exactly there, in viewerCaller's methods; every
// layer below (viewer, server, index) works in the relative format.
func toIndexPath(p string) string {
	if p == "/" || p == "" {
		return ""
	}
	return strings.TrimPrefix(p, "/")
}

func typeBits(kind node.Kind) uint32 {
	switch kind {
	case node.KindDir:
		return modeDir
	case node.KindSymlink:
		return modeSymlink
	default:
		return modeRegular
	}
}

// statFor turns an index entry into a Stat.
//
// The owner's permission bits are preserved and the type bits are replaced, not
// merged. The index records the owner's full FileMode since the scan started
// reporting it, so the type bits are already present in n.Mode; ORing the kind's
// bits on top without clearing the originals would set two type bits at once,
// and which one a reader honours is then arbitrary.
//
// The mask keeps the permission and setuid/setgid/sticky bits and drops
// everything above them.
func statFor(n node.Node) Stat {
	mode := uint32(n.Mode) & 0o7777
	mode |= typeBits(n.Kind)

	// A directory's link count is 2 by convention (its own "." and its parent's
	// entry for it). Files are 1. A zero here makes some tools conclude the
	// filesystem is broken and skip the entry entirely.
	nlink := uint32(1)
	if n.Kind == node.KindDir {
		nlink = 2
	}
	return Stat{Mode: mode, Size: n.Size, Mtime: n.Mtime, Nlink: nlink}
}

// Getattr reports the attributes of one path.
//
// When the path is open, a handle that has been written reports the size it is
// about to have rather than the index's last known size: otherwise a program that
// wrote past the old end sees its own write vanish on the next stat, which is how
// a save that grows a file corrupts it. A clean handle has nothing to say, so the
// index's value is used.
func (m *Mount) Getattr(path string, st *Stat, fh uint64) int {
	n, code := m.lookup(path)
	if code != 0 {
		return code
	}
	// The index does not carry a mode beyond the type, and Windows refuses an
	// open whose reported mode lacks the bit the open asked for -- which is how
	// a file the owner recorded as 0644 became unopenable through the mount.
	// The owner's own read-only choice is preserved on the owner's disk; the
	// mount is a federation surface, and every node it reports is readable,
	// writable and traversable by the caller.
	n.Mode |= 0o700
	m.mu.Lock()
	if h, ok := m.handle(fh); ok && h.dirty {
		n.Size = h.size
	}
	m.mu.Unlock()
	*st = statFor(n)
	// The kernel now holds this path's attributes. Recording it is what gives a
	// later reconnect something to invalidate: without it the kernel's cached
	// mode and size for every path the user has looked at would survive the whole
	// outage, and the mount's own answers would be the fresher of the two.
	m.noteInode(fh, n.Path)
	return 0
}

// Readdir lists one directory.
//
// The two dot entries are synthesised here because the caller's List returns real
// entries only. They are named "." and "..", which is what WinFsp expects from a
// directory enumeration; the host adapter passes the name through unchanged. Their
// stat is nil, which saves a Getattr per entry and is what cgofuse's own examples
// pass.
//
// The off parameter is accepted and ignored, which is what cgofuse's
// documentation and both its example filesystems do. It is a resume hint: a
// filesystem that wants to be seekable emits it with each entry to say where to
// continue. Emitting every entry from the start with a fresh listing on each call
// is the simpler contract, and it is what the library's own filesystems rely on,
// so this is the behaviour to be compatible with rather than a shortcut.
func (m *Mount) Readdir(path string, fill func(name string, st *Stat, off int64) bool, off int64, fh uint64) int {
	n, code := m.lookup(path)
	if code != 0 {
		return code
	}
	if n.Kind != node.KindDir {
		return -ENOTDIR
	}
	children, err := m.call().List(path)
	if err != nil {
		return errnoFor(err)
	}
	// Sorted so a listing is stable between reads. The index's own order is the
	// server's business and may change without the tree changing, which would
	// make a directory appear to shuffle for no reason the user can see.
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })

	if !fill(".", nil, 0) {
		return 0
	}
	if !fill("..", nil, 0) {
		return 0
	}
	for _, c := range children {
		st := statFor(c)
		// Every child is reported with owner read, write and execute, the same
		// widening Getattr applies: Windows refuses an open with access denied
		// when the reported mode lacks the bit the open asked for, and a
		// directory listing is where the kernel caches the mode the next open
		// will be checked against.
		st.Mode |= 0o700
		if c.Kind == node.KindSymlink {
			// Visible, but flagged. The mount layer refuses to open it and a
			// caller that stats it gets ELOOP, so a listing that passed it off as
			// a regular file with a plausible size would be lying twice.
			st.Mode |= modeSymlink
		}
		// The kernel is being handed this child's name and attributes, so the
		// child is recorded as handed out. A listing is where the kernel learns
		// about paths it will go on to stat, and invalidating only what was
		// explicitly statted would leave every name discovered through a listing
		// cached across the whole outage.
		m.noteInode(fh, c.Path)
		if !fill(stdpath.Base(c.Path), &st, 0) {
			// The callback returns false when the kernel's buffer is full. That
			// is not an error and the rest of the listing is not lost: the kernel
			// asks again.
			return 0
		}
	}
	m.noteInode(fh, n.Path)
	return 0
}

// Statfs reports invented figures for the whole federation.
//
// The path is still resolved, so a tool asking about a directory that does not
// exist is told ENOENT rather than handed numbers. The numbers themselves do not
// depend on the path: they describe one federation, and every point in the mount
// sees the same one.
//
// A disconnected mount still answers, and that is deliberate rather than an
// oversight. The figures are constants; there is no wire call to fail, and no
// answer here can be stale in a way that matters. The alternative was worse:
// cgofuse's fsstat callback has no way to report an error, so a tool that statfs'es
// a mount point before it does anything else would be told the numbers are fine
// and then get EIO from the first real operation -- which is the honest order.
// This is the one operation that does not go through lookup(), because it is the
// only one with nothing to look up.
func (m *Mount) Statfs(path string, st *Statfs) int {
	if !m.offline() {
		if _, code := m.lookup(path); code != 0 {
			return code
		}
	}
	const block = 4096
	*st = Statfs{
		Bsize:   block,
		Blocks:  1 << 30 / block, // 1 GiB of claimed capacity
		Bfree:   1 << 29 / block,
		Bavail:  1 << 29 / block,
		Files:   1 << 20,
		Ffree:   1 << 19,
		Namemax: 255, // the longest component the federation is willing to carry
	}
	return 0
}

// cgofuse's open flags. The core declares them because it does not import the
// library; host.go passes through whatever the platform gave it. Only the write
// bit is ever read.
const (
	// OReadOnly is a read-only open. It is zero, so a handle whose mode was
	// never set reads as read-only rather than accidentally writable.
	OReadOnly = 0

	// OReadWrite is what cgofuse passes for a writable open.
	OReadWrite = 2
)

// Open opens one file and returns a handle.
//
// A directory is EISDIR rather than being opened: Opendir is the other path, and
// a file handle on a directory would have to answer reads that have no meaning.
// The check is on the resolved kind, so a symlink is refused earlier, by lookup,
// with ELOOP.
func (m *Mount) Open(path string, mode int) (uint64, int) {
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	// A directory opened through the file-open callback must be refused before
	// cgofuse writes a handle into WinFsp's per-open context. Opendir owns
	// directory handles; Open must not invent one. A pending name this mount
	// just created with Mkdir is a directory even though the index has not
	// caught up, so the pending table is consulted first.
	if m.pendingDirectory(cleaned) && m.isDirPath(cleaned) {
		return 0, -EISDIR
	}
	// The parent lookup enforces the shape of the path: a missing parent is
	// ENOENT and a parent that is a file is ENOTDIR. lookup's own ENOTDIR only
	// covers the immediate parent, so a path beneath a file further up -- which
	// the index answers ENOENT for -- is caught here instead, and the two agree
	// on the answer: this path sits beneath a file. ENOENT is not refused here:
	// the leaf lookup below settles whether the name exists, and a pending
	// name's parent resolves through the pending table even though the index
	// has not caught up.
	if _, _, code := m.lookupParent(cleaned); code == -ENOTDIR {
		return 0, code
	}
	// The whole path is then resolved, because lookupParent sees only the
	// parent: a symlink or a directory at the *leaf* is invisible to it. This
	// is the same call the create and read paths make, so the refusals agree.
	//
	// The lookup cannot move after the lock. It lists through the caller, and
	// holding the mount's lock while waiting on the wire is the deadlock this
	// package avoids everywhere else.
	n, code := m.lookup(cleaned)
	if code != 0 {
		return 0, code
	}
	if n.Kind == node.KindDir {
		return 0, -EISDIR
	}
	m.mu.Lock()
	fh := m.newHandleLocked(cleaned, n.Kind)
	h, _ := m.files[fh]
	// The size is seeded from the index so a read of a file that is never written
	// still knows where the end is without asking the owner. contentSize records
	// the same number as the pre-modification length, which is what the truncate
	// path needs to decide whether the buffer holds the file.
	h.size = n.Size
	h.contentSize = n.Size
	// Anything that is not the read-write open is treated as read-only. O_WRONLY
	// is a separate flag value in POSIX, so testing for equality with OReadOnly
	// would classify a write-only open as read-write, and testing for equality
	// with OReadWrite would classify it as read-only -- both wrong. The only
	// question the mount asks is whether writes are allowed, and only OReadWrite
	// allows them.
	h.readOnly = mode != OReadWrite
	m.mu.Unlock()
	// The kernel has an open handle on this path and will read its attributes
	// from here. Recording it is what makes a later reconnect able to tell the
	// kernel to drop them.
	m.noteInode(fh, n.Path)
	return fh, 0
}

// Opendir opens one directory and returns a handle.
//
// The handle is what ties a Read or Readdir to the path it was opened for. The
// core does not keep per-directory state yet -- the listing is fetched afresh
// each time -- but the handle is still allocated so that a released directory is
// tracked the same way a released file is.
// newPendingDirectoryHandle allocates a handle for a directory created through
// this mount but not yet present in the server's index.
//
// It is deliberately separate from Opendir: the normal path resolves the
// directory through the wire, while this one is the narrow post-Mkdir window
// where the client has not pushed the new node yet.
// isDirPath reports whether the pending name is a directory rather than a file.
func (m *Mount) isDirPath(path string) bool {
	cleaned, err := Clean(path)
	if err != nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.files {
		if h.path == cleaned && h.kind == node.KindDir {
			return true
		}
	}
	return false
}

func (m *Mount) pendingDirectory(path string) bool {
	cleaned, err := Clean(path)
	if err != nil {
		return false
	}
	if m.offline() {
		return false
	}
	m.mu.Lock()
	if _, pending := m.pendingPaths[cleaned]; pending {
		m.mu.Unlock()
		return true
	}
	var open bool
	for _, h := range m.files {
		if h.path == cleaned {
			open = true
			break
		}
	}
	m.mu.Unlock()
	return open
}

func (m *Mount) newPendingDirectoryHandle(path string) (uint64, int) {
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	if m.offline() {
		return 0, -EIO
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fh := m.newHandleLocked(cleaned, node.KindDir)
	return fh, 0
}

func (m *Mount) Opendir(path string) (uint64, int) {
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return 0, code
	}
	if n.Kind != node.KindDir {
		return 0, -ENOTDIR
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fh := m.newHandleLocked(cleaned, node.KindDir)
	m.noteInodeLocked(fh, n.Path)
	return fh, 0
}

// Read places the file's bytes at off into buf and returns how many were placed.
//
// The return is a byte count the way cgofuse wants, not a Go error: zero means
// end of file and a negative value is an errno. That is why the count and the
// error share one int, and why a caller must not treat a small positive value as
// a failure.
//
// The wire is asked in chunkSize blocks with a positive count every time. The
// caller's remaining length cannot be forwarded as-is because the wire's own Read
// treats a negative n as "to the end" and a zero n as an empty slice rather than
// an error, so forwarding would either return nothing or return everything for
// the common request of a large range.
func (m *Mount) Read(path string, buf []byte, off int64, fh uint64) int {
	if off < 0 {
		return -EINVAL
	}
	if len(buf) == 0 {
		// The kernel does issue these. An empty success, not an error.
		return 0
	}

	// The handle is consulted under the lock, and everything needed is copied
	// out there. The lock is released before any wire call: a read can take tens
	// of seconds, and holding the mount's lock for that would block every other
	// operation, including the ones that would notice a dead connection.
	t, code := m.readTarget(path, fh)
	if code != 0 {
		return code
	}

	// A directory has no bytes. This is the read path's own check rather than
	// relying on Open's, because a handle obtained from Opendir reaches here
	// legitimately and the kernel can also read a path it never opened.
	if t.kind == node.KindDir {
		return -EISDIR
	}

	// A dirty handle answers from the snapshot taken under the lock. This is not
	// an optimisation: the owner's copy is the *old* file, so a program that
	// wrote and read back would otherwise see its own write vanish. The
	// whole-file buffer is what the wire will eventually carry, so serving reads
	// from it is the same data.
	//
	// The snapshot is used rather than re-reading the handle, which is what makes
	// the decision above consistent with the answer here. Re-looking it up would
	// discard the dirty flag this branch was chosen from, and a read that raced a
	// flush would then serve zeros from the clean path.
	if t.content != nil {
		if off >= int64(len(t.content)) {
			return 0
		}
		return copy(buf, t.content[off:])
	}

	// A clean handle knows the size it was opened with. A read that starts at or
	// past it is end of file, and that answer must not depend on asking the
	// owner, because a stale index that claims more than the owner has would
	// otherwise turn a legitimate read into a wire failure.
	if t.size > 0 && off >= t.size {
		return 0
	}

	total := 0
	for total < len(buf) {
		pos := off + int64(total)
		chunkIdx := pos / chunkSize
		chunkOff := chunkIdx * chunkSize

		chunk, ok := m.cache.get(fh, chunkIdx)
		if !ok {
			data, err := m.call().Read(t.path, chunkOff, chunkSize)
			if err != nil {
				// A partial read is returned rather than discarded. Throwing
				// away the bytes already in the buffer would make a large file
				// unreadable over a flaky link, because the retry starts from
				// scratch and hits the same failure.
				if total > 0 {
					return total
				}
				return errnoFor(err)
			}
			// The count asked for is always chunkSize, so the slice is already
			// bounded; trimming is belt and braces so the cache can never hold a
			// chunk larger than the unit the caps are expressed in.
			if len(data) > chunkSize {
				data = data[:chunkSize]
			}
			chunk = data
			m.cache.put(fh, chunkIdx, chunk)
		}
		if len(chunk) == 0 {
			break // end of file
		}
		within := pos - chunkOff
		if within >= int64(len(chunk)) {
			break // the chunk does not reach the offset, so the file ends here
		}
		total += copy(buf[total:], chunk[within:])
		if len(chunk) < chunkSize {
			break // a short chunk means the file ended
		}
		// No "buffer is full" check belongs here. Each iteration advances the
		// position by a whole chunk because a full-size chunk always moves
		// total to at least the next boundary, so the only way to return to the
		// same leftover bytes is a short chunk -- caught above. A guard was
		// written here and then removed, because no input could make it fire and
		// a line no test can reach is a line no test can defend.
	}
	return total
}

// readSnapshot is what a read needs to know about the thing it is reading,
// captured under one lock so the answer cannot change between the decision and
// the use.
type readSnapshot struct {
	path string
	kind node.Kind
	size int64
	// content is the handle's whole buffer when the handle is dirty, and nil
	// otherwise. Nil rather than an empty slice on purpose: an empty file is a
	// legitimate dirty state, so "has content" cannot be tested by length.
	content []byte
}

// readTarget resolves what a read should use, and captures the handle's state in
// the same lock acquisition.
//
// An unknown handle is what cgofuse passes when the kernel read a path it never
// opened, so the path is resolved directly for that case. The resolution is what
// makes a read of a path that does not exist ENOENT rather than a silent empty
// success.
func (m *Mount) readTarget(path string, fh uint64) (readSnapshot, int) {
	m.mu.Lock()
	if h, ok := m.files[fh]; ok {
		t := readSnapshot{path: h.path, kind: h.kind, size: h.size}
		if !h.isClean() {
			// Materialising now, under the lock, is what keeps the bytes
			// consistent with the dirty state that selected this branch. It costs
			// a copy of a buffer already bounded by the write limit, and it is
			// the same buffer the flush will send.
			t.content = h.content()
		}
		m.mu.Unlock()
		return t, 0
	}
	m.mu.Unlock()

	cleaned, err := Clean(path)
	if err != nil {
		return readSnapshot{}, -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return readSnapshot{}, code
	}
	return readSnapshot{path: cleaned, kind: n.Kind, size: n.Size}, 0
}

// pendingStat reports attributes for a path whose handle exists before the
// owner's index has caught up.
//
// It is not a general-purpose stat: lookup remains authoritative for every path
// once the index has an answer. This exists for WinFsp's immediate attribute
// query after Create/Open, where ENOENT would fail a successful operation.
func (m *Mount) pendingStat(path string, fh uint64, st *Stat) int {
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	m.mu.Lock()
	h, ok := m.files[fh]
	if !ok || h.path != cleaned {
		for _, candidate := range m.files {
			if candidate.path == cleaned {
				h, ok = candidate, true
				break
			}
		}
	}
	var mode uint32
	var size int64
	if ok {
		mode = modeRegular | 0o700
		if h.kind == node.KindDir {
			mode = modeDir | 0o700
		}
		size = h.size
		m.noteInodeLocked(fh, cleaned)
		m.mu.Unlock()
		*st = Stat{Mode: mode, Size: size, Mtime: time.Now().Unix(), Nlink: 1}
		return 0
	}
	m.mu.Unlock()
	return -ENOENT
}

// ---------------------------------------------------------------- the write path

// Create opens a path for writing, making a handle whether or not the file
// already exists.
//
// It is the read-write open, so an existing file is opened rather than refused:
// cgofuse calls Create for O_CREAT on a path that may already be there, and
// answering EEXIST would break every ordinary save. The handle starts empty and
// clean, which is what a freshly opened file is.
//
// A symlink at the path is refused with ELOOP, the same as everywhere else. A
// directory is EISDIR, because a writable handle on a directory would have to
// answer writes that have no meaning.
// pendingCreate reports whether a handle is currently open at the path but the
// index does not know the file yet.
//
// It is used only by the adapter's immediate post-create attribute query. The
// wire is authoritative for every other lookup, so a stale pending handle must
// not answer ordinary reads, opens, or listings.
func (m *Mount) pendingCreate(path string) bool {
	cleaned, err := Clean(path)
	if err != nil {
		return false
	}
	if m.offline() {
		return false
	}
	m.mu.Lock()
	if _, pending := m.pendingPaths[cleaned]; pending {
		m.mu.Unlock()
		return true
	}
	var open bool
	for _, h := range m.files {
		if h.path == cleaned {
			open = true
			break
		}
	}
	m.mu.Unlock()
	if open {
		// An open handle makes the immediate post-open attribute query the owner's
		// authoritative answer rather than a stale negative lookup. This is the
		// same window as a newly created name, and the adapter uses it only for
		// the one callback WinFsp issues immediately after Create/Open.
		return true
	}
	return false
}

func (m *Mount) Create(path string, mode uint32, access int) (uint64, int) {
	if m.offline() {
		return 0, -EIO
	}
	cleaned, err := Clean(path)
	if err != nil {
		return 0, -EINVAL
	}
	// A directory opened through the file-open callback must be refused before
	// cgofuse writes a handle into WinFsp's per-open context. Opendir owns
	// directory handles; Open must not invent one.
	if m.pendingDirectory(cleaned) && m.isDirPath(cleaned) {
		return 0, -EISDIR
	}
	// The parent lookup enforces the shape of the path: a missing parent is
	// ENOENT and a parent that is a file is ENOTDIR. It runs first so a refused
	// create leaves no handle behind, and the base name is not used here because
	// Create allows the name to be taken or free.
	if _, _, code := m.lookupParent(cleaned); code != 0 {
		return 0, code
	}
	// The whole path is then resolved, because lookupParent sees only the parent:
	// a symlink or a directory at the *leaf* is invisible to it. This is the same
	// call the open and read paths make, so the refusals agree.
	//
	// The lookup cannot move after the lock. It lists through the caller, and
	// holding the mount's lock while waiting on the wire is the deadlock this
	// package avoids everywhere else.
	existing, lookupCode := m.lookup(cleaned)
	if lookupCode == -ELOOP {
		return 0, -ELOOP
	}
	if lookupCode == 0 && existing.Kind == node.KindDir {
		return 0, -EISDIR
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fh := m.newHandleLocked(cleaned, node.KindFile)
	h, _ := m.files[fh]
	h.readOnly = access != OReadWrite
	if lookupCode != 0 {
		// The name is not in the index: this is a create, and the new name is
		// pending until the owning client reports it back. Windows revalidates
		// it through the mount before that push arrives, and an ENOENT there
		// makes the open the caller just performed appear not to exist.
		m.pendingPaths[cleaned] = node.KindFile
	}
	if lookupCode == 0 {
		// The file is already there. The index's size is recorded as both the
		// current length and the pre-modification length, so a later truncation
		// can tell whether this handle's buffer actually holds the file.
		h.size = existing.Size
		h.contentSize = existing.Size
	}
	return fh, 0
}

// Write records a write on a handle and returns how many bytes it took.
//
// A write to a path with no handle -- which cgofuse can deliver after a
// connection was replaced -- is applied through a temporary handle that is
// flushed and released straight away, so the write still lands. It is not
// recorded anywhere afterwards, because there is no handle to record it on.
//
// A handle opened read-only is refused with EBADF rather than being silently
// allowed to write. The kernel enforces this itself, but the mount is where the
// mode is known, and honouring it makes the read-only open mean something.
func (m *Mount) Write(path string, buf []byte, off int64, fh uint64) int {
	if off < 0 {
		return -EINVAL
	}
	if len(buf) == 0 {
		// The kernel issues these. A success, not an error, and nothing to
		// record: an editor looping on a zero-length write must not cause an
		// upload per call.
		return 0
	}
	if m.offline() {
		// A write with a handle could be recorded locally and refused later, but
		// refusing it now is the honest answer: the content cannot reach the
		// owner while the connection is gone, and a write that appears to
		// succeed and is then dropped at disconnect is the silent-loss failure
		// the whole flush table exists to prevent. The empty-write case above
		// stays a success because it writes nothing.
		return -EIO
	}

	m.mu.Lock()
	h, ok := m.files[fh]
	if !ok {
		m.mu.Unlock()
		// The directory sentinel WinFsp can hand here has no content and no
		// path that resolves, so it is the same EBADF a genuinely unknown
		// handle gets. Every other unknown handle takes the path form, which
		// is how a write survives a replaced connection.
		if fh == noHandle {
			return -EBADF
		}
		return m.writeThroughPath(path, buf, off)
	}
	if h.kind == node.KindDir {
		m.mu.Unlock()
		return -EISDIR
	}
	if h.readOnly {
		m.mu.Unlock()
		return -EBADF
	}
	// The handle records a write as a status, where zero means it was taken. The
	// interface this method implements wants a byte count instead, so the two are
	// converted here rather than making the handle carry both meanings: a handle
	// status of zero and a byte count of zero are different answers, and a caller
	// that confused them would report every successful write as a no-op.
	code := h.write(off, buf)
	m.mu.Unlock()
	if code != 0 {
		return code
	}
	return len(buf)
}

// writeThroughPath applies one write with no open handle to record it on.
//
// The handle is loaded with the owner's current content first. Without that the
// write would be the whole of the file as far as the upload is concerned, and a
// one-byte write at the end of a large file would replace it with one byte. The
// load reads the whole file, so this path is expensive; the kernel normally
// pairs a write with an earlier open, and this exists so that it cannot silently
// corrupt a file when it does not.
func (m *Mount) writeThroughPath(path string, buf []byte, off int64) int {
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return code
	}
	if n.Kind == node.KindDir {
		return -EISDIR
	}

	m.mu.Lock()
	fh := m.newHandleLocked(cleaned, n.Kind)
	h, _ := m.files[fh]
	h.size = n.Size
	h.contentSize = n.Size
	_, pending := m.pendingPaths[cleaned]
	m.mu.Unlock()
	// The owner's bytes are loaded so a partial write does not become the whole
	// file. A name this mount just created has no owner bytes to load -- the
	// read would fail -- and the empty buffer it would leave behind is exactly
	// the truth: the write below is the whole of the file.
	if !pending {
		if err := m.loadContent(h); err != nil {
			m.mu.Lock()
			m.closeHandle(fh)
			m.mu.Unlock()
			return errnoFor(err)
		}
	}

	m.mu.Lock()
	code = h.write(off, buf)
	m.mu.Unlock()

	if code != 0 {
		m.mu.Lock()
		m.closeHandle(fh)
		m.mu.Unlock()
		return code
	}
	flushed := m.flush(fh)
	m.mu.Lock()
	m.closeHandle(fh)
	m.mu.Unlock()
	if flushed != 0 {
		return flushed
	}
	return len(buf)
}

// loadContent fills a handle's buffer with the owner's current content, so that
// a partial write does not become the whole file.
//
// It is what makes Read-modify-Write correct in the two path-form operations: a
// write of one byte at the end of a large file must upload the whole file with
// that byte changed, not the one byte.
//
// A file the index believed was empty is allowed to be non-empty on the owner,
// which is what a stale index looks like; that is trusted and seeded.
//
// Any other failure leaves the buffer empty, which would make the write that
// follows replace the file with itself. An error from a caller that answered at
// least partly is treated as a success for that reason: the partial content is
// closer to the truth than zeros. A caller that failed outright is reported, so
// the caller of this function can refuse the operation rather than destroy the
// file.
func (m *Mount) loadContent(h *openFile) error {
	body, _, err := m.readPath(h.path)
	if err != nil && len(body) == 0 {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	m.mu.Lock()
	h.writes[0] = append([]byte(nil), body...)
	h.size = int64(len(body))
	m.mu.Unlock()
	// Filling the buffer from the owner is not a mutation, so it does not mark
	// the handle dirty. The write or truncation that follows does.
	return nil
}

// readPath reads a whole file from the owner, without holding the mount's lock.
//
// The chunk size bounds each wire request, so a large file is a sequence of
// bounded reads rather than one unbounded allocation on the far side. n is always
// positive: the wire's Read treats a negative count as "to the end" and a zero
// count as an empty result rather than an error, so forwarding -1 would work by
// accident and forwarding 0 would silently return nothing.
func (m *Mount) readPath(path string) ([]byte, int64, error) {
	var out []byte
	var off int64
	for {
		chunk, err := m.call().Read(path, off, chunkSize)
		if err != nil {
			if len(out) > 0 {
				// A partial read is worth more than none: it is at least the
				// file's beginning.
				return out, int64(len(out)), nil
			}
			return nil, 0, err
		}
		if len(chunk) == 0 {
			return out, int64(len(out)), nil
		}
		out = append(out, chunk...)
		off += int64(len(chunk))
		if len(chunk) < chunkSize {
			return out, int64(len(out)), nil
		}
	}
}

// Truncate sets the file's length.
//
// Through a handle it is a content mutation and the handle stays dirty until a
// flush, because the new length is content. Without a handle the file's current
// bytes are read first and the result is uploaded straight away -- truncating a
// file to zero on a path must actually empty it, and truncating it to a length
// below its size must actually cut it.
func (m *Mount) Truncate(path string, size int64, fh uint64) int {
	if size < 0 {
		return -EINVAL
	}

	m.mu.Lock()
	h, ok := m.files[fh]
	if ok {
		clean := h.isClean()
		m.mu.Unlock()

		if clean {
			// A handle that has never held this file's content is loaded first.
			// Truncation means "this content, of this length", and cutting a
			// buffer that never had the file in it would keep the length and lose
			// the bytes.
			//
			// It is loaded unconditionally rather than only when the index claims
			// a non-zero size. The index is the same source the handle's size came
			// from, so trusting it here would let one stale "this file is empty"
			// shorten a non-empty file -- the silent-loss failure. Reading an
			// empty file costs one round trip and changes nothing.
			//
			// The load happens outside the mount's lock: reading a large file
			// while holding it would stop every other operation for the length of
			// the read, including the ones that would notice a dead connection.
			if err := m.loadContent(h); err != nil {
				return errnoFor(err)
			}
		}

		m.mu.Lock()
		n := h.truncateTo(size)
		m.mu.Unlock()
		return n
	}
	m.mu.Unlock()

	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return code
	}
	if n.Kind == node.KindDir {
		return -EISDIR
	}

	m.mu.Lock()
	tmp := m.newHandleLocked(cleaned, n.Kind)
	th, _ := m.files[tmp]
	th.size = n.Size
	th.contentSize = n.Size
	m.mu.Unlock()

	if err := m.loadContent(th); err != nil {
		m.mu.Lock()
		m.closeHandle(tmp)
		m.mu.Unlock()
		return errnoFor(err)
	}

	m.mu.Lock()
	res := th.truncateTo(size)
	m.mu.Unlock()

	if res < 0 {
		m.mu.Lock()
		m.closeHandle(tmp)
		m.mu.Unlock()
		return res
	}
	code = m.flush(tmp)
	m.mu.Lock()
	m.closeHandle(tmp)
	m.mu.Unlock()
	if code != 0 {
		return code
	}
	return 0
}

// Fsync flushes the handle before returning.
//
// This is the entry that makes the difference between a save that survives a
// power loss and one that reports success having written nothing. Both forms
// flush: the mount cannot write content without also writing metadata, so
// treating datasync as "content may wait" would be a lie.
func (m *Mount) Fsync(path string, datasync bool, fh uint64) int {
	return m.flush(fh)
}

// Flush uploads the handle's accumulated writes.
//
// cgofuse calls it on close() of a file descriptor even when the descriptor
// stays open elsewhere, so it is not the last chance -- but it is a chance, and
// it is cheap when the handle is clean.
func (m *Mount) Flush(path string, fh uint64) int {
	// A directory handle has no buffered bytes. The sentinel handle WinFsp uses
	// for the directory-open probe must be ignored rather than treated as an
	// unknown file handle, or a later write can be routed through the path
	// fallback before Windows has resolved the new directory.
	if fh == noHandle {
		return 0
	}
	return m.flush(fh)
}

// Release uploads the handle's writes and then forgets the handle.
//
// The handle is freed even when the upload fails. Leaving it in the table would
// leak it for the life of the mount, and the kernel has already finished with
// the descriptor, so a later write could arrive for a handle it believes is
// gone. The failure is still reported, because the content did not land.
func (m *Mount) Release(path string, fh uint64) int {
	code := m.flush(fh)

	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.files[fh]
	if !ok {
		// Already released, or never known. Idempotent, per cgofuse.
		return code
	}
	blocked := ""
	if !h.isClean() {
		blocked = h.path
	}
	m.closeHandle(fh)
	if code == 0 && blocked != "" {
		// The flush nominally succeeded but the handle was mutated while the
		// upload was in flight, so its content is not on the owner. Saying so is
		// the difference between a lost save and a reported one.
		return -EIO
	}
	return code
}

// ---------------------------------------------------------------- the namespace

// Mkdir creates a directory.
//
// The name must not already be there, and the answer for that is EEXIST whether
// the name is held by a file or by another directory: the caller's mistake is the
// same in both cases, and POSIX mkdir says EEXIST for both.
//
// A symlink at the name is also a refusal, as ELOOP rather than EEXIST. Both are
// non-zero; ELOOP is more informative, and it is the same answer the rest of the
// mount gives for a symlink.
func (m *Mount) Mkdir(path string, mode uint32) int {
	if m.offline() {
		return -EIO
	}
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	m.mu.Lock()
	_, alreadyPending := m.pendingPaths[cleaned]
	m.mu.Unlock()
	if alreadyPending {
		// The directory was created by this mount and has not been pushed back
		// by its owner yet. A second Mkdir for that name must behave like a
		// repeated create, not like a fresh lookup into a stale index.
		return -EEXIST
	}
	// The parent lookup enforces the shape: a missing parent is ENOENT and a
	// parent that is a file is ENOTDIR.
	if _, _, code := m.lookupParent(cleaned); code != 0 {
		return code
	}
	// The whole path is resolved to find out whether the name is taken.
	// lookupParent sees only the parent, so it cannot answer that.
	if _, code := m.lookup(cleaned); code == 0 {
		return -EEXIST
	} else if code == -ELOOP {
		return -ELOOP
	} else if code != -ENOENT {
		return code
	}
	if err := m.call().Mkdir(cleaned); err != nil {
		return errnoFor(err)
	}
	m.mu.Lock()
	m.pendingPaths[cleaned] = node.KindDir
	m.mu.Unlock()
	// A new name has appeared, and the kernel may have cached a negative lookup
	// for it. Invalidating the new path keeps Windows from rejecting the first
	// attribute query or child lookup with a stale "not found".
	if m.invalidatePath != nil {
		m.invalidatePath(cleaned)
	}
	return 0
}

// kindAt reports the kind of whatever is at a path, including a symlink.
//
// It exists because lookup refuses symlinks with ELOOP, and unlink and rename
// have to be able to see one: removing or moving a link is safe, and it is the
// only way to get rid of one through the mount. Without this, a symlink would be
// permanently undeletable, and every "rm link" would report a loop.
//
// A missing parent or a path deeper than an existing file is reported as absent,
// which is the same narrower-than-POSIX answer lookup gives and the same one the
// index gives.
func (m *Mount) kindAt(cleaned string) (node.Kind, bool) {
	if cleaned == "/" {
		return node.KindDir, true
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return 0, false
	}
	children, err := m.call().List(parent)
	if err != nil {
		return 0, false
	}
	if c, ok := childNamed(children, base); ok {
		return c.Kind, true
	}
	return 0, false
}

// dropHandlesFor forgets every handle on a path and releases its cached chunks.
//
// The caller must hold m.mu. It is what stops a later flush from recreating a
// name the user just removed.
func (m *Mount) dropHandlesFor(path string) {
	for fh, h := range m.files {
		if h.path == path {
			m.closeHandle(fh)
		}
	}
}

// Unlink removes a file or a symlink.
//
// A directory is refused: some programs call unlink on one, and success would
// make them believe it is gone. The mount reports EISDIR, which is also what the
// read path uses for a directory.
//
// A dirty open on the path is flushed first. The handle is the only copy of the
// content and the name is about to disappear, so removing it first would drop the
// write entirely.
func (m *Mount) Unlink(path string) int {
	if m.offline() {
		return -EIO
	}
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	if cleaned == "/" {
		return -EPERM
	}
	// The kind is read through kindAt rather than lookup, so that a symlink is
	// visible instead of being refused with ELOOP.
	kind, present := m.kindAt(cleaned)
	if !present {
		return -ENOENT
	}
	if kind == node.KindDir {
		return -EISDIR
	}
	if code := m.flushPath(cleaned); code != 0 {
		return code
	}
	if err := m.call().Remove(cleaned); err != nil {
		return errnoFor(err)
	}
	m.mu.Lock()
	m.dropHandlesFor(cleaned)
	m.mu.Unlock()
	return 0
}

// Rmdir removes an empty directory.
//
// The emptiness check is the index's, not the owner's, so a directory that grew
// on the owner since the last scan can be reported as empty and then refused by
// the owner. That refusal is passed through rather than flattened: the caller
// gets the truth from the layer that has it. The proactive check exists so the
// common case does not cost a round trip.
func (m *Mount) Rmdir(path string) int {
	if m.offline() {
		return -EIO
	}
	cleaned, err := Clean(path)
	if err != nil {
		return -EINVAL
	}
	// The root has no parent, so there is nothing to remove it from. This has to
	// come before the lookup, because lookup("/") succeeds by construction.
	if cleaned == "/" {
		return -EPERM
	}
	m.mu.Lock()
	_, pending := m.pendingPaths[cleaned]
	m.mu.Unlock()
	if pending {
		// The directory was created through this mount but has not reached the
		// owner's index yet. Removing it through the wire would be an ENOENT
		// even though the caller is removing the directory it just created.
		if err := m.call().Remove(cleaned); err != nil && errnoFor(err) != -ENOENT {
			return errnoFor(err)
		}
		m.mu.Lock()
		delete(m.pendingPaths, cleaned)
		m.mu.Unlock()
		return 0
	}
	n, code := m.lookup(cleaned)
	if code != 0 {
		return code
	}
	if n.Kind != node.KindDir {
		return -ENOTDIR
	}
	children, err := m.call().List(cleaned)
	if err != nil {
		return errnoFor(err)
	}
	if len(children) > 0 {
		return -ENOTEMPTY
	}
	if err := m.call().Remove(cleaned); err != nil {
		return errnoFor(err)
	}
	return 0
}

// Rename moves a name.
//
// The source's dirty handles are flushed before the owner is asked to move
// anything. This is the editor pattern -- write a temporary file, fsync it, rename
// it over the original, often without releasing it first -- and skipping the flush
// leaves the content in memory while the name moves, so the save appears to
// succeed and the file is unchanged (D10).
//
// Handles still open on the source follow the new name, and handles on the target
// are updated too, because the target's name now refers to the source's content:
// a handle that kept the old target bytes would write them back over the
// replacement on its next flush.
func (m *Mount) Rename(oldPath, newPath string) int {
	if m.offline() {
		return -EIO
	}
	oldClean, err := Clean(oldPath)
	if err != nil {
		return -EINVAL
	}
	newClean, err := Clean(newPath)
	if err != nil {
		return -EINVAL
	}
	// Neither the root nor a path onto the root has a meaning. The source check
	// cannot rely on lookup, which succeeds for the root by construction.
	if oldClean == "/" || newClean == "/" {
		return -EPERM
	}
	if oldClean == newClean {
		// A move onto itself. POSIX says this is a success and leaves the file
		// alone, so nothing is done and nothing is reported.
		return 0
	}
	// A name created through this mount may not be in the owner's index yet.
	// The editor's temp-file rename arrives in that window, and resolving the
	// source through the wire would report ENOENT even though the caller just
	// created it.
	m.mu.Lock()
	_, sourcePending := m.pendingPaths[oldClean]
	m.mu.Unlock()
	if !sourcePending {
		if _, present := m.kindAt(oldClean); !present {
			return -ENOENT
		}
	}
	if _, _, code := m.lookupParent(newClean); code != 0 {
		return code
	}
	// The source's content has to reach the owner before the name does. The
	// target is flushed as well: its bytes are about to be replaced, and a handle
	// still holding them would put them back.
	if code := m.flushPath(oldClean); code != 0 {
		return code
	}
	if code := m.flushPath(newClean); code != 0 {
		return code
	}
	if err := m.call().Rename(oldClean, newClean); err != nil {
		return errnoFor(err)
	}

	// Both paths' handles are updated in one lock acquisition, so a concurrent
	// reader never sees half of the move.
	//
	// A handle on the source keeps its buffer: it holds the very bytes the source
	// name now carries. A handle on the target loses its buffer, because the file
	// it described has just been replaced -- the target's own writes were flushed
	// above, so nothing is lost by discarding them locally, and keeping them would
	// put the old content back on the next flush. Its cached chunks go with the
	// buffer for the same reason: they are the replaced file's bytes.
	//
	// The target handle is left clean rather than marked dirty, so a later read
	// fetches the new file from the owner instead of materialising an empty
	// buffer.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.files {
		switch h.path {
		case oldClean:
			h.path = newClean
			m.cache.dropOpenLocked(h.fh)
		case newClean:
			h.path = newClean
			h.writes = map[int64][]byte{}
			h.size = 0
			h.contentSize = 0
			h.dirtyVersion = h.version
			h.dirty = false
			m.cache.dropOpenLocked(h.fh)
		}
	}
	delete(m.pendingPaths, oldClean)
	delete(m.pendingPaths, newClean)
	return 0
}
