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
// A request whose answer carries content is special. Only one goroutine may read
// a connection, and for the owner that goroutine is its serve loop. So the loop
// reads the whole answer — control frame and content — and hands it to the
// requester, rather than the requester reaching across to read someone else's
// socket. The loop must finish before it returns: the moment it does, it resumes
// reading control frames, and a second reader would take the first content frame
// as a control frame and have both sides resynchronising on the other's
// leftovers. That is also why the content is collected rather than streamed
// through a channel: a channel would let the loop return while content was still
// outstanding.
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

// forwarded is one in-flight request: who is waiting for it.
type forwarded struct {
	// ch receives the thing that wakes the waiter: the owner's control frame for
	// an answer that carries content, or a reply that never came from the owner
	// at all.
	ch chan settled
	// streaming marks a request whose answer carries content rather than being a
	// plain reply.
	streaming bool
	// content carries the collected content of an answer that has it, once the
	// control frame has been sent on ch.
	content chan answerContent
}

// settled is what wakes a waiter.
//
// It carries an error alongside the envelope because the two ways a request can
// end without an answer — the owner disconnected, or the request was given up on
// — are values the caller has to be able to recognise, and an error flattened
// into a string in the envelope cannot be recognised by anyone. Passing it
// through unchanged is what lets a caller ask whether the owner is offline
// rather than matching on the wording of a message.
type settled struct {
	env proto.Envelope
	err error
}

// answerContent is the content half of an answer that carries it.
type answerContent struct {
	// body is the content with the peer's end marker stripped. It is nil for an
	// empty answer, which is a zero-byte file as much as it is a failed read
	// whose reason is in the control frame.
	body []byte
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
	if ok && !f.streaming {
		// A streaming request stays registered: its content is still on the
		// wire, and complete is the only thing that may read it, so complete
		// clears the entry.
		delete(link.pending, env.ID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	f.ch <- settled{env: env}
	return true
}

// complete handles a reply from an owner whose answer carries content.
//
// A read answer is three parts: a control frame, the content as a data stream,
// and a closing control frame. All three carry the same kind and ID. The closing
// frame is not decoration — frames are one sequence, so a part left unread does
// not stay where it is; it becomes the serve loop's next control frame, and the
// answer after this one is read from the middle of the wrong stream.
//
// It runs on the owner's serve loop, because that is the only goroutine allowed
// to read the owner's connection, and it does not return until every part has
// been read.
//
// It reports whether this envelope answered an outstanding request.
func (r *Router) complete(name string, env proto.Envelope, onEvent func(proto.Envelope)) bool {
	// A pushed update can land in the middle of an answer, and it is not a
	// reason to lose it. The owner sends its answer as several frames — a head, a
	// stream, a closing frame — and its watcher may emit between any two of them.
	// A body read with nowhere to put an event drops it, and a dropped event is
	// permanent: nothing rescans, so the index would stay wrong until the client
	// reconnected. That is the aggregate's only source of truth silently
	// disagreeing with the supplier's disk.
	//
	// The caller supplies the handler because applying an event needs the name it
	// belongs to and the index to apply it to, and the router has neither.
	handle := onEvent
	if handle == nil {
		handle = func(proto.Envelope) {}
	}

	r.mu.Lock()
	link, ok := r.clients[name]
	if !ok {
		r.mu.Unlock()
		return false
	}
	f, ok := link.pending[env.ID]
	if !ok {
		// The request has already been given up on. The rest of the answer is
		// still on the wire, so it is read off before control frames can be read
		// again.
		r.mu.Unlock()
		_ = pumpStream(link.conn, env.ID, nil, handle)
		_, _ = link.conn.RecvEvent(handle)
		return true
	}
	streaming := f.streaming
	delete(link.pending, env.ID)
	r.mu.Unlock()

	// The content is read here, on this goroutine, before anything is handed
	// over. An answer that claims content has to be read off even when nobody
	// wants it, or the next control frame would be parsed out of its middle.
	//
	// Every answer is read this way, whatever its kind: a data stream terminated
	// by an end marker is the shape of an answer from a client, and the
	// alternative — reading frames until something looks like a control frame —
	// would make the frame count depend on the bytes inside a file.
	var body []byte
	readErr := pumpStream(link.conn, env.ID, &body, handle)
	if readErr != nil {
		// The frame is still delivered, with the reading failure attached: a
		// truncated file and a complete one must not look the same.
		if env.Err == "" {
			env.Err = fmt.Sprintf("the answer's content ended early: %v", readErr)
		}
	}
	// The owner's closing frame is read and discarded: the requester's own closing
	// frame is written by the requester, from what is handed over here, so the
	// contents of the owner's cannot reach anyone. What reading it does is keep
	// the connection at a frame boundary — a part of an answer left unread becomes
	// the next control frame, and the following request is then parsed out of this
	// answer's middle.
	//
	// RecvEvent, not Recv: this is the other place an update can land, and Recv
	// would skip it into nothing. What is read here is the closing frame either
	// way — the difference is only what happens to an event that arrives first.
	if _, err := link.conn.RecvEvent(handle); err != nil {
		// The connection ended inside the answer. The answer is already complete
		// and is handed over as such; there is nothing left to read next, so the
		// serve loop will see the end of the connection on its own.
		_ = err
	}
	// The control frame wakes the waiter only now, when the whole answer is in
	// hand, so the waiter can put the head on the wire and then the content
	// knowing nothing else is arriving behind it.
	f.ch <- settled{env: env}
	if streaming {
		f.content <- answerContent{body: body}
	}
	return true
}

// pumpStream reads a data stream off a connection, appending its content to body
// or discarding it when body is nil.
//
// Frames are read one at a time so neither end ever holds more than one chunk of
// a file that may be far larger than memory. The peer's end marker is what ends
// the stream, so an answer with no content is an empty stream and its reason
// travels in the control frame that precedes it.
func pumpStream(from *proto.Conn, fromID uint64, body *[]byte, onEvent func(proto.Envelope)) error {
	for {
		chunk, done, err := from.RecvDataEvent(fromID, onEvent)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if body != nil {
			*body = append(*body, chunk...)
		}
	}
}

// settle takes a request out of the pending table and wakes its waiter.
//
// It is the one exit for a request that is not answered by the owner.
//
// The error is carried separately from the envelope rather than flattened into
// its Err field, so the waiter can still recognise it: a caller that wants to
// know whether the owner is offline needs the value, not a sentence about it.
func (r *Router) settle(name string, id uint64, reply proto.Envelope, reason error) {
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
	f.ch <- settled{env: reply, err: reason}
}

// isWriteRequest reports whether kind is one of the write requests the server
// forwards to the client that owns the path.
//
// The request kinds must not appear in isReply, and the two must not overlap: a
// request and its answer are the same shape in both directions, so a kind that
// matched both would have the server read its own request as the answer to it.
func isWriteRequest(kind string) bool {
	switch kind {
	case proto.KindWrite, proto.KindMkdir, proto.KindRemove, proto.KindRename:
		return true
	}
	return false
}

// isReply reports whether env is an answer to a forwarded request rather than a
// message a client originated on its own.
//
// A client originates tree uploads and events; everything else on this
// connection exists only because the server asked for it, so a kind the server
// forwards is a reply by construction.
//
// The request kinds must not appear here. A request and its answer are the same
// shape in both directions — a control frame, and for a write a content stream —
// so classifying a message by what it looks like rather than by which direction
// it came from reads the owner's answer as a second request. That is exactly
// what made every read hang before KindReadOK existed.
func isReply(env proto.Envelope) bool {
	switch env.Kind {
	case proto.KindReadOK, proto.KindWriteOK:
		return true
	}
	return false
}

// Send forwards env to a named client and returns the reply that carries the
// same ID. It waits until the reply arrives, ctx is done, or the client
// disconnects.
//
// A KindRead answer is a control frame plus content: the frame comes back here
// and the content on the returned channel, which is delivered once, after the
// frame. Any other kind's answer is the envelope itself and the channel is nil.
//
// The caller supplies ctx, so the deadline is the caller's to choose: a listing
// can afford to wait far longer than a read a user is staring at.
func (r *Router) Send(ctx context.Context, client string, env proto.Envelope) (proto.Envelope, <-chan answerContent, error) {
	r.mu.Lock()
	link, ok := r.clients[client]
	if !ok {
		r.mu.Unlock()
		return proto.Envelope{}, nil, fmt.Errorf("%w: %q", ErrClientOffline, client)
	}
	link.next++
	id := link.next
	f := &forwarded{ch: make(chan settled, 1)}
	var content chan answerContent
	if env.Kind == proto.KindRead {
		// A read's content is the answer, and it is handed to the caller to
		// forward. A write's content travels the other way — from the caller to
		// the owner — so the answer's own stream is empty and read off in
		// complete instead.
		f.streaming = true
		content = make(chan answerContent, 1)
		f.content = content
	}
	link.pending[id] = f
	r.mu.Unlock()

	env.ID = id
	if err := link.conn.Send(env); err != nil {
		r.settle(client, id, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()}, err)
		return proto.Envelope{}, nil, fmt.Errorf("server: forward to %q: %w", client, err)
	}

	select {
	case got, ok := <-f.ch:
		if !ok {
			// detach closed the channel: the owner went away while this request
			// was in flight.
			return proto.Envelope{}, nil, fmt.Errorf("%w: %q disconnected", ErrClientOffline, client)
		}
		if got.err != nil {
			return proto.Envelope{}, nil, got.err
		}
		return got.env, content, nil
	case <-ctx.Done():
		r.settle(client, id, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: ctx.Err().Error()}, ctx.Err())
		return proto.Envelope{}, nil, fmt.Errorf("server: %q did not answer: %w", client, ctx.Err())
	}
}

// ForwardWrite sends a write request to a named client and waits for its answer.
//
// It differs from Send in the one way that matters: the content of a write
// travels from the server to the owner, so it has to be written to the owner's
// connection while the request announcing it is still in flight. That is what
// content is for — the router calls it once the announcement is on the wire, and
// the chunks it sends become a data stream ended by the marker.
//
// The connection's write mutex is held across the whole of it, which is the
// reason this method exists rather than the caller sending the frames itself: no
// other frame may land between the announcement and its content.
//
// content is nil for a request that carries no bytes. It may return a notice
// alongside its error — an envelope the peer at the other end sent about why the
// content stopped — which SendAnnounced relays to the target so a truncated
// stream is not taken for a whole file.
func (r *Router) ForwardWrite(ctx context.Context, client string, env proto.Envelope, content func(id uint64, send func([]byte) error) (proto.Envelope, error)) (proto.Envelope, error) {
	r.mu.Lock()
	link, ok := r.clients[client]
	if !ok {
		r.mu.Unlock()
		return proto.Envelope{}, fmt.Errorf("%w: %q", ErrClientOffline, client)
	}
	link.next++
	id := link.next
	f := &forwarded{ch: make(chan settled, 1)}
	link.pending[id] = f
	r.mu.Unlock()

	env.ID = id
	if err := link.conn.SendAnnounced(id, env, content); err != nil {
		r.settle(client, id, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: err.Error()}, err)
		return proto.Envelope{}, fmt.Errorf("server: forward to %q: %w", client, err)
	}

	select {
	case got, ok := <-f.ch:
		if !ok {
			return proto.Envelope{}, fmt.Errorf("%w: %q disconnected", ErrClientOffline, client)
		}
		if got.err != nil {
			return proto.Envelope{}, got.err
		}
		return got.env, nil
	case <-ctx.Done():
		r.settle(client, id, proto.Envelope{ID: env.ID, Kind: env.Kind, Err: ctx.Err().Error()}, ctx.Err())
		return proto.Envelope{}, fmt.Errorf("server: %q did not answer: %w", client, ctx.Err())
	}
}
