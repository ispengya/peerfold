// Package viewer is the read-only client role: it dials a server, observes the
// aggregated tree, and never offers shares or writes anything.
package viewer

import (
	"encoding/json"
	"fmt"
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
	if head.Kind != proto.KindRead {
		return nil, fmt.Errorf("viewer: read reply kind = %q, want %q", head.Kind, proto.KindRead)
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
