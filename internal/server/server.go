package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"peerfold/internal/index"
	"peerfold/internal/proto"
)

// helloTimeout bounds the handshake only. An established connection is not
// subject to it: an idle supplier is a legitimate state, not a stalled one.
const helloTimeout = 30 * time.Second

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
// its own goroutine, and Serve returns only after every one of them has exited:
// it is the drain point that Close deliberately is not.
func (s *Server) Serve() error {
	if s.lnErr != nil {
		return s.lnErr
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

// Close stops the listener and closes every live connection, which unblocks the
// handlers parked in a read.
//
// Close does not wait for those handlers to finish: it closes, it returns, and
// Serve is the point that waits. That split is deliberate, because Serve must
// not block waiting on connections that only Close can unblock. A caller that
// never observes Serve's return therefore has no drain point, and should treat
// the registry and index as possibly still settling for a moment afterwards.
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

	// A peer that connects and never speaks must not pin this goroutine and a
	// descriptor forever. The deadline covers only the handshake: an idle but
	// established supplier is a legitimate state, so the steady-state loop
	// below runs without one.
	_ = conn.SetReadDeadline(time.Now().Add(helloTimeout))

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
	_ = conn.SetReadDeadline(time.Time{})

	if hb.Role != proto.RoleClient && hb.Role != proto.RoleViewer {
		// An unrecognized role must not be quietly treated as a viewer: that
		// tells a misconfigured client it succeeded while it supplies nothing.
		_ = c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHello, Err: fmt.Sprintf("bad hello: unknown role %q", hb.Role)})
		return
	}

	// A viewer only asks questions; it supplies nothing, so it is not a
	// registry entry and owns no index entries.
	if hb.Role == proto.RoleViewer {
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
	//
	// The index is cleared while the name still owns it, before the name is
	// released. Releasing first would let a successor register in the gap and
	// have its freshly applied nodes deleted by this handler's cleanup.
	defer func() {
		s.ix.RemoveClient(hb.Name)
		s.reg.Unregister(hb.Name)
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
				// Recv reports a read failure with no envelope, so there is no
				// request to correlate an answer with. Send the diagnostic
				// out of band — an empty Kind and ID mean "this is not the
				// reply to anything" — and then close, since the framing is
				// what just failed.
				_ = c.Send(proto.Envelope{Err: err.Error()})
			}
			return
		}
		if err := c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "not implemented"}); err != nil {
			return
		}
	}
}
