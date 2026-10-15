package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

// drainTimeout bounds the cleanup read of a put's content on a path where the
// write was refused.
//
// When the forwarding fails, the requester may be waiting: its writer is part
// way through sending a file, and the reason it must be drained at all is that
// nobody is going to read the rest of it. A drain that could block as long as
// the requester's own send would hold this handler, and the connection behind
// it, for as long as the requester chose — observed as a test hanging for six
// minutes. So the drain is bounded, and a timeout ends it: the point of the
// drain is to leave the connection at a frame boundary, and one where the peer
// has stopped sending is not a connection worth rescuing.
const drainTimeout = 5 * time.Second

// chunkTimeout bounds the wait for one more chunk of a forwarded write.
//
// It is per chunk, not per transfer: a large file over a slow link keeps
// resetting it and is never failed for being slow, while a requester that stops
// sending midway is failed instead of holding the owner's connection — and the
// write lock on it — for the whole of writeTimeout. Without it, the wait for the
// next frame inherited whatever deadline the previous read left behind, and a
// requester that simply stopped talking parked an owner's connection forever.
const chunkTimeout = 5 * time.Minute

// writeTimeout bounds a write forwarded to a client.
//
// It is far longer than a read's, and deliberately: a read is answered by the
// owner reading its own disk, while a write may be waiting for the requester to
// finish sending a file across a slow link. Failing a transfer that is still
// making progress would be wrong; the bound exists so a wedged owner or a
// requester that stops sending fails the call rather than holding it for ever.
const writeTimeout = 10 * time.Minute

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

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	// watchers is every live viewer connection, which is what an index update
	// is pushed to. It is separate from conns because conns is all peers and
	// this is only the ones that want to be told.
	watchers map[*proto.Conn]struct{}
	closing  bool
	closed   bool
	wg       sync.WaitGroup
}

// New returns a Server listening on listen. A listen failure is remembered and
// returned by Serve rather than panicking here, so a caller can report it as a
// startup error; Addr is nil in that case.
func New(listen string) *Server {
	s := &Server{
		ix:       &index.Index{},
		reg:      &Registry{},
		conns:    make(map[net.Conn]struct{}),
		watchers: make(map[*proto.Conn]struct{}),
		router:   &Router{clients: make(map[string]*clientLink)},
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

// Err is why the listener could not be created, or nil if it was.
//
// New keeps the failure instead of panicking so a caller can report it as a
// startup error — but a caller that only sees a nil Addr cannot say anything
// more useful than "unavailable", which is exactly what a user with the port
// already in use needs to be told. This is that missing half: the operating
// system's own words about the bind.
func (s *Server) Err() error { return s.lnErr }

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
	//
	// It is recorded as a watcher, though, because it is the role that wants to
	// be told when the tree changes. The registration lasts exactly as long as
	// this handler runs, which is exactly as long as the connection does.
	if hb.Role == proto.RoleViewer {
		if err := s.replyHello(c, env.ID, hb.Name, false); err != nil {
			return
		}
		unwatch := s.watch(c)
		defer unwatch()
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
		// RecvAny, not Recv: a client's update is a frame this loop has to act
		// on, and the skipping that suits a viewer would discard it here.
		env, err := c.RecvAny()
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

		// A read answer is a kind of its own. That is what keeps request and
		// answer apart: they travel in opposite directions on the same
		// connection, and the answer carries no body, but neither does a request
		// whose body failed to encode. Guessing from the body's presence would
		// mistake the owner's answer for a second question.
		if env.Kind == proto.KindReadOK {
			// An owner answering a forwarded read. Its content is still on the
			// wire, so the serve loop reads it: only this goroutine may read
			// this connection.
			//
			// The handler is passed in because the answer and the owner's own
			// change events share this connection: an event can arrive between
			// the answer's frames, and reading the answer must not throw it away.
			if !s.router.complete(name, env, func(e proto.Envelope) { s.applyEvent(name, e) }) {
				_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "unsolicited reply: nothing is waiting for this"})
			}
			continue
		}

		if env.Kind == proto.KindRead {
			if err := s.replyRead(c, env); err != nil {
				return
			}
			continue
		}

		// A write answer is a kind of its own for the same reason a read's is:
		// the request and the answer travel in opposite directions on this
		// connection and are identical in shape, so nothing about the frame
		// itself can say which one arrived. It carries a stream even though
		// every one of them is empty, and the stream has to be read here
		// because only this goroutine may read this connection.
		if env.Kind == proto.KindWriteOK {
			if !s.router.complete(name, env, func(e proto.Envelope) { s.applyEvent(name, e) }) {
				_ = c.Send(proto.Envelope{ID: env.ID, Kind: env.Kind, Err: "unsolicited reply: nothing is waiting for this"})
			}
			continue
		}

		if isWriteRequest(env.Kind) {
			if err := s.replyWrite(c, env); err != nil {
				return
			}
			continue
		}

		// A client reporting a change it saw happen. The index is updated here
		// and the same update is pushed to every watcher, so a viewer learns
		// about it through the connection it already has instead of asking
		// again.
		//
		// The node list is not acknowledged: there is nothing for the client to
		// do with an acknowledgement, and a frame in reply would give the
		// client's read loop something to classify that means nothing.
		if env.Kind == proto.KindEvent {
			if name == "" {
				// Only a named client owns index entries, so a viewer sending
				// one of these is a protocol error rather than a change.
				continue
			}
			s.applyEvent(name, env)
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

// eventBroadcastTimeout bounds one push to one watcher.
//
// A viewer that has stopped reading fills its socket buffer, and an unbounded
// write to it would block here for ever. The bound turns one viewer's
// misbehaviour into that viewer's own problem: it is dropped and the change is
// still delivered to everyone else.
const eventBroadcastTimeout = 5 * time.Second

// applyEvent folds a client's change report into the index and pushes it to
// every watcher.
//
// The index update and the broadcast carry the same nodes. A watcher holds the
// same model the index does, so a viewer that applies the pushed update itself
// ends up where a viewer that lists the server does — and the two must agree,
// because a viewer will use one after the other.
func (s *Server) applyEvent(name string, env proto.Envelope) {
	var b proto.EventBody
	if err := json.Unmarshal(env.Body, &b); err != nil {
		return
	}
	nodes := fromNodeJSON(b.Nodes)
	if len(nodes) == 0 {
		return
	}

	// A deletion is not a node with a kind; it is an instruction to drop the
	// path and everything under it. It is turned into the index's own removal
	// here, so no other layer has to know the marker exists.
	var keep []node.Node
	for _, n := range nodes {
		if n.Kind == node.KindDeleted {
			s.ix.Remove(name, n.Path)
			continue
		}
		keep = append(keep, n)
	}
	if len(keep) > 0 {
		s.ix.Apply(name, keep)
	}

	// The update goes out with the client name on it, because a path is only
	// meaningful inside one client's tree: "Documents/x.txt" names a different
	// file on every machine.
	out := proto.EventBody{Nodes: withClient(name, toNodeJSON(nodes))}
	body, err := json.Marshal(out)
	if err != nil {
		return
	}
	s.broadcast(proto.Envelope{Kind: proto.KindEvent, Body: body})
}

// withClient prefixes every node's path with the client that reported it.
//
// The index is keyed by client name and by path, while the watcher's own paths
// are relative to a share. A viewer holds the aggregated form, so the name has
// to be put back before the update leaves the server.
func withClient(name string, nodes []proto.NodeJSON) []proto.NodeJSON {
	out := make([]proto.NodeJSON, 0, len(nodes))
	for _, n := range nodes {
		if n.Path == "" {
			// The share root, which is already the name.
			n.Path = name
		} else {
			n.Path = name + "/" + n.Path
		}
		out = append(out, n)
	}
	return out
}

// toNodeJSON converts nodes to their wire form.
func toNodeJSON(nodes []node.Node) []proto.NodeJSON {
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
	return out
}

// fromNodeJSON converts the wire form back to nodes.
func fromNodeJSON(in []proto.NodeJSON) []node.Node {
	out := make([]node.Node, 0, len(in))
	for _, n := range in {
		out = append(out, node.Node{
			Path:  n.Path,
			Kind:  node.Kind(n.Kind),
			Size:  n.Size,
			Mode:  n.Mode,
			Mtime: n.Mtime,
		})
	}
	return out
}

// broadcast sends one envelope to every watcher.
//
// A watcher that has gone is dropped rather than reported: nothing was asked of
// it, so its absence is not an error, and it must not stop the update reaching
// the watchers that are still there.
//
// Each target is written to in its own goroutine, and that is not an
// optimisation — it is what keeps one watcher from stalling the others and from
// stalling the client that reported the change. SendWithin's bound only starts
// once it holds the connection's write lock, so a watcher whose socket is
// already blocked mid-reply (a reply it is not reading: a suspended process, a
// peer that stopped reading an 8 MB read) parks the sender on that lock. Written
// serially, as this was, that parking happened in the reporting client's own
// serving loop: every later change that client reported went unread and never
// reached the index, and no other watcher was told anything. The identical
// goroutine also means a genuinely wedged watcher cannot push the rest of the
// fan-out past its own deadline, because it never shares their turn.
func (s *Server) broadcast(env proto.Envelope) {
	s.mu.Lock()
	targets := make([]*proto.Conn, 0, len(s.watchers))
	for c := range s.watchers {
		targets = append(targets, c)
	}
	s.mu.Unlock()

	for _, c := range targets {
		go func(c *proto.Conn) {
			if err := c.SendWithin(env, eventBroadcastTimeout); err != nil {
				s.mu.Lock()
				delete(s.watchers, c)
				s.mu.Unlock()
			}
		}(c)
	}
}

// watch starts recording a viewer connection as a watcher, and returns the
// function that stops.
func (s *Server) watch(c *proto.Conn) (unwatch func()) {
	s.mu.Lock()
	s.watchers[c] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.watchers, c)
		s.mu.Unlock()
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
	out := toNodeJSON(nodes)
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

// replyRead forwards one read request to the client that owns the path and
// streams that client's answer back.
//
// The answer is three parts in the same shape whether the read worked or not: a
// control frame, the content as a data stream, and a closing control frame. The
// control frame is what carries "this file is not available" — the content stream
// alone cannot say why a read produced nothing, and reporting every failure as an
// empty body would make a deleted file and a zero-byte file the same thing to the
// caller.
//
// The shape must not vary between the two outcomes. A failure that stopped after
// the control frame would leave the requester's next answer to be read from where
// this answer's content should have been, because frames are one sequence.
func (s *Server) replyRead(c *proto.Conn, env proto.Envelope) error {
	var b proto.ReadBody
	var fail error
	var stream <-chan answerContent
	var reply proto.Envelope
	switch err := json.Unmarshal(env.Body, &b); {
	case err != nil:
		fail = fmt.Errorf("bad read request: %v", err)
	case b.Client == "":
		fail = errors.New("bad read request: client is required")
	case b.Off < 0:
		fail = errors.New("bad read request: offset must not be negative")
	default:
		// The owner is asked over its connection, and this handler runs on the
		// server's connection to the requester — not on the owner's serve loop —
		// so the owner's loop is free to answer.
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		var err error
		reply, stream, err = s.router.Send(ctx, b.Client, proto.Envelope{Kind: proto.KindRead, Body: env.Body})
		switch {
		case err != nil:
			fail = err
		case stream == nil:
			// The owner answered without announcing content, which is not a read.
			fail = errors.New("the owner answered a read without announcing any content")
		case reply.Err != "":
			fail = errors.New(reply.Err)
			// The owner's content is still on the way and is empty, so it is read
			// off here rather than left on the wire. The router reads a whole
			// answer before it hands it over, so this is already in the channel.
			<-stream
		}
	}
	if fail != nil {
		// The head frame is where "no such file" travels, and a failed read is an
		// empty stream rather than a missing one.
		if err := c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindReadOK, Err: fail.Error()}); err != nil {
			return err
		}
		if err := c.SendData(env.ID, nil); err != nil {
			return err
		}
		return c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindReadOK})
	}

	// The content is already in hand — the router reads a whole answer before it
	// hands it over, so the owner's connection is never read by two goroutines at
	// once — and the reading failure, if there was one, is attached to the frame
	// that is about to be sent.
	content := <-stream
	if err := c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindReadOK, Err: reply.Err}); err != nil {
		return err
	}
	if len(content.body) > 0 {
		// SendBody ends the stream with the marker itself.
		if err := c.SendBody(env.ID, content.body); err != nil {
			return err
		}
		return c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindReadOK})
	}
	if err := c.SendData(env.ID, nil); err != nil {
		return err
	}
	return c.Send(proto.Envelope{ID: env.ID, Kind: proto.KindReadOK})
}

// replyWrite forwards one write request to the client that owns the path and
// reports how it went.
//
// The answer has the same three-part shape a read uses — a control frame, a
// content stream, a closing control frame — for the same reason: frames are one
// sequence, so a part left unread becomes the next frame whoever reads it. The
// stream here is empty on every path and still present, because its absence is
// indistinguishable from a dropped frame.
//
// The content of a put travels the other way. It is read off the requester's
// connection one frame at a time and written to the owner's, so a file larger
// than memory is still a single request. The owner is handed a whole stream and
// answers only once it has it.
func (s *Server) replyWrite(c *proto.Conn, env proto.Envelope) error {
	switch env.Kind {
	case proto.KindWrite:
		return s.replyPut(c, env)
	case proto.KindMkdir:
		var b proto.MkdirBody
		if err := json.Unmarshal(env.Body, &b); err != nil {
			return s.writeAnswer(c, env, fmt.Errorf("bad mkdir request: %v", err))
		}
		return s.replyNamed(c, env, b.Client, "mkdir")
	case proto.KindRemove:
		var b proto.RemoveBody
		if err := json.Unmarshal(env.Body, &b); err != nil {
			return s.writeAnswer(c, env, fmt.Errorf("bad remove request: %v", err))
		}
		return s.replyNamed(c, env, b.Client, "remove")
	case proto.KindRename:
		var b proto.RenameBody
		if err := json.Unmarshal(env.Body, &b); err != nil {
			return s.writeAnswer(c, env, fmt.Errorf("bad rename request: %v", err))
		}
		return s.replyRename(c, env, b)
	default:
		return s.writeAnswer(c, env, fmt.Errorf("bad write request: unknown kind %q", env.Kind))
	}
}

// replyNamed forwards a write request that carries no content, once its
// addressing has been checked.
func (s *Server) replyNamed(c *proto.Conn, env proto.Envelope, client, what string) error {
	if client == "" {
		return s.writeAnswer(c, env, fmt.Errorf("bad %s request: client is required", what))
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	// content is nil: there is nothing to send, and the announcement still ends
	// the stream, so the owner reads exactly one.
	reply, err := s.router.ForwardWrite(ctx, client, proto.Envelope{Kind: env.Kind, Body: env.Body}, nil)
	if err != nil {
		return s.writeAnswer(c, env, err)
	}
	if reply.Err != "" {
		return s.writeAnswer(c, env, errors.New(reply.Err))
	}
	return s.writeAnswer(c, env, nil)
}

// replyRename forwards a rename, adding only the check this layer can make.
//
// It does not check that the two ends share a client: the viewer refused that
// case before the request was built, and by the time the paths arrive their
// client name has been stripped, so nothing here can tell two clients' identical
// paths apart. What it does check is that the client is named at all, and the
// owner applies the rule one level down — a rename stays inside one share —
// because the share map is the owner's to know.
func (s *Server) replyRename(c *proto.Conn, env proto.Envelope, b proto.RenameBody) error {
	if b.Client == "" {
		return s.writeAnswer(c, env, errors.New("bad rename request: client is required"))
	}
	// There is deliberately no check here that the two ends are in one place, and
	// one used to be: it split each path at the first separator and compared the
	// results. By this point the client name has been stripped, so the first
	// component is a *share* inside the client rather than a client name, and the
	// comparison was therefore asking a question the owner already answers.
	//
	// Both rules are enforced where they can actually be decided. The viewer
	// refuses a rename whose two ends name different clients, because it is the
	// only party that still has both names; and the owner refuses one whose two
	// ends name different shares, because only it knows its own share map. Each
	// of those has a fact this handler does not, so repeating the owner's check
	// here could only ever agree with it or be wrong.
	return s.replyNamed(c, env, b.Client, "rename")
}

// forwardPutContent reads one requester's content stream and hands each chunk to
// send, returning any truncation notice the requester sent.
//
// recvID and sendID are different on purpose: the requester's stream is
// numbered from its own view of the connection, while the owner sees the stream
// the router allocated for this forward. Reading on the forward's ID would look
// for frames the requester never sent — it is the requester's ID that names the
// bytes arriving here, and send is what re-stamps them for the owner.
//
// It is the forwarding half of replyPut, and it has two jobs beyond copying
// bytes. The first is the truncation notice: a requester that stops part-way
// says so on the stream, and the owner cannot see it otherwise — the end marker
// that follows is identical to a complete stream's — so the notice is returned
// to SendAnnounced, which relays it to the owner before ending the stream there.
// Without it, a put whose local reader died mid-file would publish the prefix
// over the target.
//
// The second is the update that may land between two chunks: RecvDataEvent hands
// one to the handler, and there is nothing to do with it here because this
// connection's updates belong to serveLoop, which is not reading while it is
// inside replyPut. Dropping it is right — the change is not lost, since serveLoop
// reads it as soon as the answer is written — and it must not be mistaken for a
// chunk of the file.
func (s *Server) forwardPutContent(c *proto.Conn, recvID uint64, send func([]byte) error) (proto.Envelope, error) {
	var notice proto.Envelope
	// The deadline is cleared on the way out, so it cannot be inherited by
	// whatever this connection reads next.
	defer c.SetReadDeadline(time.Time{})
	for {
		// Bounded per chunk. See chunkTimeout.
		if !c.SetReadDeadline(time.Now().Add(chunkTimeout)) {
			// Nothing will bound the wait, so the read cannot be allowed to
			// happen: an unbounded read here holds the owner's connection and
			// its write lock until the requester decides to speak again.
			return notice, errors.New("server: this connection cannot time out a read, so the content cannot be waited for")
		}
		chunk, done, err := c.RecvDataEvent(recvID, func(e proto.Envelope) {
			if e.Err != "" {
				notice = e
			}
		})
		if err != nil {
			return notice, err
		}
		if done {
			// The stream ended, and a notice that arrived before the marker is
			// the reason it ended early. Reporting it as a failure rather than
			// as a clean end is what makes SendAnnounced relay it to the owner:
			// returning nil here would end the owner's stream as though the
			// content were all there, and the prefix would be published.
			if notice.Err != "" {
				return notice, errors.New(notice.Err)
			}
			return notice, nil
		}
		if err := send(chunk); err != nil {
			return notice, err
		}
	}
}

// replyPut forwards a whole-file write-back, streaming the requester's content
// to the owner as it arrives.
func (s *Server) replyPut(c *proto.Conn, env proto.Envelope) error {
	var b proto.WriteBody
	switch {
	case json.Unmarshal(env.Body, &b) != nil:
		// The body is not readable as a put, so the content behind it is still
		// on this connection. It is drained before the answer, or the frames
		// would be read later as part of something else.
		return s.drainThenAnswer(c, env, errors.New("bad write request: unreadable body"))
	case b.Client == "":
		return s.drainThenAnswer(c, env, errors.New("bad write request: client is required"))
	case b.Path == "":
		return s.drainThenAnswer(c, env, errors.New("bad write request: path is required"))
	case b.Mode != proto.WriteModePut && b.Mode != proto.WriteModeCreate:
		return s.drainThenAnswer(c, env, fmt.Errorf("bad write request: unknown mode %q", b.Mode))
	}

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()

	// drained says the requester's content stream has been read to its end
	// marker. It is what the deferred drain below tests, because "the
	// forwarding failed" does not mean "the content was read":
	//
	// A forward that fails before it reaches the owner — the usual case, the
	// owner is gone — returns without ever calling the content function, and the
	// requester has already sent its frames by then. Those frames used to be
	// left on the connection: the requester read its answer and asked something
	// else, and this loop read the leftover chunk as the next request and failed
	// with "expected a control frame, got frame kind 2". The frame boundaries
	// from there on belong to a stream nobody is indexing.
	//
	// So every path out of the forwarding reads the stream, whichever end it
	// stops at.
	drained := false
	defer func() {
		if drained {
			return
		}
		// Bounded: the requester may still be sending, and this read is
		// cleanup rather than part of the answer. See drainTimeout.
		c.SetReadDeadline(time.Now().Add(drainTimeout))
		defer c.SetReadDeadline(time.Time{})
		for {
			_, done, err := c.RecvData(env.ID)
			if err != nil || done {
				return
			}
		}
	}()

	// The content is copied straight through, one chunk at a time: it is read
	// from the requester and written to the owner inside this call, so neither
	// side ever holds the file. The router keeps the owner's connection locked
	// for the whole sequence, so the stream cannot be interleaved with another.
	reply, err := s.router.ForwardWrite(ctx, b.Client, proto.Envelope{Kind: proto.KindWrite, Body: env.Body},
		func(id uint64, send func([]byte) error) (proto.Envelope, error) {
			notice, err := s.forwardPutContent(c, env.ID, send)
			if err == nil || notice.Err != "" {
				// The end marker was read. That is true both when the stream
				// ended cleanly and when it ended with a truncation notice: the
				// notice is written before the marker, so seeing one means the
				// marker was consumed too. Either way the deferred cleanup has
				// nothing left to read, and running it would consume the *next*
				// request — which is what happened when the notice path left
				// drained false: the drain read the following List request's
				// head off the wire and threw it away, and the requester waited
				// forever for an answer the server had already discarded.
				drained = true
			}
			return notice, err
		})
	if err != nil {
		return s.writeAnswer(c, env, err)
	}
	if reply.Err != "" {
		return s.writeAnswer(c, env, errors.New(reply.Err))
	}
	return s.writeAnswer(c, env, nil)
}

// drainThenAnswer reads a put's content off the connection and then reports a
// failure.
//
// Draining is not optional even though the request was refused: the requester
// has already sent the content by the time the server looks at the
// announcement, so the frames are on the wire and the only question is whether
// this connection reads them here or reads the wrong thing later.
func (s *Server) drainThenAnswer(c *proto.Conn, env proto.Envelope, reason error) error {
	// Bounded, like the cleanup drain at the end of a write. A requester can
	// announce a put and then say nothing more — an empty client name, an empty
	// path and an unknown mode are all refused before the content is wanted — and
	// without a deadline this read never returns, parking the handler, the
	// connection and this connection's write lock for the life of the process.
	// One silent peer would take the server's ability to answer anyone on that
	// connection with it.
	if !c.SetReadDeadline(time.Now().Add(drainTimeout)) {
		// The deadline could not be set, which means this read has no bound. The
		// answer is already decided, so send it rather than risk blocking here:
		// the requester gets its refusal, and the frames it withheld stay
		// unread — which the next request on this connection would notice, but
		// that is a lesser failure than never answering at all.
		return s.writeAnswer(c, env, reason)
	}
	defer c.SetReadDeadline(time.Time{})

	for {
		_, done, err := c.RecvData(env.ID)
		if err != nil {
			return s.writeAnswer(c, env, reason)
		}
		if done {
			break
		}
	}
	return s.writeAnswer(c, env, reason)
}

// writeAnswer sends the three-part answer a write always gets, and reports
// whether writing it failed.
//
// reason is what to tell the requester, or nil for success. It never varies the
// shape: the head carries the outcome, the stream is empty and ended, and the
// tail closes it.
//
// The kind is KindWriteOK and not the request's own kind. Echoing the request
// would make the answer and the next request the same thing on the wire, and the
// requester's assertion that it got an answer would then hold for a request too.
func (s *Server) writeAnswer(c *proto.Conn, env proto.Envelope, reason error) error {
	head := proto.Envelope{ID: env.ID, Kind: proto.KindWriteOK}
	if reason != nil {
		head.Err = reason.Error()
	}
	if err := c.Send(head); err != nil {
		return err
	}
	if err := c.SendData(env.ID, nil); err != nil {
		return err
	}
	return c.Send(head)
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
