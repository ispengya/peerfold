// Package node holds peerfold's file model and the path helpers that go with
// it. The index, the wire protocol, and the client's scanner all agree on this
// model.
package node

import (
	"path/filepath"
	"strings"
)

// Kind classifies a node. The values are part of the wire protocol: they travel
// inside proto.NodeJSON.Kind, so they must not be renumbered.
type Kind uint8

const (
	// KindFile is a regular file.
	KindFile Kind = 0
	// KindDir is a directory.
	KindDir Kind = 1
	// KindSymlink is a symbolic link.
	//
	// The scanner used to record a link as KindFile with Size set to the
	// length of its target string, because it took only the permission bits
	// from the owner's FileInfo and decided the kind with IsDir. That made a
	// stat and a read disagree — the owner's read follows the link — so a
	// read-modify-write through a mount overwrote the target file.
	//
	// The mount layer refuses a link with ELOOP. It can only do that if the
	// index says what the entry is, which is what this kind is for.
	//
	// 2 was free: File and Dir are 0 and 1, Deleted is 255 so that it cannot
	// collide with a real kind.
	KindSymlink Kind = 2
	// KindDeleted is not a kind a filesystem can report. A node carrying it is
	// an instruction: the path no longer exists, so the index must drop it (and
	// its subtree). The scanner never produces it; the watcher and the index
	// do. It is 255 so it cannot collide with a real kind.
	KindDeleted Kind = 255
)

// Node is one entry of the aggregated tree.
//
// Path is the share-relative, slash-separated path of the form
// "<share>/<rel>", where the share root itself is just "<share>". It is
// case-sensitive and is not normalized: two paths that differ only in case are
// two different nodes.
//
// Mtime is Unix nanoseconds. Size is the file's length in bytes and is 0 for
// directories and for zero-byte files.
type Node struct {
	Path  string
	Kind  Kind
	Size  int64
	Mode  uint32
	Mtime int64
}

// SplitFirst splits a slash-separated path into its first element and the rest.
//
//	SplitFirst("Documents/a/b.txt") == ("Documents", "a/b.txt")
//	SplitFirst("Documents")         == ("Documents", "")
//
// The caller is expected to have a non-empty path; an empty one splits into
// empty parts.
func SplitFirst(p string) (head, rest string) {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i+1:]
		}
	}
	return p, ""
}

// ValidName reports whether name may name a client or a share.
//
// A name becomes a component of every path in its namespace: a client name is
// the first component of the aggregated tree, and a share name is the first
// component the client indexes. A separator or a dot segment in either would
// make those paths mean something else, so the shape is checked once and shared
// by every side of the protocol.
func ValidName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// ValidRelPath reports whether p is a share-relative, slash-separated path:
// non-empty, not absolute, and made only of ordinary components.
//
// It is the wire's path shape, not a filesystem path. The index, the client
// that produced the path, and the server that accepts it all have to agree on
// it, or a path that the index silently drops could still be broadcast to
// mounts.
func ValidRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// IsUnder reports whether child is inside parent or equal to it. Both are
// absolute paths.
//
// This is a real path test, not a string prefix test: "/mnt/peerfoldX" is not
// under "/mnt/peerfold" even though the parent's text is a prefix of it. It is
// filepath.Rel that decides, so the comparison also respects the platform's
// separator and case rules.
func IsUnder(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		// Different volumes on Windows, or otherwise incomparable paths.
		return false
	}
	// A relative path escapes the parent when it is "..", or starts with a
	// ".." component. It is the component that matters, not the first two
	// bytes: a real entry may legitimately be named "..foo", and treating
	// that as an escape would both hide it from an exclusion and make a
	// legitimate file operation look like a traversal attempt.
	if rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
