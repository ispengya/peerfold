package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ispengya/peerfold/internal/errtoken"
	"github.com/ispengya/peerfold/internal/node"
)

// FS performs the local filesystem operations the server routes to this
// client. It holds the share map, so every path it is asked about is resolved
// through a declared share rather than taken at face value.
type FS struct {
	shares Shares
}

// NewFS returns an FS over the given shares.
func NewFS(sh Shares) *FS { return &FS{shares: sh} }

// LocalPath resolves a share-relative path to an absolute path on this machine.
//
// It is the only place a share name and a relative path become a real path, and
// it refuses to produce one that escapes the share root. On refusal it returns
// an empty path as well as an error: a caller that received a path alongside
// the error would be free to use it, which is the whole thing being prevented.
//
// The empty relative path is refused here rather than by each operation, because
// it names the share root itself and the escape check does not catch it:
// filepath.Join(root, "") is root, and root is trivially under itself. Left
// accepted it made Remove("") unlink the whole share root and made
// replaceAtomically("") build its temporary file beside that root, in the
// share's parent directory, and write the payload there. Both were reachable
// from the wire, since "win/Documents" names a client and a share and no path
// inside it.
func (f *FS) LocalPath(share, rel string) (string, error) {
	root, ok := f.shares.Root(share)
	if !ok {
		return "", fmt.Errorf("client: no such share %q", share)
	}
	if rel == "" {
		// Naming the share itself is not a path inside it. Every caller here
		// means a file or a directory in the share, and the root is neither:
		// it is the thing they are relative to.
		return "", fmt.Errorf("client: path is empty: a share-relative path must name something inside share %q", share)
	}
	full := filepath.Join(root, rel)
	if !node.IsUnder(root, full) {
		// The relative path climbed out of the share. It is refused before
		// anything is opened, so no part of the escape is ever attempted.
		return "", fmt.Errorf("client: path %q escapes share %q", rel, share)
	}
	return full, nil
}

// ReadAt returns at most n bytes of a file starting at off, along with the
// file's metadata.
//
// A read that runs past the end returns the bytes that exist and no error, so
// "read the rest of the file" is expressible. A read that starts at or past the
// end returns no bytes and no error either: a zero-byte file is a file, and
// answering it with io.EOF would make it indistinguishable from a missing one
// at every layer above this. A read that starts past the end of the file is the
// same nothing, and the caller asks for a range, not for a diagnosis. n < 0
// means "to the end of the file".
func (f *FS) ReadAt(share, rel string, off int64, n int) ([]byte, os.FileInfo, error) {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return nil, nil, err
	}
	if off < 0 {
		return nil, nil, fmt.Errorf("client: negative offset %d", off)
	}

	fh, err := os.Open(full)
	if err != nil {
		// os.Open's error already satisfies os.ErrNotExist for a missing
		// path, so it is passed through to the token classifier rather than
		// flattened into a string: the classifier keeps that property and adds
		// the category the mount layer reads.
		return nil, nil, tokenForOSError(err)
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		return nil, nil, tokenForOSError(err)
	}
	if info.IsDir() {
		// Reading a directory is not "a file with no bytes"; it is a
		// different operation, and calling it empty would hide the mistake.
		return nil, nil, fmt.Errorf("client: %q is a directory, not a file", rel)
	}

	size := info.Size()
	if off >= size {
		// Nothing left to read. This is not a failure: a zero-byte file has
		// nothing to read at offset zero, and reporting io.EOF here would make
		// an empty file a read error at every layer above. The metadata still
		// comes back, so a caller can learn the size from a read that returned
		// no bytes. It is returned as an empty, non-nil slice so the caller can
		// tell "read succeeded, nothing there" from "no data at all".
		return []byte{}, info, nil
	}

	// Clamp the request to what exists: a short read at the end is a normal
	// result, not an error.
	want := size - off
	if n >= 0 && int64(n) < want {
		want = int64(n)
	}
	buf := make([]byte, want)
	got, err := fh.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	// ReadAt returns io.EOF when it filled the buffer exactly to the end;
	// the bytes are still valid, and the short-read semantics above mean EOF
	// is not reported here.
	return buf[:got], info, nil
}

// Stat returns a path's metadata without reading it, so a caller can tell a
// directory from a file before deciding what to do.
func (f *FS) Stat(share, rel string) (os.FileInfo, error) {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return nil, err
	}
	return os.Stat(full)
}

// tempSuffix marks the file a write goes through before it reaches its name.
const tempSuffix = ".peerfold.tmp"

// WriteAll replaces the file at rel with everything r produces, and returns how
// many bytes were written.
//
// The write goes to a temporary file in the target's own directory and is moved
// onto the name only once it is complete and flushed. The order matters and is
// not interchangeable:
//
//  1. write the whole reader to the temporary file,
//  2. Sync when the caller asked for durability — after this the bytes are on
//     the medium, not merely in a buffer,
//  3. close it,
//  4. rename it onto the target.
//
// A rename within one directory is atomic, so a reader of the target sees either
// the old file or the whole new one. That is what makes "a successful return
// means the file is saved" true, and what keeps a failure from truncating the
// file that was already there: up to step 4 the target has not been touched.
//
// Without durability the bytes may still be in the page cache when the rename
// lands; a crash can then leave the new name pointing at a file whose content is
// incomplete. That is the trade an ordinary close makes, and an explicit fsync
// is what buys it back.
//
// A partial file at the real path would be worse than no write at all, because
// every later reader would take it for the whole file.
func (f *FS) WriteAll(share, rel string, r io.Reader) (int64, error) {
	var n int64
	sink := func(w io.Writer) error {
		written, err := io.Copy(w, r)
		n = written
		return err
	}
	return n, f.replaceAtomically(share, rel, sink, nil, true)
}

// replaceAtomically writes the file at rel by whatever sink does, through a
// temporary file that is moved onto the name only when the whole write is known
// to have succeeded.
//
// This is the shape every content write uses, including the one whose bytes
// arrive a frame at a time from the network. A sink that took an io.Reader would
// force the network case to collect a whole file in memory first, which is the
// one thing the streamed protocol exists to avoid.
//
// verify, when it is not nil, runs after the bytes have been written and flushed
// but before the rename, and a failure from it abandons the write. It exists
// because "the bytes arrived" and "the bytes are all of them" are different
// questions, and only the caller can answer the second. A stream that stopped
// part-way produces a file that looks complete, so a design that renamed first
// and asked afterwards would have already published a truncated file — and would
// then have to delete it, which is not the same operation at all: deleting can
// fail, and what it deletes may be something else by then.
func (f *FS) replaceAtomically(share, rel string, sink func(io.Writer) error, verify func() error, durable bool) error {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+tempSuffix)

	fh, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	// Every failure from here on has to remove the temporary file: a stray
	// ".name.peerfold.tmp" would be indexed as a file of its own on the next
	// scan, and the owner never asked for it.
	if err := sink(fh); err != nil {
		_ = fh.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("client: writing %q: %w", rel, err)
	}
	if verify != nil {
		if err := verify(); err != nil {
			_ = fh.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("client: writing %q: %w", rel, err)
		}
	}
	// Sync before the rename when durability was asked for: a rename that lands
	// while the bytes are still in a buffer would report a save that a power
	// loss can turn into a truncated file. An ordinary close does not ask for
	// it, and skipping two barriers per file is what makes a build of thousands
	// of small files bearable on Windows.
	if durable {
		if err := fh.Sync(); err != nil {
			_ = fh.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("client: syncing %q: %w", rel, err)
		}
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("client: closing %q: %w", rel, err)
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("client: replacing %q: %w", rel, err)
	}
	if durable {
		f.syncDir(filepath.Dir(full))
	}
	return nil
}

// syncDir flushes a directory entry to the medium, best effort.
//
// Syncing the file's contents is not enough to make the rename durable: the
// bytes can be on the medium while the directory entry still names the old file,
// so a crash immediately after a successful put would lose it. The rename itself
// has to be flushed, and that is done by syncing the directory that holds it.
//
// Failure is deliberately not reported. The content is already durable and the
// rename has already succeeded, so there is nothing to roll back and nothing the
// caller could do differently; a directory that cannot be opened or synced (some
// filesystems do not permit it) is not a reason to report a write as failed. The
// cost of being wrong is a lost name after a power cut, against a successful
// write reported as an error, which is the worse of the two.
func (f *FS) syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Create makes a new empty file, and refuses if the path is already taken.
func (f *FS) Create(share, rel string) error {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		// O_EXCL is what makes this the "already exists" case, so the
		// classifier sees os.ErrExist and the mount layer learns ENOTEMPTY
		// rather than having to guess from the OS's own wording.
		return tokenForOSError(err)
	}
	return fh.Close()
}

// CreateFrom makes a new file and fills it with whatever sink produces, refusing
// if the name is already taken.
//
// The name is taken exclusively first, so an existing file is refused before a
// single byte is written. If the write then fails the half-made file is removed:
// leaving it would present a truncated file as a complete one. durable asks for
// the same stable-storage flush WriteAll's replaceAtomically does.
func (f *FS) CreateFrom(share, rel string, sink func(io.Writer) error, durable bool) error {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return err
	}
	fh, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := sink(fh); err != nil {
		_ = fh.Close()
		_ = os.Remove(full)
		return fmt.Errorf("client: creating %q: %w", rel, err)
	}
	if durable {
		if err := fh.Sync(); err != nil {
			_ = fh.Close()
			_ = os.Remove(full)
			return fmt.Errorf("client: syncing %q: %w", rel, err)
		}
	}
	if err := fh.Close(); err != nil {
		_ = os.Remove(full)
		return fmt.Errorf("client: closing %q: %w", rel, err)
	}
	return nil
}

// removeIfPresent deletes a path if it exists, and reports nothing if it does
// not.
//
// It is used to undo a create whose content did not arrive whole. The name is
// known to have been free when it was taken, so removing it cannot destroy
// anything the owner had.
func (f *FS) removeIfPresent(share, rel string) {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return
	}
	_ = os.Remove(full)
}

// tokenForOSError classifies an operating-system error into one of the
// categories the mount layer distinguishes, and prefixes it with the stable
// token for that category. An error it cannot classify is returned unchanged,
// because errnoFor already turns an unrecognised error into EIO — and EIO is
// the honest answer for a failure whose category is not known.
//
// This is the producing side of the token convention; internal/mount/errno.go
// is the reading side. It lives here, in the filesystem layer, because this is
// the nearest point at which the category is still knowable. Above it the error
// becomes a string on the wire, and the text of that string is localized: the
// same missing file reads "no such file or directory" on Linux and "The system
// cannot find the file specified" on Windows.
//
// The classification uses Go's portable error sentinels rather than errno
// numbers, and the reason is measured rather than assumed. On Windows the raw
// errno for a non-empty directory is 145 and for an existing name is 183, and
// Go maps both of them onto os.ErrExist; on Linux they are ENOTEMPTY (39) and
// EEXIST (17), which Go also maps onto os.ErrExist. So os.ErrExist means "a
// name already owns this path", which covers both "already exists" and "is not
// empty", and it is the same test on both platforms. syscall.ENOTEMPTY and
// syscall.EEXIST are deliberately not used to tell those apart: on Windows they
// hold unix-style values that no real Windows errno equals, so neither ever
// matches.
//
// errnoFor's token table has one category for "not empty" and none for "already
// exists" on its own, so an ErrExist is reported as ENOTEMPTY. That is the
// safer of the two: telling a caller "this directory still has contents" when
// the truth was "this name is taken" makes it stop and look, while the reverse
// would invite it to replace something.
func tokenForOSError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrPermission):
		return errtoken.With(errtoken.Permission, err)
	case errors.Is(err, os.ErrNotExist):
		return errtoken.With(errtoken.NotFound, err)
	case errors.Is(err, os.ErrExist):
		return errtoken.With(errtoken.NotEmpty, err)
	default:
		return err
	}
}

// Mkdir makes a new directory, and refuses if the name is already taken.
//
// Mkdir rather than MkdirAll: creating the parents as a side effect would let a
// mistyped path quietly build a tree at a place the caller never named.
func (f *FS) Mkdir(share, rel string) error {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return err
	}
	return tokenForOSError(os.Mkdir(full, 0o755))
}

// Rename moves a path within one share.
//
// Both ends are resolved through LocalPath, so a rename cannot be used to reach
// outside the share it was given, at either end.
func (f *FS) Rename(share, oldRel, newRel string) error {
	oldFull, err := f.LocalPath(share, oldRel)
	if err != nil {
		return err
	}
	newFull, err := f.LocalPath(share, newRel)
	if err != nil {
		return err
	}
	return tokenForOSError(os.Rename(oldFull, newFull))
}

// Remove deletes one file or one empty directory.
//
// It is deliberately not recursive. A single wrong path would otherwise destroy
// a whole tree, and there is no way to ask for that here.
func (f *FS) Remove(share, rel string) error {
	full, err := f.LocalPath(share, rel)
	if err != nil {
		return err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return tokenForOSError(err)
	}
	if info.IsDir() {
		// os.Remove refuses a non-empty directory on its own, and this keeps
		// that behaviour explicit rather than relying on it silently.
		entries, err := os.ReadDir(full)
		if err != nil {
			return tokenForOSError(err)
		}
		if len(entries) > 0 {
			// The category is known here without asking the operating system,
			// so the token is applied directly rather than through the
			// classifier. It is the same token the classifier derives from
			// os.ErrExist, which is what os.Remove reports for this case on
			// both platforms.
			return errtoken.With(errtoken.NotEmpty,
				fmt.Errorf("client: %q is not empty: refusing to remove a directory with %d entry(ies)", rel, len(entries)))
		}
	}
	return tokenForOSError(os.Remove(full))
}
