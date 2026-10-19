// Package viewer is the read-only client role: it dials a server, observes the
// aggregated tree, and never offers shares or writes anything.
package viewer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

// dialTimeout bounds the TCP dial itself.
const dialTimeout = 10 * time.Second

// callTimeout bounds one request/response round trip. A viewer that never
// answers would otherwise hang the CLI for ever on a wedged server.
const callTimeout = 30 * time.Second

// ErrCrossClient reports that a rename named two different clients, which cannot
// be carried out: two clients share no filesystem, so there is nowhere for the
// file to move to.
//
// It is a sentinel rather than a message because this refusal is made locally,
// before any frame is sent — unlike the reasons that travel back from an owner,
// which arrive as strings in an answer's error field and cannot be compared. A
// caller can therefore test it with errors.Is, and the next plan's mount layer
// can map it to the cross-device error a filesystem is expected to return for a
// rename it cannot perform.
var ErrCrossClient = errors.New("viewer: rename across clients")

// Viewer is a read-only connection to a server.
//
// One request is in flight at a time. The protocol has a single connection with
// strictly alternating control and data frames, so a viewer that pipelined two
// listings could read one reply as the other's; keep the calls sequential.
// Event is one pushed index update: the batch of nodes, and the sequence number
// the transport gave it.
//
// Seq is what makes a dropped update visible. The channel behind Watch is
// deliberately non-blocking, so a watcher that stops reading loses updates rather
// than stalling the reader -- and a lost update leaves no other trace anywhere in
// the system, because a drop is not an error. Numbering the batches means a
// consumer can see the hole.
//
// Zero means the update arrived unnumbered, which is what a sender that does not
// number its events produces. It is not a hole.
type Event struct {
	Seq   uint64
	Nodes []node.Node
}

type Viewer struct {
	conn net.Conn
	pc   *proto.Conn
	next uint64

	// mu guards the fields below. It is taken around the fields and never across a
	// read, because a reader that held it while waiting would stop the very
	// hand-off it is waiting for.
	mu     sync.Mutex
	events chan Event
	// seq counts the pushes this connection has been handed, so a gap is
	// observable downstream. It is bumped for every update received, including
	// ones the channel then refuses, which is what makes the refusal visible
	// rather than silent.
	seq uint64
	// closed says the event channel has been closed. emit and Close both decide
	// what to do with it under mu, so a batch can never be offered to a channel
	// that has already been closed — which would be a panic rather than a lost
	// update. Without the flag the decision was split: emit read the channel
	// under mu and then sent to it after releasing the lock, and a Close in that
	// window closed the channel underneath it.
	closed bool
	// startReader makes the switch to a background reader happen once, and only
	// on the first Watch. A viewer that never watches keeps the plain
	// request/response path with its per-call deadline.
	startReader sync.Once
	reader      bool
	// connToken is held by whoever may read the connection: the reader while it
	// listens for updates, and a call from before its request is sent until its
	// last frame is read. That single hand-off is what keeps a reply from being
	// read by the reader, and a raced reply is what "replies are out of step"
	// reports.
	connToken chan struct{}
}

// readWindow bounds one read by the background reader.
//
// It is short, and that is the point. The reader holds the connection for as long
// as it is in a read, and a call cannot take the connection until that read
// returns: a reader parked on a quiet tree would therefore hold every call up
// until the tree changed, which is the opposite of what a viewer is for. So it
// reads for at most this long, hands the connection back, and looks again.
//
// A longer window (a minute, say) is still correct in the sense that no test
// fails to *deliver* an update; what fails is every call made while the tree is
// quiet, which is most of them. `idleRead` was that mistake.
//
// Known limitation, deliberately not papered over: the window bounds the whole
// frame read, not just the wait for its first byte. A pushed update large enough
// not to finish arriving within it — a control frame may be up to MaxPayload,
// 1 MiB — can leave the read stopped part-way through a frame, and the frame is
// then resumed by whoever reads the connection next rather than by this reader.
// The bound is not needed once bytes have started arriving; making it stop at
// the first byte would close this, and that is a proto-level change (ReadFrame
// would have to report how far it got, which it does not). It is left open here
// rather than half-fixed, and it is recorded as a limitation.
const readWindow = 20 * time.Millisecond

// Dial connects to serverAddr and completes the viewer handshake: the server
// replies to the hello with its name and aggregate ruling, and a viewer is not
// registered, so it can observe but cannot publish.
func Dial(serverAddr string) (*Viewer, error) {
	conn, err := net.DialTimeout("tcp", serverAddr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("viewer: dial %s: %w", serverAddr, err)
	}
	v := &Viewer{conn: conn, pc: proto.NewConn(conn)}
	if err := v.handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return v, nil
}

// handshake sends the hello and requires a hello_ok back.
func (v *Viewer) handshake() error {
	body, err := json.Marshal(proto.HelloBody{Role: proto.RoleViewer})
	if err != nil {
		return fmt.Errorf("viewer: encode hello: %w", err)
	}
	id, err := v.send(proto.KindHello, body)
	if err != nil {
		return err
	}
	env, err := v.recv(dialTimeout)
	if err != nil {
		return fmt.Errorf("viewer: read hello reply: %w", err)
	}
	if env.Err != "" {
		return fmt.Errorf("viewer: handshake rejected: %s", env.Err)
	}
	// An error reply echoes the request's Kind, so this also catches a server
	// that answered a different request than the one that was sent.
	if env.Kind != proto.KindHelloOK {
		return fmt.Errorf("viewer: hello reply kind = %q, want %q", env.Kind, proto.KindHelloOK)
	}
	if env.ID != id {
		return fmt.Errorf("viewer: hello reply ID = %d, want %d", env.ID, id)
	}
	return nil
}

// List returns the direct children of path, ascending.
//
// path is "" for the top level (the connected clients), "win" for one client's
// shares, or "win/Documents" for a directory inside a client's tree. The path
// is split at the FIRST separator only: everything after it belongs to the
// client's own namespace, where a separator is an ordinary path character
// rather than something the viewer should interpret.
func (v *Viewer) List(path string) ([]node.Node, error) {
	var out []node.Node
	// The connection is taken before the request is sent, not after. Taking it
	// afterwards leaves a race the reader wins whenever an update is already on
	// its way: the reader holds the connection, the request goes out, the reader
	// reads the reply to a question nobody asked it to read, and the call waits
	// for an answer that has already been consumed.
	err := v.stream(func() error {
		var err error
		out, err = v.list(path)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// list asks for one directory of the aggregated tree.
func (v *Viewer) list(path string) ([]node.Node, error) {
	client, rest := splitPath(path)

	body, err := json.Marshal(proto.ListBody{Client: client, Path: rest})
	if err != nil {
		return nil, fmt.Errorf("viewer: encode list: %w", err)
	}
	id, err := v.send(proto.KindList, body)
	if err != nil {
		return nil, err
	}

	// The request is not retried and no other request is sent until this reply
	// arrives: the connection carries one in-flight response at a time.
	env, err := v.recv(callTimeout)
	if err != nil {
		return nil, fmt.Errorf("viewer: read list reply for %q: %w", path, err)
	}
	if env.Err != "" {
		return nil, fmt.Errorf("viewer: %s", env.Err)
	}
	if env.ID != id {
		return nil, fmt.Errorf("viewer: list reply ID = %d, want %d: replies are out of step", env.ID, id)
	}
	if env.Kind != proto.KindListOK {
		return nil, fmt.Errorf("viewer: list reply kind = %q, want %q", env.Kind, proto.KindListOK)
	}

	// The reply body arrives as a data stream, not in the envelope: a listing
	// can be far past one frame's payload, so the server sends it in chunks.
	raw, err := v.recvBody(env, id, "list reply for "+path)
	if err != nil {
		return nil, err
	}
	var b proto.ListOKBody
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("viewer: decode list reply: %w", err)
	}
	out := make([]node.Node, 0, len(b.Nodes))
	for _, n := range b.Nodes {
		out = append(out, node.Node{
			Path:  n.Path,
			Kind:  node.Kind(n.Kind),
			Size:  n.Size,
			Mode:  n.Mode,
			Mtime: n.Mtime,
		})
	}
	return out, nil
}

// Read returns up to n bytes of a file, starting at off. A negative n means
// "to the end of the file", which is how Cat asks for everything.
//
// The bytes come from the owner's disk, not from the server's index: the index
// holds names and sizes, so a file that has been deleted since the upload is
// reported by the owner rather than served from a stale copy.
func (v *Viewer) Read(path string, off int64, n int) ([]byte, error) {
	var out []byte
	// Taken before the request goes out: see List.
	err := v.stream(func() error {
		var err error
		out, err = v.read(path, off, n)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// read is Read with the connection already in hand.
func (v *Viewer) read(path string, off int64, n int) ([]byte, error) {
	if off < 0 {
		return nil, fmt.Errorf("viewer: read %q: offset %d is negative", path, off)
	}
	client, rest := splitPath(path)
	if client == "" || rest == "" {
		return nil, fmt.Errorf("viewer: read %q: a read needs a client and a path inside it", path)
	}

	body, err := json.Marshal(proto.ReadBody{Client: client, Path: rest, Off: off, N: n})
	if err != nil {
		return nil, fmt.Errorf("viewer: encode read: %w", err)
	}
	id, err := v.send(proto.KindRead, body)
	if err != nil {
		return nil, err
	}

	head, err := v.recv(callTimeout)
	if err != nil {
		return nil, fmt.Errorf("viewer: read reply for %q: %w", path, err)
	}
	if head.Err != "" {
		// The head frame is where "no such file" arrives. The content stream and
		// the closing frame are still owed — frames are one sequence, so a part
		// left unread would be parsed as the next answer — so both are read
		// before the failure is returned.
		// Bound the stream's read as well: the connection is held for the whole
		// round trip, so a server that stops mid-stream would otherwise hold it for
		// ever.
		if err := v.conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
			return nil, err
		}
		if _, err := v.pc.RecvBody(id); err != nil {
			v.conn.SetReadDeadline(time.Time{})
			return nil, fmt.Errorf("viewer: read reply for %q: %s (and its stream: %v)", path, head.Err, err)
		}
		v.conn.SetReadDeadline(time.Time{})
		if _, err := v.recv(callTimeout); err != nil {
			return nil, fmt.Errorf("viewer: read reply for %q: %s (and its closing frame: %v)", path, head.Err, err)
		}
		return nil, fmt.Errorf("viewer: %s", head.Err)
	}
	if head.ID != id {
		return nil, fmt.Errorf("viewer: read reply ID = %d, want %d: replies are out of step", head.ID, id)
	}
	if head.Kind != proto.KindReadOK {
		return nil, fmt.Errorf("viewer: read reply kind = %q, want %q", head.Kind, proto.KindReadOK)
	}

	// The content is a data stream, so it can exceed one frame. A zero-byte
	// file arrives as one frame of zero bytes — which is not the same as no
	// frame at all, and that is what tells it apart from a failed read.
	data, err := v.readContent(id)
	if err != nil {
		return nil, fmt.Errorf("viewer: read content for %q: %w", path, err)
	}
	// The tail frame closes the answer, and its outcome is the one that counts:
	// it is what proves the whole stream was delivered.
	tail, err := v.recv(callTimeout)
	if err != nil {
		return nil, fmt.Errorf("viewer: read reply for %q ended without its closing frame: %w", path, err)
	}
	if tail.Err != "" {
		return nil, fmt.Errorf("viewer: %s", tail.Err)
	}
	return data, nil
}

// readContent reads a data stream.
//
// A stream that ends without carrying any bytes is a zero-byte file, which is a
// successful answer. Every failure travels in the head frame instead, so there is
// nothing here to tell apart.
func (v *Viewer) readContent(id uint64) ([]byte, error) {
	defer v.conn.SetReadDeadline(time.Time{})
	var body []byte
	for {
		if err := v.conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
			return nil, err
		}
		chunk, done, err := v.pc.RecvDataEvent(id, v.publishEvent)
		if err != nil {
			return nil, err
		}
		if done {
			return body, nil
		}
		body = append(body, chunk...)
	}
}

// Cat returns a whole file.
func (v *Viewer) Cat(path string) ([]byte, error) {
	return v.Read(path, 0, -1)
}

// Put replaces the file at path with everything r produces.
//
// The content is sent while the request is still open, one chunk at a time, so a
// file far larger than memory is still one call. Nothing is buffered here: the
// reader is drained straight into frames.
func (v *Viewer) Put(path string, r io.Reader) error {
	return v.put(path, r, proto.WriteModePut)
}

// PutExclusive writes path only if nothing is already there, and refuses if
// something is.
//
// It is separate from Put because the two differ in what they will destroy. A
// caller that means "save this here" wants Put; one that means "this file must
// not already exist" — writing a lock file, or refusing to clobber somebody
// else's work — wants this, and could not get it by checking first: the check
// and the write would not be one step, so a file created between them would be
// overwritten anyway.
func (v *Viewer) PutExclusive(path string, r io.Reader) error {
	return v.put(path, r, proto.WriteModeCreate)
}

// put takes the connection first and then sends the write, so the reader is
// already out of the way when the answer starts arriving.
func (v *Viewer) put(path string, r io.Reader, mode string) error {
	return v.stream(func() error { return v.putBody(path, r, mode) })
}

// putBody sends a whole-file write-back in the given mode, with the connection
// in hand.
func (v *Viewer) putBody(path string, r io.Reader, mode string) error {
	client, rest, err := splitWritePath("write", path)
	if err != nil {
		return err
	}
	body, err := json.Marshal(proto.WriteBody{Client: client, Path: rest, Mode: mode})
	if err != nil {
		return fmt.Errorf("viewer: encode write: %w", err)
	}
	id, err := v.send(proto.KindWrite, body)
	if err != nil {
		return err
	}

	// The content goes out after the announcement and before the answer is
	// read: the server is waiting for it, and a viewer that waited for the
	// answer first would be waiting for the owner to answer a request whose
	// content had not been sent.
	//
	// A local reader that fails still leaves the server's answer on the way: the
	// server ends the stream and answers on every path, including the one where
	// the content was cut short. Returning here without reading that answer
	// leaves the connection out of step — the next call reads this answer and
	// reports an ID that does not match its own request — and the failure would
	// be reported twice, once now and once by whatever came next. So the answer
	// is drained and the content's own error is what the caller gets, because
	// that is the error that explains why.
	if err := v.sendContent(id, r); err != nil {
		// The answer's error is deliberately dropped: the server reports that
		// the content was cut short, which is this error's cause rather than
		// news, and the local failure is the one worth naming.
		_ = v.awaitWrite(id, "write to "+path)
		return err
	}
	return v.awaitWrite(id, "write to "+path)
}

// Mkdir creates a directory on the owning client.
func (v *Viewer) Mkdir(path string) error {
	client, rest, err := splitWritePath("mkdir", path)
	if err != nil {
		return err
	}
	// The connection is taken before the request is sent: see List. The body is
	// built inside so nothing but this call can be in flight on it.
	return v.stream(func() error {
		body, err := json.Marshal(proto.MkdirBody{Client: client, Path: rest})
		if err != nil {
			return fmt.Errorf("viewer: encode mkdir: %w", err)
		}
		return v.writeRequest(proto.KindMkdir, body, "mkdir "+path)
	})
}

// Remove deletes one file or one empty directory on the owning client.
func (v *Viewer) Remove(path string) error {
	client, rest, err := splitWritePath("remove", path)
	if err != nil {
		return err
	}
	return v.stream(func() error {
		body, err := json.Marshal(proto.RemoveBody{Client: client, Path: rest})
		if err != nil {
			return fmt.Errorf("viewer: encode remove: %w", err)
		}
		return v.writeRequest(proto.KindRemove, body, "remove "+path)
	})
}

// Rename moves a path within one client.
//
// Both ends must name the same client. A rename across clients is refused rather
// than turned into a copy, because two clients share no filesystem and a copy is
// not what was asked for. Both the viewer and the owner apply that rule, and the
// viewer's is the one that can: see the comment inside.
func (v *Viewer) Rename(oldPath, newPath string) error {
	oldClient, oldRest, err := splitWritePath("rename", oldPath)
	if err != nil {
		return err
	}
	newClient, newRest, err := splitWritePath("rename", newPath)
	if err != nil {
		return err
	}
	if newClient != oldClient {
		// Refused here as well as by the owner, and deliberately so. The two
		// ends are stripped of the client name before they are sent, so what the
		// owner receives cannot say which clients they came from — if the
		// refusal lived only there, this request would be indistinguishable from
		// a legitimate rename of two identically spelled paths and would be
		// carried out. This is the only party that still knows.
		//
		// Wrapped rather than formatted: this is the one refusal a caller can
		// still identify, because it is decided here instead of on the wire.
		return fmt.Errorf("%w %q to %q: %q and %q", ErrCrossClient,
			oldPath, newPath, oldClient, newClient)
	}
	return v.stream(func() error {
		body, err := json.Marshal(proto.RenameBody{Client: oldClient, OldPath: oldRest, NewPath: newRest})
		if err != nil {
			return fmt.Errorf("viewer: encode rename: %w", err)
		}
		return v.writeRequest(proto.KindRename, body, "rename "+oldPath+" to "+newPath)
	})
}

// splitWritePath separates the client name from the path inside it and requires
// both, since a write has to name a machine and a place on it.
func splitWritePath(what, path string) (client, rest string, err error) {
	client, rest = splitPath(path)
	if client == "" || rest == "" {
		return "", "", fmt.Errorf("viewer: %s %q: a %s needs a client and a path inside it", what, path, what)
	}
	return client, rest, nil
}

// writeRequest sends a write that carries no content and returns its outcome.
func (v *Viewer) writeRequest(kind string, body []byte, what string) error {
	id, err := v.send(kind, body)
	if err != nil {
		return err
	}
	return v.awaitWrite(id, what)
}

// awaitWrite reads the three parts of a write answer and reports the outcome.
//
// All three are always read, including on failure. The answer is a frame
// sequence, so a part left unread does not stay where it is: it becomes the next
// frame read, and every later answer is then one frame out of step.
//
// The head is read and reported before the stream that follows it, and that
// order is the point: the head is where every outcome travels, and the two
// frames after it are the stream and the close. If the answer is already
// known — the head says what happened — then a failure to read the rest of it,
// which is the server failing to write frames it was cleaning up with, must not
// replace what the head said with a report about the transport. That is how an
// error the server had already sent would be lost in favour of "the connection
// closed", which says less.
func (v *Viewer) awaitWrite(id uint64, what string) error {
	head, err := v.recv(callTimeout)
	if err != nil {
		return fmt.Errorf("viewer: %s: %w", what, err)
	}
	if head.ID != id {
		return fmt.Errorf("viewer: %s: reply ID = %d, want %d: replies are out of step", what, head.ID, id)
	}
	if head.Kind != proto.KindWriteOK {
		return fmt.Errorf("viewer: %s: reply kind = %q, want %q", what, head.Kind, proto.KindWriteOK)
	}
	// All three parts are read whatever the head says. A write's answer is a
	// head, an (empty) stream, and a closing frame even when it failed — the
	// stream is what marks the end of the answer on the wire, and skipping it
	// leaves its end marker to be read as the next reply. Returning on the
	// head's error alone produced exactly that: the next request on this
	// connection read a data frame and failed with "expected a control frame,
	// got frame kind 2".
	if _, err := v.readContent(id); err != nil {
		return fmt.Errorf("viewer: %s: reading the answer's stream: %w", what, err)
	}
	tail, err := v.recv(callTimeout)
	if err != nil {
		return fmt.Errorf("viewer: %s ended without its closing frame: %w", what, err)
	}
	if head.Err != "" {
		// The head carries the outcome, and it is reported after the answer has
		// been consumed in full.
		return fmt.Errorf("viewer: %s: %s", what, head.Err)
	}
	if tail.Err != "" {
		return fmt.Errorf("viewer: %s: %s", what, tail.Err)
	}
	return nil
}

// sendContent writes everything r produces as the content stream of id, in
// chunks, and ends it.
//
// The reader is never collected first: one chunk is read from r and one frame is
// written, so the memory a put needs does not grow with the file.
func (v *Viewer) sendContent(id uint64, r io.Reader) error {
	buf := make([]byte, proto.MaxChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if err := v.pc.SendData(id, buf[:n]); err != nil {
				return fmt.Errorf("viewer: send content: %w", err)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// The stream still has to be ended, so the server is not left
			// waiting for bytes that will never come. The failure is reported
			// afterwards, but the attempt to close the stream is unconditional.
			//
			// The notice comes first, because the end marker cannot carry it:
			// "the stream ended" and "the stream is all there" are the same
			// frame, so without a signal the owner would take the bytes that did
			// arrive for the whole file and rename them over the target. This is
			// the requester-side half of the same notice the server sends when
			// its own forwarding fails.
			v.pc.SendStreamError(id, err)
			endErr := v.pc.SendData(id, nil)
			if endErr != nil {
				return fmt.Errorf("viewer: content failed (%v) and its stream could not be ended (%v)", err, endErr)
			}
			return fmt.Errorf("viewer: send content: %w", err)
		}
	}
	return v.pc.SendData(id, nil)
}

// recvBody reads a reply's body.
//
// A streamed reply carries no body in its envelope, and one that fits carries
// the whole body there. The distinction is "is Body nil", not "is Body empty":
// a listing of nothing still marshals to "{}"-worth of bytes, so an empty,
// non-nil Body is a body, while nil means the bytes are still coming.
func (v *Viewer) recvBody(env proto.Envelope, id uint64, what string) ([]byte, error) {
	if env.Body != nil {
		return env.Body, nil
	}
	if err := v.conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
		return nil, fmt.Errorf("viewer: %s: %w", what, err)
	}
	defer v.conn.SetReadDeadline(time.Time{})
	// RecvBody is not used here because it reads with RecvData, which has nowhere
	// to put a pushed update and therefore drops one. An update can land between
	// two chunks of a reply's body just as easily as before its head, and this is
	// a body read: the same reasoning as the head read above, applied to the
	// bytes.
	//
	// A truncation notice is the other thing this must not drop. Nothing on this
	// connection puts one on a listing's body today, but the moment something
	// does, a body read that discards it turns a cut-short listing into a
	// complete one — the failure this whole design exists to prevent.
	var body []byte
	for {
		chunk, done, err := v.pc.RecvDataEvent(id, v.publishEvent)
		if err != nil {
			return nil, fmt.Errorf("viewer: %s: %w", what, err)
		}
		if done {
			return body, nil
		}
		body = append(body, chunk...)
	}
}

// Close drops the connection. A viewer owns nothing on the server, so there is
// nothing to clean up beyond the socket.
// send writes one request and returns the ID it was given, so the reply can be
// matched to it.
func (v *Viewer) send(kind string, body []byte) (uint64, error) {
	v.next++
	id := v.next
	if err := v.pc.Send(proto.Envelope{ID: id, Kind: kind, Body: body}); err != nil {
		return 0, fmt.Errorf("viewer: send %s: %w", kind, err)
	}
	return id, nil
}

// recv reads the next envelope, bounding the wait so a quiet server fails the
// call rather than hanging it for ever.
//
// It runs inside stream, so the request has already gone out and the reader is
// standing aside: this is the only reader of this reply.
//
// A pushed update that arrives before the reply is passed over here, and
// published rather than dropped: the reader is not reading, so this is the only
// place it can be seen, and a viewer that lost the update because it happened to
// ask a question at the same time would be worse than one that asked nothing.
// The same applies between a reply's head and its body; every body read below
// uses the same wrapper.
func (v *Viewer) recv(d time.Duration) (proto.Envelope, error) {
	return v.recvWithin(d)
}

// recvWithin reads the next envelope with its own deadline, publishing any
// pushed update that arrives while it waits.
func (v *Viewer) recvWithin(d time.Duration) (proto.Envelope, error) {
	if err := v.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return proto.Envelope{}, err
	}
	defer v.conn.SetReadDeadline(time.Time{})
	return v.pc.RecvEvent(v.publishEvent)
}

// emit publishes a batch of updates, and never waits: a watcher that has stopped
// reading loses updates rather than breaking the connection.
//
// The sequence number is bumped for every batch that arrives, including one the
// channel refuses. That is the whole mechanism: the enqueue is what fails, so a
// counter bumped only on success would make a drop indistinguishable from a quiet
// period. Bumping first and dropping under the same lock means the next batch
// that does get through carries a number that reveals the hole.
func (v *Viewer) emit(nodes []node.Node) {
	// The whole decision is made under the lock — whether there is a channel, and
	// whether it is still open — because the send itself is non-blocking and so
	// holding the lock here cannot wait on a caller.
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seq++
	if v.events == nil || v.closed {
		return
	}
	select {
	case v.events <- Event{Seq: v.seq, Nodes: nodes}:
	default:
		// Refused, and the counter has already moved on, so the next accepted
		// batch is numbered one past the gap.
	}
}

// stream runs one request and everything belonging to its answer with the reader
// standing aside, and lets it resume afterwards.
//
// The order is the whole point. The connection is taken before the request goes
// out, not after: a request sent while the reader still holds the connection
// produces a reply the reader reads, and the call then waits for an answer that
// has already been consumed — which is what "replies are out of step" reports.
//
// Before a reader exists there is nothing to stand aside: the call is the only
// reader, and this is a no-op.
func (v *Viewer) stream(fn func() error) error {
	v.mu.Lock()
	if !v.reader {
		v.mu.Unlock()
		return fn()
	}
	token := v.connToken
	v.mu.Unlock()

	select {
	case <-token:
	case <-time.After(callTimeout):
		return fmt.Errorf("viewer: no free connection within %s", callTimeout)
	}
	defer func() { token <- struct{}{} }()
	return fn()
}

// isTimeout reports whether err is a deadline expiring rather than a connection
// failing. The two look alike at a Recv and mean opposite things here.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// startPump switches the viewer to a single background reader.
//
// It is what makes Watch work at all. Pushed updates arrive on their own
// schedule, and without something reading the socket they would sit in its
// buffer until the next request happened to be made — a watcher that hears
// nothing until it asks a question is not watching. Exactly one goroutine reads
// the connection at a time from here on, because two concurrent readers would
// shred the frame boundaries.
func (v *Viewer) startPump() {
	v.startReader.Do(func() {
		v.mu.Lock()
		v.events = make(chan Event, 64)
		v.connToken = make(chan struct{}, 1)
		v.connToken <- struct{}{}
		v.reader = true
		events := v.events
		v.mu.Unlock()
		go v.pump(events)
	})
}

// pump reads pushed updates while no call is being served.
// pump reads pushed updates.
//
// It never reads a reply, and the token alone is what makes that true — there is
// no flag saying "a call is underway", because the call holding the token is
// exactly that statement. A call takes the token before it sends, so while this
// holds the token the call has not asked anything yet and no reply can exist;
// when this finds the token gone it waits, and finds it again only after the
// call has read its own answer to the end.
//
// The read is bounded by readWindow, which is what lets the connection go: a
// call cannot take it until this read returns, so a read parked for as long as
// the tree is quiet would hold every call up until the tree changed.
func (v *Viewer) pump(events chan Event) {
	defer func() {
		// The connection is gone, so every later call has to fail rather than
		// wait for a reply nobody is left to read.
		v.mu.Lock()
		v.reader = false
		v.mu.Unlock()
	}()
	for {
		select {
		case <-v.connToken:
		default:
			// A call is mid-round-trip. Take the token the moment it is back.
			time.Sleep(time.Millisecond)
			continue
		}
		err := v.conn.SetReadDeadline(time.Now().Add(readWindow))
		if err != nil {
			return
		}
		// Only pushed updates are published; anything else here has no call
		// waiting for it, because a call holds the token, and is dropped rather
		// than queued — nothing reads a queue on this connection.
		_, err = v.pc.RecvEvent(v.publishEvent)
		v.connToken <- struct{}{}
		if err != nil && !isTimeout(err) {
			return
		}
	}
}

// publishEvent decodes one pushed update and hands it to whoever is watching.
//
// A push that carries no sequence number is passed through as zero rather than
// being numbered here: the number has to come from the sender for a gap to mean
// anything, and inventing one for an unnumbered sender would turn its quiet
// periods into reported losses.
func (v *Viewer) publishEvent(env proto.Envelope) {
	var b proto.EventBody
	if json.Unmarshal(env.Body, &b) != nil {
		return
	}
	nodes := nodeJSONToNodes(b.Nodes)
	if b.Seq != 0 {
		// The sender numbered it, so the number it chose is carried through: it
		// is the sender's view of its own stream, and renumbering would erase the
		// gaps it is reporting.
		v.emitSeq(b.Seq, nodes)
		return
	}
	v.emit(nodes)
}

// emitSeq publishes a batch carrying a sequence number chosen by the sender,
// rather than one this connection assigned.
func (v *Viewer) emitSeq(seq uint64, nodes []node.Node) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// The local counter still advances past the sender's number, so a batch this
	// connection assigns later cannot collide with one the sender numbered.
	if seq > v.seq {
		v.seq = seq
	}
	if v.events == nil || v.closed {
		return
	}
	select {
	case v.events <- Event{Seq: seq, Nodes: nodes}:
	default:
	}
}

// nodeJSONToNodes converts a pushed update's wire nodes to the tree's model.
func nodeJSONToNodes(in []proto.NodeJSON) []node.Node {
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

// Watch returns a channel of index updates pushed by the server.
//
// Each value is the batch one filesystem change produced, with fully qualified
// paths: "win/Documents/x.txt", not "Documents/x.txt". The channel is shared —
// calling Watch twice returns the same channel rather than splitting the
// updates — and it is closed when the viewer is.
//
// The first call starts a background reader, because a push arrives on the
// server's schedule rather than in answer to anything. From then on a call's own
// timeout bounds the wait for its reply, and the reader hands the connection
// back every readWindow so a call is not held up by a quiet tree.
//
// The channel is buffered and a push never waits for a reader, so a caller that
// stops reading loses updates rather than breaking the connection. That is the
// intended trade: a viewer that needs the tree always has List, and a listing
// taken after an update has already been applied cannot be stale. What the drop
// does leave is a hole in the sequence numbers, which is how a consumer can tell
// that it happened.
func (v *Viewer) Watch() <-chan Event {
	v.startPump()
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.events
}

// Match reports whether a pushed path is target itself or something under it.
//
// It exists so a caller does not have to reinvent the comparison, which is a
// path question rather than a string one: the prefix "win/Documents" must match
// "win/Documents/x.txt" but not "win/DocumentsX/x.txt".
func Match(target, path string) bool {
	if path == target {
		return true
	}
	return strings.HasPrefix(path, target+"/")
}

// Close ends the connection and closes the event channel, so a caller ranging
// over it is released rather than left waiting for ever.
//
// It is safe to call more than once — a second close reports nothing and closes
// nothing — and safe to call while a batch is being published: the channel is
// closed exactly once and no send can follow it.
func (v *Viewer) Close() error {
	v.mu.Lock()
	already := v.closed
	if v.events != nil && !v.closed {
		v.closed = true
		close(v.events)
		v.events = nil
	} else {
		v.closed = true
	}
	v.mu.Unlock()
	if already {
		// A second close has nothing left to close, and the socket's own
		// "already closed" error is not a failure of this call. Reporting it
		// would make Close fail for a caller that closed on one path and
		// deferred a close on another — which is ordinary.
		return nil
	}
	return v.conn.Close()
}

// splitPath turns a user-facing path into the (client, path) pair the protocol
// carries.
//
// A bare name is a client and its path is empty — "" addresses the top level.
// The split happens once, on the first separator: everything after it belongs
// to the client's own namespace, where a separator is an ordinary path
// character rather than something the viewer should interpret.
//
// Backslash counts as a separator too. A share name cannot contain one, so it
// can only ever be a redundant way to spell the boundary, and a Windows user
// typing "win\Documents" means the same thing.
func splitPath(path string) (client, rest string) {
	if i := strings.IndexAny(path, `/\`); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}
