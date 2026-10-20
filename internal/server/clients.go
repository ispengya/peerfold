package server

import (
	"encoding/json"
	"sort"

	"github.com/ispengya/peerfold/internal/proto"
)

// registerClientInfo records a supplier's connection details. It is called
// once registration has succeeded and is undone on every path out of the
// handler, so it lasts exactly as long as the connection does.
func (s *Server) registerClientInfo(c *proto.Conn, info proto.ClientInfo) {
	s.mu.Lock()
	s.clientInfos[c] = info
	s.mu.Unlock()
}

// unregisterClientInfo drops a supplier that has disconnected.
func (s *Server) unregisterClientInfo(c *proto.Conn) {
	s.mu.Lock()
	delete(s.clientInfos, c)
	s.mu.Unlock()
}

// ClientInfos returns the connected suppliers, sorted by name.
//
// This is the status view of the same fact Registry tracks: Registry answers
// whether a name is live, while this carries where the connection came from
// and when it was accepted. A slice is returned rather than the live map so a
// caller can hold it while connections come and go.
func (s *Server) ClientInfos() []proto.ClientInfo {
	s.mu.Lock()
	out := make([]proto.ClientInfo, 0, len(s.clientInfos))
	for _, info := range s.clientInfos {
		out = append(out, info)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].RemoteAddr < out[j].RemoteAddr
	})
	return out
}

// replyClients answers one KindClients request. The list is small, so it
// travels inside the control frame body rather than as a data stream.
func (s *Server) replyClients(c *proto.Conn, env proto.Envelope) error {
	body, err := json.Marshal(proto.ClientsOKBody{Clients: s.ClientInfos()})
	if err != nil {
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "encode clients reply: " + err.Error()})
	}
	return c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindClientsOK, Body: body})
}
