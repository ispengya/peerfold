package mount

import (
	"path"
	"strings"

	"peerfold/internal/node"
)

// Clean normalises a FUSE path and refuses anything that would escape the
// mount root. FUSE hands us absolute slash-separated paths; the caller of this
// function is the last place that can notice a traversal attempt, because
// every layer below treats the path as opaque.
//
// The empty path means the root, because FUSE's own getattr on the mount point
// arrives as the empty string in some versions and as "/" in others, and
// treating one of those as an error would break every mount that used it.
func Clean(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", errStr("mount: path must be absolute: " + p)
	}
	cleaned := path.Clean(p)
	// path.Clean("/../etc") == "/etc", which already refuses to climb past the
	// root, but it does so silently. Reject the attempt instead of quietly
	// serving a different path: a caller that asked for ".." is either buggy
	// or hostile, and neither should be answered with a plausible-looking file.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errStr("mount: path escapes the root: " + p)
		}
	}
	return cleaned, nil
}

// SplitParent returns the parent directory and the base name. The root has no
// parent and yields errRoot.
//
// The two ends are returned together rather than derived twice at each call
// site, because a base name computed from a different string than the parent is
// the classic way a path check gets bypassed.
func SplitParent(p string) (parent, base string, err error) {
	if p == "/" || p == "" {
		return "", "", errRoot
	}
	parent = path.Dir(p)
	base = path.Base(p)
	if parent == "" {
		parent = "/"
	}
	return parent, base, nil
}

// lookup resolves a path to the index entry for it. It returns (node, 0) on
// success and (zero, -errno) on failure, so callers can return the code
// straight to cgofuse.
//
// Two different failures look identical when a name is absent from its parent:
// the name may simply not exist (ENOENT), or the parent may be a file rather
// than a directory (ENOTDIR). Callers branch on which one it is — a shell
// reports them differently, and so does every program that creates a file and
// then asks why it failed — so the two are told apart deliberately.
//
// Telling them apart costs one List of the parent, and the distinction comes
// entirely from that call's error, so it is worth stating what that error
// actually means rather than assuming. index.Children, measured, reports
// ErrNotDir only when the node at the queried path is itself a file, and
// ErrNotFound in every other case. It never returns an empty list for a path
// with no node, and — this is the part that is narrower than POSIX — it does
// not walk the path's ancestors: Children("a/file.txt/x") is ErrNotFound even
// though file.txt is a file, because the question it answers is "is there a
// node at this path, and is that node a directory".
//
// Two consequences follow, and both are pinned by tests. A name missing from a
// listable parent is ENOENT. A path whose immediate parent is a file is
// ENOTDIR. A file further up the path is ENOENT, and reporting ENOTDIR there
// would be inventing behaviour the frozen v1 core cannot produce. ENOENT rather
// than ENOTDIR is the safe direction in any case: "mkdir -p /a/b" proceeds on
// it, while ENOTDIR makes a caller stop.
//
// Keeping this at one List also keeps a lookup at O(1) round trips rather than
// O(depth). Every List is a round trip to the owner, so a component-by-component
// descent would trade this for one round trip per component on a deep path,
// which is the wrong trade for a stat that a shell issues before every command.
//
// There is no whole-tree cache here on purpose. The index is the federation's
// own view, and a second copy of it in this process would need its own
// invalidation, which is the machinery D5 exists to bound. What the kernel
// caches is tracked separately and narrowly: see the inode table in watch.go.
//
// One narrow exception, and its bound is the point: a path that a *push* has
// named is answered from that push rather than from a listing. That is not
// caching the tree, it is recording what this mount was told, and it is what
// keeps a listing from contradicting a change that has already been applied. The
// table holds exactly the paths that were named by an event -- never a directory
// listing -- so a cold path still costs the same one List, and a walk of a
// directory it has never heard about is unaffected.
func (m *Mount) lookup(p string) (node.Node, int) {
	cleaned, err := Clean(p)
	if err != nil {
		return node.Node{}, -EINVAL
	}
	if cleaned == "/" {
		// The root is synthesised, not listed: no client owns it and the
		// server has no entry for it, so listing its parent is impossible.
		// Its mode comes from the options because there is nothing to read.
		return node.Node{Path: "/", Kind: node.KindDir, Mode: m.opts.RootMode}, 0
	}
	if n, ok := m.eventFor(cleaned); ok {
		if n.Kind == node.KindDeleted {
			// The tombstone shadows whatever a listing would say. A listing is
			// answered from the caller's index, which for this path is older
			// than the push that removed it, so the push wins.
			return node.Node{}, -ENOENT
		}
		if n.Kind == node.KindSymlink {
			return node.Node{}, -ELOOP
		}
		return n, 0
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return node.Node{}, -EINVAL
	}
	children, err := m.caller.List(parent)
	if err != nil {
		// The parent could not be listed, and that error is the diagnosis, so
		// it is passed through rather than flattened. See the comment above for
		// what the layer below does and does not distinguish.
		return node.Node{}, errnoFor(err)
	}
	if child, ok := childNamed(children, base); ok {
		if child.Kind == node.KindSymlink {
			// A symlink's stat and its read disagree: the index records the
			// link itself, while the owner opens the path and the operating
			// system follows it. The two answers cannot both be right, and a
			// caller that stat'ed a small link and then read a large target
			// would corrupt its own buffers. Refusing is the only answer that
			// cannot corrupt anything (D12).
			//
			// This deliberately does not distinguish a POSIX symlink from a
			// Windows junction or reparse point. The client classifies all of
			// them as KindSymlink, and on Windows a junction is both more
			// common and more dangerous: following one can leave the shared
			// tree entirely.
			return node.Node{}, -ELOOP
		}
		return child, 0
	}
	// The name is not among the parent's children, and the parent listed
	// successfully, so the parent is a real directory and the name is simply
	// absent.
	return node.Node{}, -ENOENT
}

// childNamed finds the child whose base name is name.
//
// The comparison is on the base name rather than the whole path because List
// returns full paths, and on a whole name rather than a suffix so that a file
// called "ab" cannot answer for "b". The first match wins; a directory cannot
// hold two entries with the same name, so there is never a second.
func childNamed(children []node.Node, name string) (node.Node, bool) {
	for _, c := range children {
		if path.Base(c.Path) == name {
			return c, true
		}
	}
	return node.Node{}, false
}

// lookupParent returns the parent directory node and the base name, which is
// what every operation that creates, removes or renames something needs: the
// parent has to exist and be a directory, and the base name is the name that
// must be free (or present, for remove and rename).
//
// It refuses the root for the same reason SplitParent does: the root has no
// parent, so there is nothing to create it in or delete it from.
func (m *Mount) lookupParent(p string) (node.Node, string, int) {
	cleaned, err := Clean(p)
	if err != nil {
		return node.Node{}, "", -EINVAL
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return node.Node{}, "", -EINVAL
	}
	n, code := m.lookup(parent)
	if code != 0 {
		return node.Node{}, "", code
	}
	if n.Kind != node.KindDir {
		// A component of the parent path is a file. Reported as ENOTDIR rather
		// than ENOENT because the caller's mistake is about the shape of the
		// path, not about something being missing.
		return node.Node{}, "", -ENOTDIR
	}
	return n, base, 0
}
