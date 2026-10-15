// Package errtoken carries the error-category tokens that peerfold puts in
// front of an error message so a reader can classify it without reading the
// text.
//
// It exists as its own package because two layers need it and neither may
// import the other: the producing side is internal/client (and, for the
// not-empty case, internal/server), and the consuming side is internal/mount,
// which sits above them. If the tokens lived in internal/mount then
// internal/client would have to import the mount layer, which inverts the
// dependency and drags the FUSE adapter into every client build. The tokens are
// a tiny shared vocabulary, so they get a tiny shared package.
//
// Why tokens at all: the wire carries an error as free text (proto's Err field
// is a string), and the owner's error is operating-system-specific and
// localized — the same missing file reads "no such file or directory" on Linux
// and "The system cannot find the file specified" on Windows. A reader that
// matched that text would be unmaintable and wrong in every non-English locale.
package errtoken

import "fmt"

// The tokens. Each is the lowercased name of the errno it stands for, so the
// mapping back is mechanical, and each ends with the name of the condition
// rather than its cause. internal/mount matches these case-sensitively and only
// as a whole token: "enoentish: ..." and "ENOENT: ..." are both unclassified.
const (
	// NotFound is "this path does not exist".
	NotFound = "enoent"
	// NotDir is "a component of this path is not a directory".
	NotDir = "enotdir"
	// IsDir is "this path is a directory, and the operation wanted a file".
	IsDir = "eisdir"
	// NotEmpty is "this path already owns a name": either the directory still
	// has contents, or the name is taken. The two are one token because they
	// are the same test at the producing layer — see internal/client's
	// tokenForOSError for why os.ErrExist covers both on both platforms.
	NotEmpty = "enotempty"
	// Exists is "the name is taken" where the producer can tell that apart.
	Exists = "eexist"
	// Permission is "the operating system refused".
	Permission = "eacces"
	// CrossClient is "this rename would span two owners", which no single
	// filesystem operation can perform.
	CrossClient = "exdev"
	// Offline is "the connection this operation needed is gone".
	Offline = "eio"
)

// With prefixes an error with one of the tokens above, keeping the original
// text for a human reader.
//
// The original text is kept verbatim after the token because the CLI's output
// and its tests depend on it; the token is only a prefix. The %w verb is used
// rather than %s so that errors.Is still reaches the original error, which is
// what lets a caller keep using os.ErrNotExist while the mount layer reads the
// token.
//
// A nil error stays nil, so callers can write "return With(t, err)" without a
// guard.
func With(token string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", token, err)
}
