package server

import (
	"encoding/json"
	"sort"

	"github.com/ispengya/peerfold/internal/proto"
)

// registerViewer records one mount connection. It is called from the hello
// handler and undone on every path out of it, so the registry lasts exactly as
// long as the connection does.
func (s *Server) registerViewer(c *proto.Conn, info proto.ViewerInfo) {
	s.mu.Lock()
	s.viewers[c] = info
	s.mu.Unlock()
}

// unregisterViewer drops a mount that has disconnected.
func (s *Server) unregisterViewer(c *proto.Conn) {
	s.mu.Lock()
	delete(s.viewers, c)
	s.mu.Unlock()
}

// Viewers returns the mounts currently attached to the server, sorted by name
// and then mount point.
//
// It is the server's own account, so it stays correct when the mount's machine
// is unreachable by anything except the connection the mount itself keeps open.
// A slice is returned rather than the live map so a caller can hold it while
// connections come and go.
func (s *Server) Viewers() []proto.ViewerInfo {
	s.mu.Lock()
	out := make([]proto.ViewerInfo, 0, len(s.viewers))
	for _, info := range s.viewers {
		out = append(out, info)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].MountPoint != out[j].MountPoint {
			return out[i].MountPoint < out[j].MountPoint
		}
		return out[i].RemoteAddr < out[j].RemoteAddr
	})
	return out
}

// replyViewers answers one KindViewers request. The list is small, so it
// travels inside the control frame body rather than as a data stream; a status
// query has no reason to pay for the streaming path.
func (s *Server) replyViewers(c *proto.Conn, env proto.Envelope) error {
	body, err := json.Marshal(proto.ViewersOKBody{Viewers: s.Viewers()})
	if err != nil {
		// A marshal failure is the server's fault, not the request's, so it is
		// reported as an ordinary error rather than a framing failure.
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "encode viewers reply: " + err.Error()})
	}
	return c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindViewersOK, Body: body})
}
