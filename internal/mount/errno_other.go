//go:build !linux && !windows

package mount

import "syscall"

// On the other platforms cgofuse builds for, its cgo build takes these values
// from the C library, so the standard library's constants are the same numbers
// the library will pass to the kernel.
const (
	ENOSYS     = syscall.ENOSYS
	ENOTEMPTY  = syscall.ENOTEMPTY
	EOPNOTSUPP = syscall.EOPNOTSUPP
	ELOOP      = syscall.ELOOP
	ENOATTR    = syscall.ENOATTR
)
