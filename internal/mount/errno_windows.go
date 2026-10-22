//go:build windows

package mount

// The values cgofuse's pure-Go Windows build declares. Windows has no POSIX
// errno table of its own here; the library's constants are what WinFsp maps
// back to NTSTATUS, so they are the numbers this core must return.
const (
	ENOSYS     = 40
	ENOTEMPTY  = 41
	EOPNOTSUPP = 130
	ELOOP      = 114
	ENOATTR    = 120
)
