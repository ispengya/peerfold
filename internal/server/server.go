package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"peerfold/internal/index"
	"peerfold/internal/node"
	"peerfold/internal/proto"
)

// helloTimeout bounds the handshake only. An established connection is not
// subject to it: an idle supplier is a legitimate state, not a stalled one.
const helloTimeout = 30 * time.Second

// readTimeout bounds a read forwarded to a client.
//
// It is shorter than a listing's budget because a read is what a user typed and
// is waiting on: an owner that has not answered in two seconds is either gone or
// wedged, and either way a failure serves the user better than a wait.
const readTimeout = 2 * time.Second

// Server accepts client connections and owns the aggregated index.
//
// New binds the listener, so Addr is meaningful before Serve is called and
// "127.0.0.1:0" reports the port the kernel actually chose.
type Server struct {
	ln     net.Listener
	lnErr  error
	ix     *index.Index
	reg    *Registry
	router *Router

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
		ix:     &index.Index{},
		reg:    &Registry{},
		conns:  make(map[net.Conn]struct{}),
		router: &Router{clients: make(map[string]*clientLink)},
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
		// A viewer has no name in the registry, so it owns no index entries
		// and cannot upload a tree.
		s.serveLoop("", c)
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
		// The router link goes last. A request forwarded to a client that is on
		// its way out must find it gone and fail, rather than be written into a
		// connection nobody is reading.
		s.router.detach(hb.Name)
	}()

	if err := s.replyHello(c, env.ID, hb.Name, aggregate); err != nil {
		return
	}
	// Requests can only be forwarded to a client that is reachable, and this
	// starts after the hello is answered so the order stays "the handshake, then
	// everything else".
	s.router.attach(hb.Name, c)
	s.serveLoop(hb.Name, c)
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
// A tree upload is indexed batch by batch as it arrives, so the aggregated tree
// grows during the upload instead of appearing all at once at the end.
//
// name is the registered client this connection owns index entries for, or ""
// for a viewer, which owns none.
func (s *Server) serveLoop(name string, c *proto.Conn) {
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

		if env.Kind == proto.KindTreeBatch {
			if err := s.applyBatch(name, env); err != nil {
				_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()})
				return
			}
			// Every batch is acknowledged, and the acknowledgement matters:
			// it is what keeps a client that uploads a large tree from
			// streaming into a socket nobody is draining, which would wedge
			// both ends once the buffers fill. It carries no request ID
			// because it is a flow-control token, not a reply to correlate.
			if err := c.Send(proto.Envelope{}); err != nil {
				return
			}
			continue
		}

		// TreeDone closes the upload. Its reply is the acknowledgement the client
		// waits for, which is what makes "Err == nil" mean the whole tree is
		// indexed rather than merely written to a socket.
		//
		// The loop keeps going: ending the upload is not the same as ending the
		// connection. A client that disconnects here would lose the very tree it
		// just published, because the index belongs to the connection and is
		// dropped when this handler returns.
		if env.Kind == proto.KindTreeDone {
			if !env.EOF {
				// An upload is a sequence of batches closed by one TreeDone,
				// and every message is its own control frame. EOF is what
				// marks the end of the sequence, so a TreeDone without it is
				// an unfinished upload and must not be acknowledged as whole.
				if err := c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "tree done without the end-of-upload marker"}); err != nil {
					return
				}
				continue
			}
			if err := c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind}); err != nil {
				return
			}
			continue
		}

		if env.Kind == proto.KindList {
			if err := s.replyList(c, env); err != nil {
				return
			}
			continue
		}

		// KindRead on a client's own connection is either a question or an
		// answer, and the direction is what tells them apart: a question is
		// forwarded and names the target client in its body, while an owner's
		// answer is addressed from itself, so it arrives on the connection whose
		// name it carries. A viewer has no name, so its KindRead is always a
		// question.
		if env.Kind == proto.KindRead {
			var b proto.ReadBody
			if err := json.Unmarshal(env.Body, &b); err != nil {
				if name == "" {
					_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: fmt.Sprintf("bad read request: %v", err)})
					continue
				}
				// A malformed answer from a client is a protocol failure, not a
				// request to answer, so the connection is dropped.
				return
			}
			if name != "" && b.Client == name {
				// An owner answering a forwarded read. Its content is still on
				// the wire, so the serve loop forwards it: only this goroutine
				// may read this connection.
				os.Stderr.WriteString(fmt.Sprintf("PROBE complete name=%q id=%d err=%q\n", name, env.ID, env.Err))
				if !s.router.complete(name, env) {
					_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "unsolicited reply: nothing is waiting for this"})
				}
				continue
			}
			if err := s.replyRead(c, env); err != nil {
				return
			}
			continue
		}

		// Any other forwarded kind is a plain answer, so it goes to whoever
		// asked rather than being answered here. One with no waiter is a peer
		// answering something it was not asked, which is a protocol error rather
		// than noise to discard.
		if isReply(env) {
			if !s.router.deliver(name, env) {
				_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "unsolicited reply: nothing is waiting for this"})
			}
			continue
		}

		if err := c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "not implemented"}); err != nil {
			return
		}
	}
}

// replyList answers one listing request.
//
// A listing is available to any connection, including a supplier: reading the
// aggregated tree is not a privileged operation, and the index is keyed by
// client name rather than by connection, so a client can list its own tree over
// the same connection it uploads on.
func (s *Server) replyList(c *proto.Conn, env proto.Envelope) error {
	var b proto.ListBody
	if err := json.Unmarshal(env.Body, &b); err != nil {
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: fmt.Sprintf("bad list request: %v", err)})
	}
	nodes, err := s.Query().List(b.Client, b.Path)
	if err != nil {
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()})
	}
	out := make([]proto.NodeJSON, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, proto.NodeJSON{
			Path:  n.Path,
			Kind:  uint8(n.Kind),
			Size:  n.Size,
			Mode:  n.Mode,
			Mtime: n.Mtime,
		})
	}
	body, err := json.Marshal(proto.ListOKBody{Nodes: out})
	if err != nil {
		// A marshal failure is the server's fault, not the request's, so it is
		// reported as an ordinary error rather than a framing failure.
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: fmt.Sprintf("encode list reply: %v", err)})
	}
	// The control frame goes first and carries no body, which is what tells the
	// reader that the bytes are coming as a stream. It has to: a directory of
	// 20000 files is far past one frame's payload, so a listing cannot be
	// answered in the envelope at all.
	if err := c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindListOK}); err != nil {
		return err
	}
	return c.SendBody(env.ID, body)
}

// replyRead forwards one read request to the client that owns the path.
//
// The content itself does not pass through this function: the owner's serve loop
// reads it and writes it straight onto this connection, because only that loop
// may read the owner's socket. This function waits for the answer's control frame
// — which is where "no such file" lives, since content alone cannot tell a
// deleted file from an empty one — and then tells the requester the read is over.
func (s *Server) replyRead(c *proto.Conn, env proto.Envelope) error {
	var b proto.ReadBody
	if err := json.Unmarshal(env.Body, &b); err != nil {
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: fmt.Sprintf("bad read request: %v", err)})
	}
	if b.Client == "" {
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "bad read request: client is required"})
	}
	if b.Off < 0 {
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "bad read request: offset must not be negative"})
	}

	// The requester is recorded with the request so the owner's loop knows where
	// to put the content. This handler runs on the server's connection to the
	// requester, not on the owner's, so the owner's loop is free to answer.
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	reply, err := s.router.Send(ctx, b.Client, proto.Envelope{Kind: proto.KindRead, Body: env.Body}, c, env.ID)
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("PROBE replyRead sendErr=%v\n", err))
		return c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()})
	}
	os.Stderr.WriteString(fmt.Sprintf("PROBE replyRead ok replyID=%d replyErr=%q sending tail\n", reply.ID, reply.Err))
	// The content has already been written, so this is the last thing the
	// requester needs: it marks the end of the read and carries its outcome.
	return c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindRead})
}

// applyBatch indexes one tree batch for the named client.
func (s *Server) applyBatch(name string, env proto.Envelope) error {
	if name == "" {
		// Only a registered client owns index entries, so a viewer or an
		// unregistered connection may not write any.
		return errors.New("tree upload is only available to a registered client")
	}
	var b proto.TreeBatchBody
	if err := json.Unmarshal(env.Body, &b); err != nil {
		return fmt.Errorf("bad tree batch: %w", err)
	}
	nodes := make([]node.Node, 0, len(b.Nodes))
	for _, n := range b.Nodes {
		nodes = append(nodes, node.Node{
			Path:  n.Path,
			Kind:  node.Kind(n.Kind),
			Size:  n.Size,
			Mode:  n.Mode,
			Mtime: n.Mtime,
		})
	}
	s.ix.Apply(name, nodes)
	return nil
}
