package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ispengya/peerfold/internal/logging"
	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

const (
	// dialTimeout bounds connecting and the handshake.
	dialTimeout = 10 * time.Second
	// ackTimeout bounds a wait for a reply that is actually outstanding. It is
	// generous because a server may be reading a large tree, but finite because
	// a server that goes quiet must fail the call rather than hang the client.
	ackTimeout = 30 * time.Second
	// idleTimeout is the read loop's backstop when nothing is outstanding. It
	// exists so a request that arrives while the loop is parked still has a
	// point at which the loop notices a dead connection; it is far longer than
	// ackTimeout because an idle supplier is a normal, long-lived state.
	idleTimeout = 15 * time.Minute
	// maxBatchBody is the largest body a tree batch may encode to.
	//
	// Half of one frame's payload, not all of it: the envelope around the body
	// costs bytes too, and a batch that only just fits would be refused by the
	// frame writer with the tree already half sent.
	maxBatchBody = proto.MaxPayload / 2

	// DefaultShutdownGrace is how long Shutdown waits for requests already in
	// flight. It is shorter than the stop command's own grace, so the process
	// gets to say goodbye rather than being killed halfway through.
	DefaultShutdownGrace = 5 * time.Second

	// byeTimeout bounds the wait for the server's acknowledgement of the
	// goodbye. The connection close that follows withdraws the tree anyway, so
	// a server that has stopped answering is not worth waiting longer for.
	byeTimeout = 2 * time.Second
)

// errShuttingDown is the answer a forwarded request gets once Shutdown has
// started and the request was not already being served.
//
// It is different from a transport failure: the connection is healthy and the
// data is intact, this client simply will not start new work. The mount maps it
// to EIO like any other owner-side refusal.
var errShuttingDown = errors.New("client is shutting down")

// errContentTruncated reports that the server said the content of a write
// stopped part-way.
//
// It is a sentinel rather than a message because the write must be abandoned
// when it happens, and that decision is made by whoever asks the sink's verify
// hook: a prefix of a file looks exactly like a whole file to every later
// reader, so publishing it is silent data loss.
var errContentTruncated = errors.New("the content was cut short by the sender")

// Options configures a Client.
type Options struct {
	// Server is the address to dial.
	Server string
	// Name is this client's name in the aggregated tree.
	Name string
	// Shares are the directories this client supplies.
	Shares Shares
	// Aggregate asks to be the client that may also aggregate other clients'
	// trees. The server decides; the reply is what this client believes.
	Aggregate bool
	// Role is the role to claim in the handshake, or "" for an ordinary client.
	//
	// The client package is where the handshake lives, so tests that need to pin
	// what the server does with a role it did not expect dial through here rather
	// than reaching past it.
	Role string
	// OnBatch, when set, is called with the number of nodes uploaded so far.
	OnBatch func(int)
	// Batch is the largest number of nodes in one upload batch, or zero for
	// DefaultBatch.
	Batch int
}

// Client is one connection to a server, supplying the trees of its shares.
//
// Exactly one goroutine reads the connection: the read loop started by Dial.
// Everything that arrives — batch acknowledgements, the answer to a request, a
// request to read a file — is dispatched from there, so no two readers ever
// interleave on the same frames.
type Client struct {
	conn      net.Conn
	pc        *proto.Conn
	fs        *FS
	shares    Shares
	name      string
	batch     int
	aggregate bool

	mu        sync.Mutex
	onBatch   func(int)
	nextID    uint64
	waiting   map[uint64]chan proto.Envelope
	acks      int
	loopErr   error
	closed    bool
	loopEnded chan struct{}
	done      chan struct{}

	// serving counts the forwarded requests the read loop is inside right now.
	// Shutdown waits for it to reach zero, so an upload that has already begun
	// is finished rather than cut off mid-stream.
	serving int
	// draining makes new forwarded requests be refused instead of started.
	// Existing ones are unaffected: they were admitted before this was set.
	draining bool
	// idle is closed when draining is set and serving reaches zero. It is nil
	// when nobody is waiting, and never closed twice: endServe clears it after
	// closing it.
	idle chan struct{}
}

// Dial connects to addr, shakes hands, and starts serving requests.
//
// name is this client's name in the aggregated tree. The shares are what it
// supplies; a client with no shares is legal and supplies an empty tree.
func Dial(opt Options) (*Client, error) {
	raw, err := net.DialTimeout("tcp", opt.Server, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", opt.Server, err)
	}
	c := &Client{
		conn:      raw,
		pc:        proto.NewConn(raw),
		fs:        NewFS(opt.Shares),
		shares:    opt.Shares,
		name:      opt.Name,
		batch:     opt.Batch,
		onBatch:   opt.OnBatch,
		waiting:   make(map[uint64]chan proto.Envelope),
		loopEnded: make(chan struct{}),
		done:      make(chan struct{}),
	}

	// The handshake is done inline, before the read loop starts, so there is
	// never a moment with two readers and the name is known before the loop can
	// be asked to serve a read of it.
	if err := raw.SetDeadline(time.Now().Add(dialTimeout)); err != nil {
		raw.Close()
		return nil, fmt.Errorf("client: set deadline: %w", err)
	}
	role := opt.Role
	if role == "" {
		role = proto.RoleClient
	}
	body, err := json.Marshal(proto.HelloBody{Name: opt.Name, Role: role, Aggregate: opt.Aggregate})
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("client: encode hello: %w", err)
	}
	if err := c.pc.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
		raw.Close()
		return nil, fmt.Errorf("client: send hello: %w", err)
	}
	env, err := c.recvWithin(dialTimeout)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("client: read hello reply: %w", err)
	}
	if env.Err != "" {
		raw.Close()
		return nil, fmt.Errorf("client: server refused the handshake: %s", env.Err)
	}
	var ok proto.HelloOKBody
	if err := json.Unmarshal(env.Body, &ok); err != nil {
		raw.Close()
		return nil, fmt.Errorf("client: decode hello reply: %w", err)
	}
	// The server's ruling is what this client answers reads under. Taking it from
	// the reply rather than from the request keeps the two ends agreeing even if
	// the request asked for something the server declined.
	c.name = ok.Name
	c.aggregate = ok.Aggregate
	if err := raw.SetDeadline(time.Time{}); err != nil {
		raw.Close()
		return nil, fmt.Errorf("client: clear deadline: %w", err)
	}

	go c.readLoop()
	return c, nil
}

// Name is the name the server registered this client under.
func (c *Client) Name() string { return c.name }

// Aggregate reports whether the server accepted this client as the aggregating
// one, which is what lets it serve other clients' trees too.
func (c *Client) Aggregate() bool { return c.aggregate }

// Err reports why the read loop stopped, or nil while it is running.
//
// It exists because the loop's failure is otherwise invisible from outside this
// package: the connection dies, the server withdraws every node this client
// published, and a caller that only holds the *Client has nothing to look at.
// A long-running process that cannot report its own death is worse than one that
// crashes, because it looks healthy.
func (c *Client) Err() error { return c.loopFailure() }

// Done is closed when the read loop has stopped, so a caller can wait on the
// connection ending instead of polling Err.
//
// It is loopEnded, not done: done means the caller closed this client, which is
// the one exit that is not a failure.
func (c *Client) Done() <-chan struct{} { return c.loopEnded }

// SetOnBatch registers a callback for upload progress, called with the number of
// nodes sent so far.
func (c *Client) SetOnBatch(f func(int)) {
	c.mu.Lock()
	c.onBatch = f
	c.mu.Unlock()
}

// onBatchCount reports upload progress, if anyone is listening.
func (c *Client) onBatchCount(n int) {
	c.mu.Lock()
	f := c.onBatch
	c.mu.Unlock()
	if f != nil {
		f(n)
	}
}

// uploadWindow is how many tree batches may be in flight before the uploader
// waits for an acknowledgement.
//
// The upload used to be one batch out, one acknowledgement in. On a 50 ms link
// a hundred-thousand-file tree is two hundred batches and therefore two hundred
// round trips of nothing but waiting; the server applies each batch in
// microseconds. Keeping a handful in flight hides that latency while the
// server's acknowledgement remains the backpressure signal, so a fast uploader
// cannot outrun a server that is still indexing.
const uploadWindow = 4

// UploadTree sends the whole shared tree as a sequence of batches, ending with a
// tree_done that is marked as the end of the upload.
//
// It waits for the tree_done acknowledgement, so returning nil means the server
// has the tree rather than merely that it was written to a socket. Because the
// connection is a single ordered stream, that acknowledgement also proves every
// batch before it was applied: the server reads and indexes batches in order
// before it reads the tree_done.
func (c *Client) UploadTree() error {
	batch := c.batchSize()
	pending := make([]node.Node, 0, batch)
	sent := 0

	// ackBase is the acknowledgement count when the upload started. The window
	// is measured against acks received for this upload rather than against the
	// client's lifetime total, which may already include earlier traffic.
	ackBase := c.ackCount()
	batchesSent := 0

	encode := func() ([]byte, error) {
		return json.Marshal(proto.TreeBatchBody{Nodes: toNodeJSON(pending)})
	}

	sendBatch := func(b []byte) error {
		for batchesSent-(c.ackCount()-ackBase) >= uploadWindow {
			acked := c.ackCount()
			if err := c.waitForAck(acked); err != nil {
				return err
			}
		}
		if _, err := c.send(proto.KindTreeBatch, b, false); err != nil {
			return err
		}
		batchesSent++
		return nil
	}

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		b, err := encode()
		if err != nil {
			return fmt.Errorf("client: encode tree batch: %w", err)
		}
		if err := sendBatch(b); err != nil {
			return err
		}
		sent += len(pending)
		pending = pending[:0]
		c.onBatchCount(sent)
		return nil
	}

	// overflow reports whether pending, as it stands, is over the byte budget.
	overflow := func() (bool, error) {
		b, err := encode()
		if err != nil {
			return false, fmt.Errorf("client: encode tree batch: %w", err)
		}
		return len(b) > maxBatchBody, nil
	}

	// add buffers one node and flushes when either limit is reached.
	add := func(n node.Node) error {
		// A lone node is always sent on its own, so a single path longer than
		// the whole budget cannot wedge the upload: WriteFrame reports the real
		// limit rather than the client looping for ever trying to fit it.
		if len(pending) > 0 {
			over, err := overflow()
			if err != nil {
				return err
			}
			if over || len(pending) >= batch {
				// Whatever is buffered is what fits. A batch is bounded by
				// bytes as well as by count, because a thousand long paths
				// encode past one frame even though a thousand is the
				// configured number of entries.
				if err := flush(); err != nil {
					return err
				}
			}
		}
		pending = append(pending, n)
		return nil
	}

	if err := Scan(c.shares, batch, func(nodes []node.Node) error {
		for _, n := range nodes {
			if err := add(n); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}

	shares, err := json.Marshal(proto.TreeDoneBody{Shares: c.shares.Names()})
	if err != nil {
		return fmt.Errorf("client: encode tree done: %w", err)
	}
	// eof marks this as the end of the upload. The server refuses a tree_done
	// without it, so a client whose last batch was lost cannot look finished.
	if _, err := c.sendAndWait(proto.KindTreeDone, shares, true, "the upload acknowledgement"); err != nil {
		return err
	}
	return nil
}

// toNodeJSON converts index nodes to their wire form.
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

// readLoop is the connection's only reader.
//
// It dispatches everything the server sends: batch acknowledgements, replies to
// requests this client made, and read requests, which it answers inline so the
// content stream is consumed before the loop looks for the next frame.
func (c *Client) readLoop() {
	defer close(c.loopEnded)
	for {
		// Park with a deadline. Something outstanding uses the request timeout;
		// nothing outstanding uses the idle backstop, so a request that arrives
		// while the loop is already parked still has a bound.
		timeout := idleTimeout
		if c.anyWaiting() {
			timeout = ackTimeout
		}
		if err := c.conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			c.endLoop(err)
			return
		}

		env, err := c.pc.Recv()
		if err != nil {
			if isTimeout(err) && !c.anyWaiting() {
				// Nothing was outstanding, so this is the backstop expiring on
				// an idle connection. Stay up.
				continue
			}
			debugf("client read loop: %v", err)
			c.endLoop(err)
			return
		}

		if env.Kind == proto.KindRead && env.ID != 0 {
			c.beginServe()
			err := c.satisfyRead(env)
			c.endServe()
			if err != nil {
				debugf("client serving a read: %v", err)
				c.endLoop(err)
				return
			}
			continue
		}
		if isWriteRequest(env.Kind) && env.ID != 0 {
			c.beginServe()
			err := c.satisfyWrite(env)
			c.endServe()
			if err != nil {
				debugf("client serving a write: %v", err)
				c.endLoop(err)
				return
			}
			continue
		}
		c.deliver(env)
	}
}

// endLoop publishes the loop's failure and releases everything waiting.
//
// The failure is what makes a dead connection fail fast: without it a request
// would wait out its whole timeout to learn what the loop already knew.
func (c *Client) endLoop(err error) {
	c.mu.Lock()
	if c.loopErr == nil {
		c.loopErr = err
	}
	pending := c.waiting
	c.waiting = make(map[uint64]chan proto.Envelope)
	c.mu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

// deliver hands an envelope to whoever is waiting for its ID.
//
// An envelope with ID 0 and an error is the server's out-of-band diagnostic: it
// answers no request, so it is reported to nothing and the loop ends, since
// whatever produced it has already decided the connection is unusable. A batch
// acknowledgement also carries no ID; it is a flow-control token, counted.
//
// An envelope with an ID nobody is waiting for is dropped rather than treated as
// fatal: the request may have timed out a moment earlier, and a late answer to a
// request that is no longer wanted is not a reason to tear down a working
// connection.
func (c *Client) deliver(env proto.Envelope) {
	if env.ID == 0 {
		if env.Err != "" {
			debugf("server reported: %s", env.Err)
			c.endLoop(fmt.Errorf("server: %s", env.Err))
			return
		}
		c.countAck()
		return
	}
	c.mu.Lock()
	ch, ok := c.waiting[env.ID]
	if ok {
		delete(c.waiting, env.ID)
	}
	c.mu.Unlock()
	if !ok {
		debugf("dropped a reply for unknown request %d", env.ID)
		return
	}
	ch <- env
}

// satisfyRead answers a kind_read request with three parts, in order: a control
// frame saying whether the file could be read, the content as a data stream, and
// a control frame closing the answer.
//
// Two control frames rather than one because they answer different questions.
// The head frame is what the server reads before the content, and it is where
// "no such file" lives: the content stream alone cannot tell a deleted file from
// a zero-byte one. The tail frame is what unblocks the server's forwarding loop,
// which has to have finished reading the stream before it can resume reading
// control frames off this connection.
func (c *Client) satisfyRead(env proto.Envelope) error {
	started := time.Now()
	var req proto.ReadBody
	reason, data, ok := error(nil), []byte(nil), false
	if c.shuttingDown() {
		// The request was read after Shutdown began, so it is refused rather than
		// started. The answer keeps the three-part shape every read answer has,
		// or the server's frame sequence would no longer line up.
		reason = errShuttingDown
	} else if err := json.Unmarshal(env.Body, &req); err != nil {
		reason = fmt.Errorf("bad read request: %v", err)
	} else if req.Client != c.name {
		// The request names a client this one is not. That is a server-side
		// routing mistake, and answering it would read the wrong machine's disk.
		reason = fmt.Errorf("read request is addressed to %q, not %q", req.Client, c.name)
	} else {
		logging.Infof("client: read %s off=%d n=%d", req.Path, req.Off, req.N)
		// req.Client is this client's own name, which is how the server routes
		// the request; the share is the first component of the path, because
		// everything after the client name belongs to this client's namespace.
		var err error
		data, _, err = c.fs.ReadAt(shareOf(req.Path), restOf(req.Path), req.Off, req.N)
		if err != nil {
			logging.Errorf("client: read %s failed: %v", req.Path, err)
			reason = err
		} else {
			ok = true
		}
	}

	logging.Infof("client: read %s off=%d n=%d bytes=%d took=%s err=%v",
		req.Path, req.Off, req.N, len(data), time.Since(started), reason)

	head := proto.Envelope{ID: env.ID, Kind: proto.KindReadOK}
	if reason != nil {
		// The reason is reported, not swallowed: this is the only place a user
		// can learn why the read failed.
		head.Err = reason.Error()
	}
	if err := c.pc.Send(head); err != nil {
		return err
	}

	// The stream is owed in every case, including the failure: a caller waiting
	// for content cannot be told to stop waiting by silence. A zero-byte file is
	// an empty stream, and it is told apart from a failed read by the head frame,
	// which is where every failure is reported. Marking it inside the stream
	// would not work: a zero-length frame is the stream's own end marker, so a
	// marker frame would be read as the end and the frames after it would be
	// parsed as the next answer.
	switch {
	case !ok:
		if err := c.pc.SendData(env.ID, nil); err != nil {
			return err
		}
	case len(data) == 0:
		if err := c.pc.SendData(env.ID, nil); err != nil {
			return err
		}
	default:
		if err := c.pc.SendBody(env.ID, data); err != nil {
			return err
		}
	}

	// The tail frame closes the answer. It repeats the outcome so the server's
	// loop can report it once the content has been forwarded.
	return c.pc.Send(head)
}

// satisfyWrite answers a write request with the same three parts a read uses: a
// control frame saying whether it worked, the content stream (empty, but always
// present and always ended), and a control frame closing the answer.
//
// The stream is owed even by the operations that carry no content. The server
// reads it before it resumes reading control frames, so a mkdir that skipped its
// stream would leave the server's frame count wrong on this connection for ever
// after — the next control frame would be read as that stream's data.
//
// Content arrives after the announcement, on the same connection, one frame at a
// time. That is why the file is written from inside this loop: the frames are
// consumed as they arrive, so a file far larger than one frame never has to be
// held whole, and the connection stays at a frame boundary throughout.
func (c *Client) satisfyWrite(env proto.Envelope) error {
	started := time.Now()
	var reason error
	if c.shuttingDown() {
		// Every write request is followed by a content stream, empty or not, and
		// the server waits for it to be consumed before it reads any other frame
		// on this connection. Refusing without draining would leave the stream
		// to be parsed as the next control frame.
		_ = c.drainContent(env.ID)
		reason = errShuttingDown
	} else {
		switch env.Kind {
		case proto.KindMkdir:
			reason = c.serveMkdir(env)
		case proto.KindRemove:
			reason = c.serveRemove(env)
		case proto.KindRename:
			reason = c.serveRename(env)
		case proto.KindWrite:
			reason = c.servePut(env)
		default:
			reason = fmt.Errorf("write request has unknown kind %q", env.Kind)
		}
	}
	logging.Infof("client: %s took=%s err=%v", env.Kind, time.Since(started), reason)

	head := proto.Envelope{ID: env.ID, Kind: proto.KindWriteOK}
	if reason != nil {
		head.Err = reason.Error()
	}
	if err := c.pc.Send(head); err != nil {
		return err
	}
	if err := c.pc.SendData(env.ID, nil); err != nil {
		return err
	}
	return c.pc.Send(head)
}

// drainContent reads the request's content stream to its end and discards it.
//
// It is used when the operation has already failed before the content was
// wanted. The frames are still on the wire, so they have to be consumed; leaving
// them would make every later frame on this connection be read as one position
// earlier than it really is.
func (c *Client) drainContent(id uint64) error {
	for {
		_, done, err := c.pc.RecvData(id)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// servePut handles a whole-file write-back, either replacing the target or
// refusing to touch a name that is already taken.
func (c *Client) servePut(env proto.Envelope) error {
	var req proto.WriteBody
	if err := json.Unmarshal(env.Body, &req); err != nil {
		// The content still has to leave the connection, whatever the
		// announcement said. Decoding failed, but the stream did not.
		_ = c.drainContent(env.ID)
		return fmt.Errorf("bad write request: %v", err)
	}
	if req.Client != c.name {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("write request is addressed to %q, not %q", req.Client, c.name)
	}
	share, rel := shareOf(req.Path), restOf(req.Path)
	logging.Infof("client: write %s", req.Path)

	// The sink pulls one frame at a time off the connection and hands the bytes
	// straight to the file, so no copy of the file is ever held.
	//
	// A failure to read the stream is recorded rather than returned from the
	// sink, so that the write itself is still attempted and can be undone. The
	// difference matters: a sink that returned the error would abandon a
	// partially written temporary file, and the caller that asked for the write
	// would never learn why.
	var streamErr error
	sink := func(w io.Writer) error {
		for {
			// A control frame that is not a pushed update is the sender telling
			// this end that the content was cut short. It has to be recorded,
			// because the end marker that follows is identical to a complete
			// stream's: without this, a prefix of a file would be published as
			// the whole file.
			chunk, done, err := c.pc.RecvDataEvent(env.ID, func(e proto.Envelope) {
				if e.Err != "" && streamErr == nil {
					streamErr = fmt.Errorf("%w: %s", errContentTruncated, e.Err)
				}
			})
			if err != nil {
				streamErr = err
				return nil
			}
			if done {
				return nil
			}
			// Re-armed per accepted chunk, not once for the whole transfer. The
			// loop's deadline was set before this request arrived, with
			// idleTimeout as its bound, so without this a transfer that takes
			// longer than that — a large file over a slow link, which is healthy
			// the whole time — would fail part-way through. What the deadline is
			// for is a peer that has stopped talking, so what it should measure is
			// the gap between two chunks, not the length of the transfer. The
			// server bounds a forwarded chunk the same way, for the same reason.
			if err := c.conn.SetReadDeadline(time.Now().Add(ackTimeout)); err != nil {
				streamErr = err
				return nil
			}
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		}
	}
	// "The stream ended" and "the stream is all there" are different questions,
	// and this is the only place that can answer the second. It is asked before
	// the file is published, so a stream that stopped part-way leaves nothing at
	// the target name: a prefix of a file looks exactly like a whole one to
	// every later reader.
	ended := func() error {
		if streamErr != nil {
			return fmt.Errorf("the content ended early: %w", streamErr)
		}
		return nil
	}
	// The content is owed on every path out of this function, not only the paths
	// that want it. `replaceAtomically` and `CreateFrom` resolve the target path —
	// and, for a create, take the name — *before* the sink runs, so both can refuse
	// without having read a byte of the stream. Returning there would leave the data
	// frames queued in front of this handler's answer, and the next thing the read
	// loop sees is a data frame where it expects a control frame: the loop exits, and
	// the server withdraws every node this client published. A mistyped share name
	// would therefore cost a whole client its presence in the tree.
	//
	// One drain, here, for every failure that did not consume the stream, rather
	// than one on each early return where it is easy to forget. Consumption is
	// recorded by whether the operation is known to have read the stream at all.
	var outErr error
	switch req.Mode {
	case proto.WriteModeCreate:
		// A create takes the name before the content arrives, so a stream that
		// stops part-way leaves a file behind that this end has to remove. The
		// name was free when it was taken, which is the part that cannot be
		// undone.
		if err := c.fs.CreateFrom(share, rel, sink); err != nil {
			outErr = err
		} else if streamErr != nil {
			c.fs.removeIfPresent(share, rel)
			outErr = fmt.Errorf("content of %q ended early: %w", rel, streamErr)
		} else {
			return nil
		}
	case proto.WriteModePut, "":
		if err := c.fs.replaceAtomically(share, rel, sink, ended); err != nil {
			outErr = err
		} else {
			return nil
		}
	default:
		// An unknown mode is refused before the content is written: guessing
		// which of the two writes was meant could destroy a file.
		return fmt.Errorf("write request has unknown mode %q", req.Mode)
	}

	// Only reached on a failure that may not have read the stream. Draining is
	// safe when it did: the stream is already at its end marker, so this reads
	// the marker and returns.
	//
	// A failure to drain is returned in place of the original error, because it
	// means the connection is out of step and that is the more serious of the
	// two — the caller is about to lose this client entirely.
	if err := c.drainContent(env.ID); err != nil {
		return fmt.Errorf("%w (and the content could not be drained: %v)", outErr, err)
	}
	return outErr
}

// serveMkdir handles a directory creation.
func (c *Client) serveMkdir(env proto.Envelope) error {
	var req proto.MkdirBody
	if err := json.Unmarshal(env.Body, &req); err != nil {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("bad mkdir request: %v", err)
	}
	if req.Client != c.name {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("mkdir request is addressed to %q, not %q", req.Client, c.name)
	}
	logging.Infof("client: mkdir %s", req.Path)
	reason := c.fs.Mkdir(shareOf(req.Path), restOf(req.Path))
	if err := c.drainContent(env.ID); err != nil {
		// Losing the connection while answering is the more serious failure:
		// the operation's own result cannot be reported on a dead connection.
		return err
	}
	return reason
}

// serveRemove handles a delete of one file or one empty directory.
func (c *Client) serveRemove(env proto.Envelope) error {
	var req proto.RemoveBody
	if err := json.Unmarshal(env.Body, &req); err != nil {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("bad remove request: %v", err)
	}
	if req.Client != c.name {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("remove request is addressed to %q, not %q", req.Client, c.name)
	}
	logging.Infof("client: remove %s", req.Path)
	reason := c.fs.Remove(shareOf(req.Path), restOf(req.Path))
	if err := c.drainContent(env.ID); err != nil {
		return err
	}
	return reason
}

// serveRename handles a rename within this client's own tree.
//
// A rename that named two different clients never reaches here: the viewer
// refuses it, because by the time a path arrives here the client name has been
// stripped and the owner can no longer tell two clients' identical paths apart.
// Copying the file between machines was the alternative, and that would make a
// rename into a transfer with a window in the middle where both names exist.
func (c *Client) serveRename(env proto.Envelope) error {
	var req proto.RenameBody
	if err := json.Unmarshal(env.Body, &req); err != nil {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("bad rename request: %v", err)
	}
	if req.Client != c.name {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("rename request is addressed to %q, not %q", req.Client, c.name)
	}
	// Both ends are in the same client, so they must be in the same share:
	// a share name is a directory, and no client has one directory that is
	// also another. Sending the rename to one share with a path from the other
	// would move the file to a name nobody asked for.
	oldShare, newShare := shareOf(req.OldPath), shareOf(req.NewPath)
	if oldShare != newShare {
		_ = c.drainContent(env.ID)
		return fmt.Errorf("rename spans shares %q and %q: a rename stays inside one share", oldShare, newShare)
	}
	logging.Infof("client: rename %s -> %s", req.OldPath, req.NewPath)
	reason := c.fs.Rename(oldShare, restOf(req.OldPath), restOf(req.NewPath))
	if err := c.drainContent(env.ID); err != nil {
		return err
	}
	return reason
}

// isWriteRequest reports whether kind is one of the write requests the server
// forwards to the client that owns the path.
//
// It is a list of the request kinds, and it must stay separate from the kinds
// that answer them: a request and an answer are the same shape in both
// directions, so a classification that matched both would have the client answer
// its own answer.
func isWriteRequest(kind string) bool {
	switch kind {
	case proto.KindWrite, proto.KindMkdir, proto.KindRemove, proto.KindRename:
		return true
	}
	return false
}

// shareOf returns the share name in a client-relative path, which is its first
// component.
//
// The server routes a read by client name and hands the owner the path as the
// owner's own clients see it, so "Documents/sub/a.txt" is share "Documents" and
// the rest is the path inside that share. A path with no separator is all share.
func shareOf(path string) string {
	if i := strings.IndexAny(path, `/\`); i >= 0 {
		return path[:i]
	}
	return path
}

// restOf returns the path inside a share: everything after the share's name.
func restOf(path string) string {
	if i := strings.IndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return ""
}

// ackCount reads the batch acknowledgement counter.
func (c *Client) ackCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acks
}

// countAck records one batch acknowledgement.
func (c *Client) countAck() {
	c.mu.Lock()
	c.acks++
	c.mu.Unlock()
}

// anyWaiting reports whether the loop has anything outstanding.
func (c *Client) anyWaiting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiting) > 0
}

// register records a waiter under an ID.
func (c *Client) register(id uint64, ch chan proto.Envelope) {
	c.mu.Lock()
	c.waiting[id] = ch
	c.mu.Unlock()
}

// unregister drops a waiter.
func (c *Client) unregister(id uint64) {
	c.mu.Lock()
	delete(c.waiting, id)
	c.mu.Unlock()
}

// await waits for a reply, failing on the loop's error or a closed client.
func (c *Client) await(id uint64, ch chan proto.Envelope, what string) (proto.Envelope, error) {
	select {
	case env, ok := <-ch:
		if !ok {
			c.mu.Lock()
			err := c.loopErr
			c.mu.Unlock()
			if err == nil {
				err = fmt.Errorf("the connection closed")
			}
			return proto.Envelope{}, fmt.Errorf("client: %s: %w", what, err)
		}
		return env, nil
	case <-c.done:
		return proto.Envelope{}, fmt.Errorf("client: %s: the client is closed: %w", what, net.ErrClosed)
	}
}

// sendAndWait writes a message that expects a reply and waits for it.
//
// eof marks the message as the end of a sequence.
//
// The waiter is registered before the write, not after. A reply can arrive while
// the sending goroutine is between the two, and a reply with no waiter is
// dropped by the read loop, so registering afterwards would turn a fast server
// into a request that waits out its timeout.
func (c *Client) sendAndWait(kind string, body []byte, eof bool, what string) (proto.Envelope, error) {
	c.mu.Lock()
	if c.loopErr != nil {
		err := c.loopErr
		c.mu.Unlock()
		return proto.Envelope{}, fmt.Errorf("client: %s: connection failed: %w", what, err)
	}
	c.nextID++
	id := c.nextID
	ch := make(chan proto.Envelope, 1)
	c.waiting[id] = ch
	c.mu.Unlock()

	env := proto.Envelope{ID: id, Kind: kind, EOF: eof}
	if body != nil {
		env.Body = body
	}
	if err := c.pc.Send(env); err != nil {
		c.unregister(id)
		return proto.Envelope{}, fmt.Errorf("client: send %s: %w", kind, err)
	}
	return c.await(id, ch, what)
}

// Close drops the connection. The server notices the disconnection and removes
// this client's nodes; there is no goodbye message to send.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	// done is closed before the socket, so anything blocked on a reply is
	// released by the close rather than by the read loop's subsequent failure.
	close(c.done)
	return c.conn.Close()
}

// Shutdown stops accepting new forwarded requests, waits up to grace for the
// ones already being served, says goodbye, and closes the connection.
//
// A plain Close is correct for a process that is going away, but it can cut an
// upload in half and it leaves the server to discover the departure from a
// socket close. Shutdown is the version a signal handler uses: the in-flight
// request finishes, the server acknowledges the withdrawal, and only then does
// the connection go.
//
// The caller is expected to stop its filesystem watcher first. This method
// knows nothing about the watcher, and a change reported after draining starts
// would be sent on a connection that is about to close.
//
// A negative or zero grace means DefaultShutdownGrace. Shutdown is safe to call
// more than once.
func (c *Client) Shutdown(grace time.Duration) error {
	if grace <= 0 {
		grace = DefaultShutdownGrace
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if c.draining {
		// Another caller is already running the shutdown; closing again is the
		// one thing this one can do without racing the wait below.
		c.mu.Unlock()
		return c.Close()
	}
	c.draining = true
	idle := make(chan struct{})
	if c.serving == 0 {
		close(idle)
	} else {
		c.idle = idle
	}
	c.mu.Unlock()

	select {
	case <-idle:
	case <-time.After(grace):
		logging.Infof("client: shutdown grace expired with %d request(s) in flight", c.servingCount())
	}

	c.sendBye(byeTimeout)
	return c.Close()
}

// beginServe records that the read loop is starting one forwarded request.
func (c *Client) beginServe() {
	c.mu.Lock()
	c.serving++
	c.mu.Unlock()
}

// endServe records that the request is finished, and releases a shutdown that
// was waiting for exactly that.
func (c *Client) endServe() {
	c.mu.Lock()
	c.serving--
	if c.draining && c.serving == 0 && c.idle != nil {
		close(c.idle)
		c.idle = nil
	}
	c.mu.Unlock()
}

// shuttingDown reports whether new forwarded requests should be refused.
func (c *Client) shuttingDown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.draining
}

// servingCount is used only for the log line about an expired grace.
func (c *Client) servingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serving
}

// sendBye tells the server this client is leaving and waits, briefly, for the
// acknowledgement.
//
// The wait is bounded and its failure is not returned: the close that follows
// withdraws the tree either way, and a server that is not answering is not a
// reason to hang the shutdown.
func (c *Client) sendBye(timeout time.Duration) {
	c.mu.Lock()
	if c.closed || c.loopErr != nil {
		c.mu.Unlock()
		return
	}
	c.nextID++
	id := c.nextID
	ch := make(chan proto.Envelope, 1)
	c.waiting[id] = ch
	c.mu.Unlock()

	if err := c.pc.Send(proto.Envelope{ID: id, Kind: proto.KindBye}); err != nil {
		c.unregister(id)
		return
	}
	select {
	case <-ch:
	case <-time.After(timeout):
	case <-c.done:
	}
	c.unregister(id)
}

// send writes one control message and returns the ID it was given.
//
// Every send carries an ID: a reply echoes the ID of the request it answers, so
// a client that sent two messages both numbered zero could not tell the replies
// apart.
func (c *Client) send(kind string, body []byte, eof bool) (uint64, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, fmt.Errorf("client: send %s on a closed connection: %w", kind, net.ErrClosed)
	}
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	env := proto.Envelope{ID: id, Kind: kind, EOF: eof}
	if body != nil {
		env.Body = body
	}
	if err := c.pc.Send(env); err != nil {
		return id, fmt.Errorf("client: send %s: %w", kind, err)
	}
	return id, nil
}

// batchSize is the configured batch size, or the default.
func (c *Client) batchSize() int {
	if c.batch < 1 {
		return DefaultBatch
	}
	return c.batch
}

// waitForAck waits until the batch acknowledgement counter has moved past
// before, failing on a server error or a dead connection.
//
// The counter rather than a message ID, because a batch acknowledgement carries
// no ID: it is a flow-control token, not a reply to a particular batch.
func (c *Client) waitForAck(before int) error {
	deadline := time.Now().Add(ackTimeout)
	for {
		if err := c.loopFailure(); err != nil {
			return fmt.Errorf("client: waiting for a batch acknowledgement: %w", err)
		}
		if c.ackCount() > before {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("client: waiting for a batch acknowledgement: %w", os.ErrDeadlineExceeded)
		}
		select {
		case <-c.done:
			return fmt.Errorf("client: waiting for a batch acknowledgement: the client is closed: %w", net.ErrClosed)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// loopFailure reports the read loop's failure, if it has one.
func (c *Client) loopFailure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loopErr
}

// isTimeout reports whether err is a read deadline expiring, as opposed to the
// connection failing.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// recvWithin reads one envelope, bounding the wait so a server that goes quiet
// fails the call instead of hanging it.
//
// It is for the handshake, where the read loop does not exist yet. Everything
// after the handshake is read by the loop.
func (c *Client) recvWithin(d time.Duration) (proto.Envelope, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return proto.Envelope{}, err
	}
	defer c.conn.SetReadDeadline(time.Time{})
	return c.pc.Recv()
}

// debugf writes a diagnostic line, when PEERFOLD_DEBUG is set.
//
// A client is a long-running process whose failures would otherwise be silent:
// the loop exiting with no explanation is exactly the situation that needs one.
//
// It goes to stderr rather than stdout because the client's stdout is not a log
// stream — there is nothing on it a user parses — and a diagnostic that landed
// there would interleave with whatever a future subcommand prints. The variable
// is consulted per call rather than cached so that long-lived processes and
// tests can turn it on without a restart.
func debugf(format string, args ...any) {
	if os.Getenv("PEERFOLD_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "peerfold: "+format+"\n", args...)
}
