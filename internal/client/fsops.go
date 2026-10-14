package client

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"peerfold/internal/node"
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
func (f *FS) LocalPath(share, rel string) (string, error) {
	root, ok := f.shares.Root(share)
	if !ok {
		return "", fmt.Errorf("client: no such share %q", share)
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
// end returns nothing and io.EOF. n < 0 means "to the end of the file".
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
		// path, so it is passed through rather than flattened into a string.
		return nil, nil, err
	}
	defer fh.Close()

	info, err := fh.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		// Reading a directory is not "a file with no bytes"; it is a
		// different operation, and calling it empty would hide the mistake.
		return nil, nil, fmt.Errorf("client: %q is a directory, not a file", rel)
	}

	size := info.Size()
	if off >= size {
		// Nothing left to read. The metadata still comes back, so a caller
		// can learn the size from a read that returned no bytes.
		return nil, info, io.EOF
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
