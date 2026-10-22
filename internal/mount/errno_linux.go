//go:build linux

package mount

// The values cgofuse's cgo build takes from the C library on Linux. They are
// the kernel's own errno numbers, which is why they differ from the pure-Go
// Windows table.
const (
	ENOSYS     = 38
	ENOTEMPTY  = 39
	EOPNOTSUPP = 95
	ELOOP      = 40
	// ENOATTR is cgofuse's spelling of ENODATA on Linux; the kernel has no
	// ENOATTR of its own.
	ENOATTR = 61
)
