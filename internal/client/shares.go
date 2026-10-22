// Package client holds the supplying side of peerfold: what it shares, how it
// scans that into tree nodes, and the local filesystem operations the server
// routes to it.
package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ispengya/peerfold/internal/node"
)

// Shares is the mapping from share names to absolute directories, plus the
// absolute paths whose subtrees must stay out of the index.
//
// The exclusion exists for the observer's own mountpoint: if the aggregated
// tree is mounted inside a shared directory, that subtree feeds back into the
// index. Keeping it out is what stops the recursion.
type Shares struct {
	roots    map[string]string
	names    []string
	excluded []string
}

// NewShares parses share specs of the form "NAME=/absolute/path".
//
// Every root must be an existing directory, every name must be non-empty and
// unique, and the exclude list is resolved to absolute, cleaned paths. Paths in
// exclude do not have to exist yet: the mountpoint may be created later.
func NewShares(pairs []string, exclude []string) (Shares, error) {
	sh := Shares{roots: make(map[string]string, len(pairs))}
	for _, pair := range pairs {
		name, root, ok := strings.Cut(pair, "=")
		if !ok {
			return Shares{}, fmt.Errorf("client: share %q is not of the form NAME=/path", pair)
		}
		if name == "" {
			return Shares{}, fmt.Errorf("client: share %q has an empty name", pair)
		}
		if !validName(name) {
			// The name becomes the first component of every indexed path, so
			// a separator or a ".." in it would put a path into the index that
			// no longer means what it says.
			return Shares{}, fmt.Errorf("client: share name %q must not be %q or contain %q or %q", name, ".", "/", `\`)
		}
		if root == "" {
			return Shares{}, fmt.Errorf("client: share %q has an empty path", pair)
		}
		if _, dup := sh.roots[name]; dup {
			return Shares{}, fmt.Errorf("client: share name %q is declared twice", name)
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			return Shares{}, fmt.Errorf("client: share %q: resolve %q: %w", name, root, err)
		}
		abs = filepath.Clean(abs)
		info, err := os.Lstat(abs)
		if err != nil {
			return Shares{}, fmt.Errorf("client: share %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Scan walks with WalkDir, which does not follow a symlinked root.
			// Accepting one would produce a share header with no contents.
			return Shares{}, fmt.Errorf("client: share %q: %q is a symlink, not a directory", name, abs)
		}
		if !info.IsDir() {
			return Shares{}, fmt.Errorf("client: share %q: %q is not a directory", name, abs)
		}
		sh.roots[name] = abs
		sh.names = append(sh.names, name)
	}
	sort.Strings(sh.names)

	for _, e := range exclude {
		abs, err := filepath.Abs(e)
		if err != nil {
			return Shares{}, fmt.Errorf("client: exclude %q: %w", e, err)
		}
		sh.excluded = append(sh.excluded, filepath.Clean(abs))
	}
	return sh, nil
}

// Root returns the absolute directory behind a share name.
func (s Shares) Root(name string) (string, bool) {
	root, ok := s.roots[name]
	return root, ok
}

// Names returns the declared share names in ascending order.
func (s Shares) Names() []string {
	return append([]string(nil), s.names...)
}

// Excluded reports whether abs is one of the excluded subtrees, or lives inside
// one. The second case matters: an exclusion that only matched its own root
// would still index everything under it.
func (s Shares) Excluded(abs string) bool {
	for _, e := range s.excluded {
		if node.IsUnder(e, abs) {
			return true
		}
	}
	return false
}

// validName reports whether a share name may become the root of the indexed
// paths. It must be a single ordinary path component: no separators (which
// would make it look like a nested path), and not "." or "..".
func validName(name string) bool {
	// One definition, shared with the server, which has to make the same
	// decision about a name arriving on the wire.
	return node.ValidName(name)
}

// DefaultBatch is the batch size used when a caller passes a non-positive one.
const DefaultBatch = 500

// classify decides what a walked entry is, from the FileInfo the walk reported
// and whether the DirEntry called itself a directory.
//
// Two Windows cases make this more than a two-way choice, and both used to be
// indexed as a regular file with a meaningless size:
//
//   - A symbolic link. d.Info() reports the link itself, so the symlink bit is
//     set. The owner's read opens the path and follows the link, so recording
//     it as a file made a stat say 9 bytes and a read return the target — and a
//     read-modify-write through a mount overwrote the target file.
//   - A directory junction or other reparse point. Go reports these with
//     ModeIrregular set and neither the symlink bit nor the dir bit, and
//     DirEntry.IsDir() also says false, because a junction is not a directory
//     to Lstat. Junctions are far more common than symlinks on Windows —
//     OneDrive, node_modules, "C:\Users\All Users" — so this is the case that
//     bites most often. It is treated as a link because that is the safe
//     answer: a junction points somewhere, and the mount layer refuses links.
//
// A name that is neither a directory, a link, nor a regular file (a socket, a
// device, a pipe) is reported as KindSymlink rather than KindFile, so that the
// mount layer refuses it. Indexing it as a file would promise a size and a
// content that do not exist.
func classify(mode os.FileMode, isDir bool) node.Kind {
	switch {
	case mode&os.ModeSymlink != 0:
		return node.KindSymlink
	case isDir:
		return node.KindDir
	case mode&os.ModeType != 0:
		// ModeIrregular, ModeNamedPipe, ModeSocket, ModeDevice, ModeCharDevice.
		return node.KindSymlink
	default:
		return node.KindFile
	}
}

// Scan walks every share and hands the nodes to emit in batches of at most
// batch entries.
//
// Each share contributes a header node for its own root before its contents.
// Nothing is filtered by kind, name, or size: an empty directory and a
// zero-byte file are entries like any other. An excluded subtree is skipped
// whole, via filepath.SkipDir, so its contents are never visited. An error from
// emit stops the walk and is returned unchanged, so a caller that aborts the
// scan does not cause the walk to look like it failed on its own.
func Scan(sh Shares, batch int, emit func([]node.Node) error) error {
	if batch <= 0 {
		batch = DefaultBatch
	}

	// pending accumulates nodes until a batch is full. It is reset by slicing
	// to zero, and because it is flushed before returning, no caller sees a
	// shared backing array.
	var pending []node.Node
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		// Copy: the caller may keep the slice, and pending's array is reused.
		out := append([]node.Node(nil), pending...)
		pending = pending[:0]
		return emit(out)
	}
	add := func(n node.Node) error {
		pending = append(pending, n)
		if len(pending) >= batch {
			return flush()
		}
		return nil
	}

	for _, name := range sh.names {
		root := sh.roots[name]
		rootInfo, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("client: share %q: %w", name, err)
		}
		header := node.Node{
			Path:  name,
			Kind:  node.KindDir,
			Mode:  uint32(rootInfo.Mode()),
			Mtime: rootInfo.ModTime().UnixNano(),
		}
		if err := add(header); err != nil {
			return err
		}
		// The share root is not walked when it is itself excluded: nothing
		// under it may be indexed.
		if sh.Excluded(root) {
			continue
		}

		err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root {
				// Already emitted as the header, and dirEntry ordering means
				// this is the first callback.
				return nil
			}
			if sh.Excluded(path) {
				// A directory that is excluded must not be descended into; a
				// file cannot be skipped, so just drop it.
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			n := node.Node{
				Path:  name + "/" + filepath.ToSlash(rel),
				Mode:  uint32(info.Mode()),
				Mtime: info.ModTime().UnixNano(),
			}
			n.Kind = classify(info.Mode(), d.IsDir())
			if n.Kind == node.KindFile {
				n.Size = info.Size()
			}
			return add(n)
		})
		if err != nil {
			return err
		}
	}
	return flush()
}
