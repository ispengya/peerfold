package mount

import (
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

// TestErrnosMatchCgofuse pins this package's errno table against the library's.
//
// The core keeps its own copies of these numbers so that it can name the error
// without importing the library everywhere, and that duplication is a real
// hazard rather than a tidiness question: every one of these values is handed
// back to the kernel through cgofuse, so a number that agrees with uname(1) but
// not with the library is simply the wrong error.
//
// The comparison is against cgofuse's own constants rather than against
// measured literals. The cgo build takes several of them from the C library and
// the pure-Go Windows build declares its own table, so the two do not agree
// with each other; the core's constants are selected per platform to match
// whichever build is in use. A cgofuse upgrade that moved a number would fail
// here instead of silently changing what the mount reports.
func TestErrnosMatchCgofuse(t *testing.T) {
	cases := []struct {
		name string
		ours int
		lib  int
	}{
		{"EPERM", EPERM, fuse.EPERM},
		{"ENOENT", ENOENT, fuse.ENOENT},
		{"EIO", EIO, fuse.EIO},
		{"EBADF", EBADF, fuse.EBADF},
		{"EACCES", EACCES, fuse.EACCES},
		{"EEXIST", EEXIST, fuse.EEXIST},
		{"EXDEV", EXDEV, fuse.EXDEV},
		{"ENOTDIR", ENOTDIR, fuse.ENOTDIR},
		{"EISDIR", EISDIR, fuse.EISDIR},
		{"EINVAL", EINVAL, fuse.EINVAL},
		{"ENOSPC", ENOSPC, fuse.ENOSPC},
		{"ENOSYS", ENOSYS, fuse.ENOSYS},
		{"ENOTEMPTY", ENOTEMPTY, fuse.ENOTEMPTY},
		{"EOPNOTSUPP", EOPNOTSUPP, fuse.EOPNOTSUPP},
		{"ELOOP", ELOOP, fuse.ELOOP},
		{"ENOATTR", ENOATTR, fuse.ENOATTR},
	}
	for _, c := range cases {
		if c.ours != c.lib {
			t.Errorf("%s = %d here, %d in cgofuse", c.name, c.ours, c.lib)
		}
	}
}
