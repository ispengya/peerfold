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

	"peerfold/internal/node"
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
		m[n.Path] = n
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
	delete(m, path)
	prefix := path + "/"
	for p := range m {
		if strings.HasPrefix(p, prefix) {
			delete(m, p)
		}
	}
	// A client with no nodes left is gone as a key too. This keeps the
	// comment on Clients honest, and stops a client that removed everything
	// from looking present-but-empty.
	if len(m) == 0 {
		delete(ix.clients, client)
	}
}

// RemoveClient drops every node belonging to client.
func (ix *Index) RemoveClient(client string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	delete(ix.clients, client)
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
		// The roots are whatever is indexed at one path component.
		out := make([]node.Node, 0, len(m))
		for p, n := range m {
			if !strings.Contains(p, "/") {
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

	prefix := dir + "/"
	out := make([]node.Node, 0, 8)
	for p, n := range m {
		rest, found := strings.CutPrefix(p, prefix)
		if !found {
			continue
		}
		// Only direct children: a grandchild still has a separator in rest.
		if strings.Contains(rest, "/") {
			continue
		}
		out = append(out, n)
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
