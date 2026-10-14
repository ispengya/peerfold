package server

import (
	"peerfold/internal/node"
)

// Query is the read side of the server: it answers what is currently indexed
// without touching the write paths. It is obtained from a Server and is safe
// for concurrent use, because it holds no state of its own — every answer is
// read from the live index and registry.
type Query struct {
	s *Server
}

// Query returns the read-side view of this server.
func (s *Server) Query() *Query { return &Query{s: s} }

// Clients returns the connected client names, ascending.
func (q *Query) Clients() []string { return q.s.reg.Live() }

// List returns the direct children of path on client, ascending.
//
// client == "" addresses the top level, which is the set of connected clients
// rather than the contents of any index: a client is presented as a directory
// named after itself, so a caller walks down through the client namespace and
// then into that client's share-relative paths.
//
// A client that is live but has indexed nothing yet is still listed — the top
// level describes who is connected, not who has finished uploading.
//
// An unknown client is ErrNoSuchClient, and it is reported for the top level
// too: an empty listing there would be indistinguishable from a server with no
// clients, which is a different situation.
func (q *Query) List(client, path string) ([]node.Node, error) {
	if client == "" {
		return q.topLevel(path)
	}
	if !q.live(client) {
		return nil, ErrNoSuchClient
	}
	return q.s.ix.Children(client, path)
}

// topLevel synthesizes the client namespace. Only the empty path exists here:
// "win" is a top-level name, but "win/Documents" belongs to the client's own
// namespace and is addressed with client == "win" instead.
func (q *Query) topLevel(path string) ([]node.Node, error) {
	if path != "" {
		// The first component would be a client name, so it is addressed as
		// List(name, "") by the caller. Reaching here means the caller asked
		// for a client-relative path without naming the client.
		return nil, ErrNoSuchClient
	}
	names := q.s.reg.Live()
	out := make([]node.Node, 0, len(names))
	for _, name := range names {
		out = append(out, node.Node{Path: name, Kind: node.KindDir})
	}
	return out, nil
}

// live reports whether name is connected.
func (q *Query) live(name string) bool {
	for _, n := range q.s.reg.Live() {
		if n == name {
			return true
		}
	}
	return false
}
