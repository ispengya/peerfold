package mount

import (
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

// TestErrnosMatchCgofuse pins this package's errno table against the library's.
//
// The core keeps its own copies of these numbers so that it builds without a FUSE
// library, and that duplication is a real hazard rather than a tidiness question:
// every one of these values is handed back to the kernel through cgofuse, so a
// number that agrees with uname(1) but not with the library is simply the wrong
// error. ENOSYS was 38 here for three tasks and is 40 in cgofuse -- 38 would have
// told a Linux kernel ENAMETOOLONG.
//
// The comparison is against cgofuse's own constants rather than against measured
// literals, so it holds on both of cgofuse's builds: the cgo one redefines errno
// in C and the pure-Go Windows one declares them in Go, and they agree with each
// other. A cgofuse upgrade that moved a number would fail here instead of
// silently changing what the mount reports.
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
		{"ELOOP", ELOOP, fuse.ELOOP},
		{"ENOATTR", ENOATTR, fuse.ENOATTR},
	}
	for _, c := range cases {
		if c.ours != c.lib {
			t.Errorf("%s = %d here, %d in cgofuse", c.name, c.ours, c.lib)
		}
	}
}
