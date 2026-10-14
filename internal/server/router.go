package server

import (
	"context"
	"fmt"
	"sync"

	"peerfold/internal/proto"
)

// Router forwards a request to the client that owns a path and returns that
// client's answer.
//
// Each forwarded request gets its own ID and its own waiter, so answers cannot
// be confused with each other. The connection is written through proto.Conn,
// which serialises frames, so a forwarded request never interleaves with
// another.
//
// A request whose answer carries content is special: the content is a stream
// that has to be read off the owner's connection, and only one goroutine may
// read a connection. So the owner's own serve loop reads that stream and
// forwards it, rather than the requester's goroutine reaching across to read
// someone else's socket. That is why this type knows both ends of a request.
type Router struct {
	mu      sync.Mutex
	clients map[string]*clientLink
}

// clientLink is one connected client as the router sees it: the connection to
// write on and the forwarded requests waiting for an answer.
type clientLink struct {
	conn *proto.Conn
	next uint64
	// pending maps a forwarded request's ID to its waiter. It is guarded by the
	// Router's mutex, because an answer arrives on the owner's serve loop while
	// the request is registered on the requester's goroutine.
	pending map[uint64]*forwarded
}

// forwarded is one in-flight request: who is waiting, and where an answer goes.
type forwarded struct {
	// ch receives the envelope that wakes the waiter.
	ch chan proto.Envelope
	// back is the requester's connection, and backID the request ID that
	// requester is waiting on. Together they are how streamed content is
	// re-stamped on the way through: the owner answers the router's ID, the
	// requester waits on its own.
	back   *proto.Conn
	backID uint64
	// reply is the owner's closing control frame, recorded so the waiter can be
	// woken with it once the stream has been dealt with.
	reply proto.Envelope
}

// Router returns the request router for this server.
func (s *Server) Router() *Router { return s.router }

// attach records a client's connection so requests can be forwarded to it.
func (r *Router) attach(name string, c *proto.Conn) {
	if name == "" {
		// A viewer owns nothing, so nothing can be forwarded to it.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[name] = &clientLink{conn: c, pending: make(map[uint64]*forwarded)}
}

// detach drops a client's connection and releases every caller waiting on it,
// so a request in flight when its owner disconnects fails instead of blocking
// until its timeout.
func (r *Router) detach(name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	link, ok := r.clients[name]
	delete(r.clients, name)
	if ok {
		// Unregistering and releasing happen under one lock hold: releasing
		// first would leave a window where an answer for an already-released
		// request is looked up and dropped as unknown.
		for id, f := range link.pending {
			close(f.ch)
			delete(link.pending, id)
		}
	}
	r.mu.Unlock()
}

// connOf returns a connected client's connection.
func (r *Router) connOf(name string) (*proto.Conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	link, ok := r.clients[name]
	if !ok {
		return nil, false
	}
	return link.conn, true
}

// deliver wakes the waiter for a reply that carries no content.
//
// It reports whether a waiter existed. A reply with no waiter is a peer
// answering something it was not asked, which is a protocol error rather than
// noise to discard.
func (r *Router) deliver(name string, env proto.Envelope) bool {
	r.mu.Lock()
	link, ok := r.clients[name]
	if !ok {
		r.mu.Unlock()
		return false
	}
	f, ok := link.pending[env.ID]
	if ok {
		delete(link.pending, env.ID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	f.ch <- env
	return true
}

// complete handles a reply from an owner, forwarding the content it announced.
//
// It runs on the owner's serve loop, which is the only goroutine allowed to read
// the owner's connection. The requester's goroutine cannot reach across and read
// someone else's socket, and if the owner's loop simply resumed reading, the two
// would shred the stream between them: one would consume the content while the
// other took the following frame, each resynchronising on the other's leftovers.
// So the loop does the reading, one chunk at a time, straight onto the
// requester's connection.
//
// A read answer is three parts: a head frame, a data stream, then a tail frame.
// All three are read here — the head is what this function is handed, the stream
// has to be drained before control frames can be read again, and the tail is what
// says the answer finished and how it went.
//
// It reports whether this envelope answered an outstanding request.
func (r *Router) complete(name string, env proto.Envelope) bool {
	r.mu.Lock()
	link, ok := r.clients[name]
	if !ok {
		r.mu.Unlock()
		return false
	}
	f, ok := link.pending[env.ID]
	if !ok {
		r.mu.Unlock()
		return false
	}
	back, backID := f.back, f.backID
	r.mu.Unlock()

	fmt.Printf("PROBE complete name=%q id=%d err=%q\n", name, env.ID, env.Err)
	if _, err := pumpStream(link.conn, env.ID, back, backID); err != nil {
		r.settle(name, env.ID, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()})
		return true
	}
	tail, err := link.conn.Recv()
	fmt.Printf("PROBE tail id=%d err=%q recverr=%v\n", tail.ID, tail.Err, err)
	if err != nil {
		r.settle(name, env.ID, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: fmt.Sprintf("the answer's stream ended without its closing frame: %v", err)})
		return true
	}
	if tail.ID != env.ID {
		r.settle(name, env.ID, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: fmt.Sprintf("the answer's closing frame is for request %d, want %d", tail.ID, env.ID)})
		return true
	}
	r.settle(name, env.ID, tail)
	return true
}

// settle takes a request out of the pending table and wakes its waiter.
//
// It is the one exit for a completed request, so a waiter is never left behind
// by a path that forgot to release it.
func (r *Router) settle(name string, id uint64, reply proto.Envelope) {
	r.mu.Lock()
	link, ok := r.clients[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	f, ok := link.pending[id]
	if ok {
		delete(link.pending, id)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	f.ch <- reply
}

// pumpStream copies a data stream from one connection to another.
//
// Frames are re-stamped as they pass through, one at a time, so neither side
// holds more than one chunk of a file that may be far larger than memory.
//
// A write failure does not stop the reading. Frames are one sequence, so a
// half-read stream would leave the owner's next frame being parsed as content.
// The remainder is read and dropped, and the first failure is what the caller is
// told about.
func pumpStream(from *proto.Conn, fromID uint64, to *proto.Conn, toID uint64) (int, error) {
	var first error
	var frames int
	stop := false
	for {
		chunk, done, err := from.RecvData(fromID)
		if err != nil {
			if first == nil {
				first = err
			}
			break
		}
		if done {
			if !stop && to != nil {
				if err := to.SendData(toID, nil); err != nil && first == nil {
					first = err
				}
			}
			break
		}
		frames++
		if stop || to == nil {
			// Still draining: the chunk is read so the stream stays in step, and
			// discarded because its destination is gone.
			continue
		}
		if err := to.SendData(toID, chunk); err != nil {
			first = err
			stop = true
		}
	}
	return frames, first
}

// isReply reports whether env is an answer to a forwarded request rather than a
// message a client originated on its own.
//
// A client originates tree uploads and events; everything else on this
// connection exists only because the server asked for it, so a kind the server
// forwards is a reply by construction.
func isReply(env proto.Envelope) bool {
	switch env.Kind {
	case proto.KindRead, proto.KindWrite, proto.KindMkdir, proto.KindCreate,
		proto.KindRename, proto.KindRemove:
		return true
	}
	return false
}

// Send forwards env to a named client and returns the reply that carries the
// same ID. It waits until the reply arrives, ctx is done, or the client
// disconnects.
//
// back and backID describe the requester, so content the owner streams can be
// forwarded without the requester reading the owner's connection itself.
//
// The caller supplies ctx, so the deadline is the caller's to choose: a listing
// can afford to wait far longer than a read a user is staring at, and one number
// baked in here would get one of them wrong.
func (r *Router) Send(ctx context.Context, client string, env proto.Envelope, back *proto.Conn, backID uint64) (proto.Envelope, error) {
	r.mu.Lock()
	link, ok := r.clients[client]
	if !ok {
		r.mu.Unlock()
		return proto.Envelope{}, fmt.Errorf("%w: %q", ErrClientOffline, client)
	}
	link.next++
	id := link.next
	f := &forwarded{ch: make(chan proto.Envelope, 1), back: back, backID: backID}
	link.pending[id] = f
	r.mu.Unlock()

	env.ID = id
	if err := link.conn.Send(env); err != nil {
		r.settle(client, id, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()})
		return proto.Envelope{}, fmt.Errorf("server: forward to %q: %w", client, err)
	}

	select {
	case reply, ok := <-f.ch:
		if !ok {
			// detach closed the channel: the owner went away while this
			// request was in flight.
			return proto.Envelope{}, fmt.Errorf("%w: %q disconnected", ErrClientOffline, client)
		}
		if reply.Err != "" {
			return reply, fmt.Errorf("%w: %s", ErrNotFound, reply.Err)
		}
		return reply, nil
	case <-ctx.Done():
		r.mu.Lock()
		delete(link.pending, id)
		r.mu.Unlock()
		return proto.Envelope{}, fmt.Errorf("server: %q did not answer: %w", client, ctx.Err())
	}
}
