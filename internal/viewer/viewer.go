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
	"time"

	"peerfold/internal/node"
	"peerfold/internal/proto"
)

// dialTimeout bounds the TCP dial itself.
const dialTimeout = 10 * time.Second

// callTimeout bounds one request/response round trip. A viewer that never
// answers would otherwise hang the CLI for ever on a wedged server.
const callTimeout = 30 * time.Second

// Viewer is a read-only connection to a server.
//
// One request is in flight at a time. The protocol has a single connection with
// strictly alternating control and data frames, so a viewer that pipelined two
// listings could read one reply as the other's; keep the calls sequential.
type Viewer struct {
	conn net.Conn
	pc   *proto.Conn
	next uint64
}

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
		if _, err := v.pc.RecvBody(id); err != nil {
			return nil, fmt.Errorf("viewer: read reply for %q: %s (and its stream: %v)", path, head.Err, err)
		}
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
	var body []byte
	for {
		if err := v.conn.SetReadDeadline(time.Now().Add(callTimeout)); err != nil {
			return nil, err
		}
		chunk, done, err := v.pc.RecvData(id)
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

// put sends a whole-file write-back in the given mode.
func (v *Viewer) put(path string, r io.Reader, mode string) error {
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
	if err := v.sendContent(id, r); err != nil {
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
	body, err := json.Marshal(proto.MkdirBody{Client: client, Path: rest})
	if err != nil {
		return fmt.Errorf("viewer: encode mkdir: %w", err)
	}
	return v.writeRequest(proto.KindMkdir, body, "mkdir "+path)
}

// Remove deletes one file or one empty directory on the owning client.
func (v *Viewer) Remove(path string) error {
	client, rest, err := splitWritePath("remove", path)
	if err != nil {
		return err
	}
	body, err := json.Marshal(proto.RemoveBody{Client: client, Path: rest})
	if err != nil {
		return fmt.Errorf("viewer: encode remove: %w", err)
	}
	return v.writeRequest(proto.KindRemove, body, "remove "+path)
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
		return fmt.Errorf("viewer: rename %q to %q: the two ends are on different clients (%q and %q): a rename stays inside one client",
			oldPath, newPath, oldClient, newClient)
	}
	body, err := json.Marshal(proto.RenameBody{Client: oldClient, OldPath: oldRest, NewPath: newRest})
	if err != nil {
		return fmt.Errorf("viewer: encode rename: %w", err)
	}
	return v.writeRequest(proto.KindRename, body, "rename "+oldPath+" to "+newPath)
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
	if head.Err != "" {
		// The outcome is decided and the stream that follows is empty by
		// construction — a write's answer never carries content — so nothing is
		// left unread by returning here: the close that comes next is the
		// server's to send and no part of it can change this answer.
		return fmt.Errorf("viewer: %s: %s", what, head.Err)
	}
	// The stream is read whatever the outcome says: a failed write still sent
	// one, and its end marker is what puts this connection back at a frame
	// boundary.
	if _, err := v.readContent(id); err != nil {
		return fmt.Errorf("viewer: %s: reading the answer's stream: %w", what, err)
	}
	tail, err := v.recv(callTimeout)
	if err != nil {
		return fmt.Errorf("viewer: %s ended without its closing frame: %w", what, err)
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
	body, err := v.pc.RecvBody(id)
	if err != nil {
		return nil, fmt.Errorf("viewer: %s: %w", what, err)
	}
	return body, nil
}

// Close drops the connection. A viewer owns nothing on the server, so there is
// nothing to clean up beyond the socket.
func (v *Viewer) Close() error { return v.conn.Close() }

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

// recv reads one envelope, bounding the wait so a quiet server fails the call
// rather than hanging it for ever.
func (v *Viewer) recv(d time.Duration) (proto.Envelope, error) {
	if err := v.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return proto.Envelope{}, err
	}
	defer v.conn.SetReadDeadline(time.Time{})
	return v.pc.Recv()
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
