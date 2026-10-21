// Package index is the server's aggregated view of every client's tree.
//
// It is keyed by client name and then by the client's own share-relative path,
// so the same path on two clients is two entries. Paths are case-sensitive and
// are never folded: "Documents" and "documents" are different nodes.
package index

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/ispengya/peerfold/internal/node"
)

// ErrNotDir reports that a path exists but is not a directory, so it cannot
// have children.
var ErrNotDir = errors.New("index: not a directory")

// ErrNotFound reports that no node is indexed at the path.
var ErrNotFound = errors.New("index: not found")

// Index holds every client's nodes. The zero value is ready to use.
//
// The mutex is an RWMutex because reads dominate: a listing walks the index far
// more often than a client pushes an update.
type Index struct {
	mu      sync.RWMutex
	clients map[string]map[string]node.Node
	// kids is the listing half of the index: client -> directory -> the full
	// paths of its direct children. It is maintained alongside clients rather
	// than derived on demand because Children used to scan every node of a
	// client to answer one listing. On a 100000-node tree that made each
	// directory open O(total nodes), and a walk of a node_modules tree did it
	// once per directory.
	//
	// The empty directory path addresses the top level, whose children are the
	// clients' share roots.
	kids map[string]map[string]map[string]struct{}
}

// Apply stores nodes under client, replacing any node already indexed at the
// same path. A node whose Kind is node.KindDeleted is not stored: it is an
// instruction to remove that path, which is exactly what Remove does.
//
// A path that is empty, absolute, or contains a ".." component is dropped
// rather than stored. The index is keyed by share-relative paths, and such a
// path would let a later subtree removal reach outside the subtree it names.
func (ix *Index) Apply(client string, nodes []node.Node) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for _, n := range nodes {
		if !validPath(n.Path) {
			continue
		}
		if n.Kind == node.KindDeleted {
			ix.removeLocked(client, n.Path)
			continue
		}
		if ix.clients == nil {
			ix.clients = make(map[string]map[string]node.Node)
		}
		m := ix.clients[client]
		if m == nil {
			m = make(map[string]node.Node)
			ix.clients[client] = m
		}
		if _, exists := m[n.Path]; !exists {
			ix.addKidLocked(client, parentOf(n.Path), n.Path)
		}
		m[n.Path] = n
	}
}

// parentOf returns the directory containing p, or "" for a top-level path.
//
// The index's paths are share-relative and slash-separated; Index.validPath
// guarantees no leading or trailing separator, so the last separator is the
// only one that matters.
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// addKidLocked records p as a child of dir. The caller holds the write lock.
func (ix *Index) addKidLocked(client, dir, p string) {
	if ix.kids == nil {
		ix.kids = make(map[string]map[string]map[string]struct{})
	}
	dirs := ix.kids[client]
	if dirs == nil {
		dirs = make(map[string]map[string]struct{})
		ix.kids[client] = dirs
	}
	children := dirs[dir]
	if children == nil {
		children = make(map[string]struct{})
		dirs[dir] = children
	}
	children[p] = struct{}{}
}

// dropKidLocked removes p from dir's child set, deleting empty containers.
// The caller holds the write lock.
func (ix *Index) dropKidLocked(client, dir, p string) {
	dirs := ix.kids[client]
	if dirs == nil {
		return
	}
	children := dirs[dir]
	if children == nil {
		return
	}
	delete(children, p)
	if len(children) == 0 {
		delete(dirs, dir)
	}
}

// Remove deletes path and everything beneath it on client.
//
// The comparison is by path component, not by raw string prefix: removing
// "Documents/sub" must not also delete "Documents/subway". Removing a path that
// is not indexed is a no-op, and the empty path is not a stored key, so it
// removes nothing; RemoveClient is what drops a whole client.
func (ix *Index) Remove(client, path string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(client, path)
}

func (ix *Index) removeLocked(client, path string) {
	m := ix.clients[client]
	if m == nil {
		return
	}
	ix.removeOneLocked(client, m, path)
	prefix := path + "/"
	for p := range m {
		if strings.HasPrefix(p, prefix) {
			ix.removeOneLocked(client, m, p)
		}
	}
	// The removed directory's own child set goes with it. A later path of the
	// same name starts with no children, which is what a recreated empty
	// directory looks like.
	delete(ix.kids[client], path)
	// A client with no nodes left is gone as a key too. This keeps the
	// comment on Clients honest, and stops a client that removed everything
	// from looking present-but-empty.
	if len(m) == 0 {
		delete(ix.clients, client)
		delete(ix.kids, client)
	}
}

// removeOneLocked deletes one node and its entry in its parent's child set.
// The caller holds the write lock and is responsible for the subtree.
func (ix *Index) removeOneLocked(client string, m map[string]node.Node, path string) {
	if _, ok := m[path]; !ok {
		return
	}
	delete(m, path)
	ix.dropKidLocked(client, parentOf(path), path)
}

// RemoveClient drops every node belonging to client.
func (ix *Index) RemoveClient(client string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	delete(ix.clients, client)
	delete(ix.kids, client)
}

// Lookup returns the node indexed at client's path.
func (ix *Index) Lookup(client, path string) (node.Node, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n, ok := ix.clients[client][path]
	return n, ok
}

// Children returns the direct children of dir on client, ascending by Path.
//
// The empty dir addresses the top level, which is where each share's root node
// lives; it is not a path that is indexed, so it is handled before the
// existence check rather than reported as missing. An unknown client therefore
// yields an empty slice and no error at "" — it has no roots — while any other
// path on an unknown client is ErrNotFound, because nothing is indexed there.
func (ix *Index) Children(client, dir string) ([]node.Node, error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	m := ix.clients[client]
	if dir == "" {
		// The roots are whatever is indexed at one path component, and the
		// child set for the empty parent is exactly those.
		paths := ix.kids[client][""]
		out := make([]node.Node, 0, len(paths))
		for p := range paths {
			if n, ok := m[p]; ok {
				out = append(out, n)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		return out, nil
	}

	parent, ok := m[dir]
	if !ok {
		return nil, ErrNotFound
	}
	if parent.Kind == node.KindFile {
		return nil, ErrNotDir
	}

	paths := ix.kids[client][dir]
	out := make([]node.Node, 0, len(paths))
	for p := range paths {
		if n, ok := m[p]; ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Clients returns the client names that have nodes, ascending.
func (ix *Index) Clients() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, 0, len(ix.clients))
	for c := range ix.clients {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Len returns the total number of nodes across all clients.
func (ix *Index) Len() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	n := 0
	for _, m := range ix.clients {
		n += len(m)
	}
	return n
}

// validPath reports whether p may be indexed.
//
// Paths are share-relative and slash-separated, so this rejects:
//   - the empty path;
//   - anything absolute (leading "/");
//   - any "." or ".." component, which would let a later subtree removal reach
//     outside the subtree it names;
//   - empty components, from a leading, trailing, or doubled "/", which make a
//     path that is its own parent's "child" and that Remove cannot match;
//   - any backslash. A Windows-style separator would otherwise arrive as one
//     opaque component: "Documents\..\..\etc" contains no "/" to split on, so
//     it would be stored and later joined onto a share root as a real escape.
//
// A rejected path is dropped rather than reported, so a non-conforming peer
// cannot poison the index. That also means the index cannot be relied on to
// tell a caller its path was bad.
func validPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return false
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
