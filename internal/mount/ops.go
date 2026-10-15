package mount

import (
	stdpath "path"
	"sort"

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
	m.mu.Lock()
	if h, ok := m.handle(fh); ok && h.dirty {
		n.Size = h.size
	}
	m.mu.Unlock()
	*st = statFor(n)
	return 0
}

// Readdir lists one directory.
//
// The two dot entries are synthesised here because the caller's List returns real
// entries only. Their stat is nil, which is what cgofuse's own examples pass and
// what the host adapter turns into a NULL stat pointer, saving a Getattr per
// entry.
//
// They are named "./" and "../" rather than "." and "..". The FUSE protocol
// carries no distinction -- the name is a name -- but the convention in this
// library is the slashed form, and matching it keeps the mount's listings
// byte-identical to the ones its own examples produce.
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
	children, err := m.caller.List(path)
	if err != nil {
		return errnoFor(err)
	}
	// Sorted so a listing is stable between reads. The index's own order is the
	// server's business and may change without the tree changing, which would
	// make a directory appear to shuffle for no reason the user can see.
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })

	if !fill("./", nil, 0) {
		return 0
	}
	if !fill("../", nil, 0) {
		return 0
	}
	for _, c := range children {
		st := statFor(c)
		if c.Kind == node.KindSymlink {
			// Visible, but flagged. The mount layer refuses to open it and a
			// caller that stats it gets ELOOP, so a listing that passed it off as
			// a regular file with a plausible size would be lying twice.
			st.Mode |= modeSymlink
		}
		if !fill(stdpath.Base(c.Path), &st, 0) {
			// The callback returns false when the kernel's buffer is full. That
			// is not an error and the rest of the listing is not lost: the kernel
			// asks again.
			return 0
		}
	}
	return 0
}

// Statfs reports invented figures for the whole federation.
//
// The path is still resolved, so a tool asking about a directory that does not
// exist is told ENOENT rather than handed numbers. The numbers themselves do not
// depend on the path: they describe one federation, and every point in the mount
// sees the same one.
func (m *Mount) Statfs(path string, st *Statfs) int {
	if _, code := m.lookup(path); code != 0 {
		return code
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
