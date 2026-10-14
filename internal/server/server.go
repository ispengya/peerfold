package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"peerfold/internal/index"
	"peerfold/internal/proto"
)

// Server accepts client connections and owns the aggregated index.
//
// New binds the listener, so Addr is meaningful before Serve is called and
// "127.0.0.1:0" reports the port the kernel actually chose.
type Server struct {
	ln    net.Listener
	lnErr error
	ix    *index.Index
	reg   *Registry

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool
	closed  bool
	wg      sync.WaitGroup
}

// New returns a Server listening on listen. A listen failure is remembered and
// returned by Serve rather than panicking here, so a caller can report it as a
// startup error; Addr is nil in that case.
func New(listen string) *Server {
	s := &Server{
		ix:    &index.Index{},
		reg:   &Registry{},
		conns: make(map[net.Conn]struct{}),
	}
	s.ln, s.lnErr = net.Listen("tcp", listen)
	return s
}

// Addr is the address the server is listening on, or nil if the listener could
// not be created.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Index is the aggregated tree.
func (s *Server) Index() *index.Index { return s.ix }

// Registry is the connected-client table.
func (s *Server) Registry() *Registry { return s.reg }

// Serve accepts connections until Close is called. Each connection is served on
// its own goroutine. The error from a listener closed by Close is reported as
// nil: that is an ordinary shutdown, not a failure.
func (s *Server) Serve() error {
	if s.lnErr != nil {
		return s.lnErr
	}
	if s.ln == nil {
		return errors.New("server: Serve called on a closed server")
	}

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				// Close waits for the handlers so nothing unregisters after a
				// caller believes shutdown finished.
				s.wg.Wait()
				return nil
			}
			return err
		}

		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			conn.Close()
			s.wg.Wait()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

// Close stops the listener and every live connection, then waits for the
// handlers to finish. It is safe to call more than once.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.closing = true
	ln := s.ln
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	// Closing the connections is what unblocks handlers parked in a read; the
	// WaitGroup is only waited on by Serve, so Close must not block on it.
	for _, c := range conns {
		c.Close()
	}
	return err
}

// handleConn performs the hello handshake and then serves the connection until
// the peer goes away.
func (s *Server) handleConn(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	c := proto.NewConn(conn)

	env, err := c.Recv()
	if err != nil {
		return
	}
	if env.Kind != proto.KindHello {
		// Nothing is implemented before the handshake, so this is refused
		// rather than left to look like a request that was simply ignored.
		_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "not implemented: expected hello first"})
		return
	}
	var hb proto.HelloBody
	if err := json.Unmarshal(env.Body, &hb); err != nil {
		_ = c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHello, Err: fmt.Sprintf("bad hello: %v", err)})
		return
	}

	// A viewer only asks questions; it supplies nothing, so it is not a
	// registry entry and owns no index entries.
	if hb.Role != proto.RoleClient {
		if err := s.replyHello(c, env.ID, hb.Name, false); err != nil {
			return
		}
		s.serveLoop(c)
		return
	}

	if hb.Name == "" {
		_ = c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHello, Err: "bad hello: name is required"})
		return
	}

	aggregate, err := s.reg.Register(hb.Name, hb.Aggregate)
	if err != nil {
		_ = c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHello, Err: err.Error()})
		return
	}
	// From here on the name owns index entries, so every exit path has to drop
	// them: a client that reconnects must not inherit a stale tree.
	defer func() {
		s.reg.Unregister(hb.Name)
		s.ix.RemoveClient(hb.Name)
	}()

	if err := s.replyHello(c, env.ID, hb.Name, aggregate); err != nil {
		return
	}
	s.serveLoop(c)
}

// replyHello answers a hello with the server's ruling on the aggregate role.
func (s *Server) replyHello(c *proto.Conn, id uint64, name string, aggregate bool) error {
	body, err := json.Marshal(proto.HelloOKBody{Name: name, Aggregate: aggregate})
	if err != nil {
		return err
	}
	return c.Send(proto.Envelope{ID: id, Kind: proto.KindHelloOK, Body: body})
}

// serveLoop answers whatever arrives until the peer disconnects or misbehaves.
//
// Every kind but hello is unimplemented in this task, and an unknown or
// malformed frame ends the connection: continuing to read a stream whose
// framing is in doubt is how a peer desynchronises silently.
func (s *Server) serveLoop(c *proto.Conn) {
	for {
		env, err := c.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()})
			}
			return
		}
		if err := c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "not implemented"}); err != nil {
			return
		}
	}
}
