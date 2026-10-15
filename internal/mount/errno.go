package mount

import (
	"errors"
	"strings"

	"peerfold/internal/errtoken"
	"peerfold/internal/viewer"
)

// The errno values cgofuse uses on both platforms. They are declared here
// rather than taken from syscall, because syscall's values are
// platform-specific and the core must build everywhere.
//
// "Both platforms" is not a figure of speech: cgofuse ships its own table, and
// it is the same one on Windows and on Linux. The cgo build redefines errno in
// C to these numbers and the pure-Go Windows build declares them in Go, so
// ENOSYS is 40 on both, not the 38 an uname(1) would suggest. The values were
// read out of the library (fsop_cgo.go and fsop_nocgo_windows.go) and are pinned
// there by TestErrnosMatchCgofuse, because every one of these numbers is handed
// back to the kernel through cgofuse and a wrong one is a wrong error.
//
// EPERM=1 ENOENT=2 EIO=5 EBADF=9 EACCES=13 EEXIST=17 EXDEV=18 ENOTDIR=20
// EISDIR=21 EINVAL=22 ENOSPC=28 ENOSYS=40 ENOTEMPTY=41 ENOATTR=120 ELOOP=114.
const (
	EPERM     = 1
	ENOENT    = 2
	EIO       = 5
	EBADF     = 9
	EACCES    = 13
	EEXIST    = 17
	EXDEV     = 18
	ENOTDIR   = 20
	EISDIR    = 21
	EINVAL    = 22
	ENOSPC    = 28
	ENOSYS    = 40
	ENOTEMPTY = 41
	ELOOP     = 114
	// ENOATTR is what a getxattr for a missing attribute must return. It is
	// cgofuse's name for ENODATA on every platform it builds for, and 120 is its
	// value on both -- the same 120 that a Linux kernel would call something else
	// entirely, which is why this number is copied from the library rather than
	// from experience.
	ENOATTR = 120
)

// Error tokens come from internal/errtoken, which both this package and the
// producing side (internal/client) import. The aliases below keep the reading
// code in this file terse without giving the tokens a second definition.
const (
	tokenNotFound    = errtoken.NotFound
	tokenNotDir      = errtoken.NotDir
	tokenIsDir       = errtoken.IsDir
	tokenNotEmpty    = errtoken.NotEmpty
	tokenExists      = errtoken.Exists
	tokenPermission  = errtoken.Permission
	tokenCrossClient = errtoken.CrossClient
	tokenOffline     = errtoken.Offline
)

// withToken is the producing-side helper, re-exported under the name the tests
// in this package use. It is the same function the client calls.
var withToken = errtoken.With

// errnoFor is the only place in the mount layer that converts an error into an
// errno. An error it does not recognise becomes EIO, which is the conservative
// direction: reporting a failure is better than reporting a success the caller
// will act on.
func errnoFor(err error) int {
	if err == nil {
		return 0
	}
	// Sentinels first, so a wrapped sentinel works without any text at all.
	switch {
	case errors.Is(err, errNotFound):
		return -ENOENT
	case errors.Is(err, errNotDir):
		return -ENOTDIR
	case errors.Is(err, errIsDir):
		return -EISDIR
	case errors.Is(err, errNotEmpty):
		return -ENOTEMPTY
	case errors.Is(err, errExists):
		return -EEXIST
	case errors.Is(err, errPermission):
		return -EACCES
	case errors.Is(err, errCrossClient):
		return -EXDEV
	case errors.Is(err, errOffline):
		return -EIO
	// The viewer decides a cross-client rename and refuses it before any frame
	// is sent, so there is no wire for a token to travel on and errors.Is is
	// the only way to recognise it. That works because the viewer wraps the
	// sentinel rather than formatting it — which is a property worth pinning,
	// so there is a test that fails if the wrapping ever goes away.
	case errors.Is(err, viewer.ErrCrossClient):
		return -EXDEV
	}
	if _, code, ok := tokenOf(err.Error()); ok {
		return code
	}
	return -EIO
}

// allTokens is every token, paired with the errno it maps to. It is a list
// rather than a map because a list is what the prefix scan needs, and because
// this way errnoForToken's mapping has exactly one definition.
var allTokens = []struct {
	token string
	errno int
}{
	{tokenNotFound, -ENOENT},
	{tokenNotDir, -ENOTDIR},
	{tokenIsDir, -EISDIR},
	{tokenNotEmpty, -ENOTEMPTY},
	{tokenExists, -EEXIST},
	{tokenPermission, -EACCES},
	{tokenCrossClient, -EXDEV},
	{tokenOffline, -EIO},
}

// tokenOf finds the first "<token>: " in a message and returns the token and
// its errno.
//
// It matches against the known tokens rather than taking the text before the
// first colon, and it scans rather than only looking at position zero. Both
// matter, and the reason is what actually travels on the wire. The producers
// wrap: the client's read path sends
//
//	client: reading "a.txt": enoent: open a.txt: The system cannot find the file specified
//
// so the token is neither at position zero nor the text before the first colon.
// Taking the first colon would read "client" and lose the enoent underneath it,
// and every error with a context prefix would degrade to EIO — making a real
// missing file indistinguishable from an I/O fault, which is the one
// distinction this convention exists to make.
//
// Matching only known tokens is what keeps the scan safe. A file may be called
// "enoent", but a path in an error message is quoted or namespaced, and the
// separator is required, so the filename only matches if it appears as exactly
// "enoent: " — and if it does, the owner's own error would have to be
// unclassified anyway.
func tokenOf(text string) (string, int, bool) {
	for rest := text; ; {
		i := strings.Index(rest, ": ")
		if i < 0 {
			return "", 0, false
		}
		head := rest[:i]
		for _, t := range allTokens {
			// The token must be the whole word before the separator, so
			// "theenoent: x" is not a match while "enoent: x" is.
			if head == t.token {
				return t.token, t.errno, true
			}
			if strings.HasSuffix(head, " "+t.token) || strings.HasSuffix(head, "\t"+t.token) {
				return t.token, t.errno, true
			}
		}
		rest = rest[i+2:]
	}
}

// errnoForToken maps one token to its errno, purely for the tests that want to
// check the table itself rather than a message. An unknown or empty token
// becomes EIO, which is the conservative answer.
func errnoForToken(token string) int {
	for _, t := range allTokens {
		if t.token == token {
			return t.errno
		}
	}
	return -EIO
}

// Sentinel errors. These are used in tests and wherever an error never reaches
// the wire; on the wire the token is what carries the category. Each one is
// built from its own token so that errnoFor's sentinel branch and its token
// branch can never disagree about the same condition.
var (
	errNotFound    = errors.New(tokenNotFound + ": not found")
	errNotDir      = errors.New(tokenNotDir + ": not a directory")
	errIsDir       = errors.New(tokenIsDir + ": is a directory")
	errNotEmpty    = errors.New(tokenNotEmpty + ": directory is not empty")
	errExists      = errors.New(tokenExists + ": name is taken")
	errPermission  = errors.New(tokenPermission + ": permission denied")
	errCrossClient = errors.New(tokenCrossClient + ": cross-client rename")
	errOffline     = errors.New(tokenOffline + ": the connection is gone")
)
