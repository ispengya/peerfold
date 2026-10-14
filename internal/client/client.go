// Package client dials a peerfold server and publishes local shares into its
// aggregated tree.
package client

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"peerfold/internal/node"
	"peerfold/internal/proto"
)

// ackTimeout bounds the wait for the server's upload acknowledgement. An
// upload that the server never confirms must fail rather than hang: the client
// is what a user is waiting on.
const ackTimeout = 60 * time.Second

// Options configures one client connection.
type Options struct {
	// Server is the address to dial, host:port.
	Server string
	// Name identifies this client's subtree. It must be unique among the
	// connected clients.
	Name string
	// Role is proto.RoleClient or proto.RoleViewer.
	Role string
	// Shares are the local directories to publish.
	Shares Shares
	// Aggregate asks to be the client that supplies the aggregated tree. v1
	// allows exactly one; the server reports what it actually granted.
	Aggregate bool
	// Batch is the maximum number of nodes per tree batch. Values below 1 use
	// DefaultBatch.
	Batch int
	// OnBatch, if set, is called after each batch has been sent, with the
	// running total of nodes sent. It exists so a caller can show upload
	// progress.
	OnBatch func(sent int)
}

// Client is a live connection to a server.
type Client struct {
	conn      net.Conn
	pc        *proto.Conn
	name      string
	shares    Shares
	aggregate bool
	onBatch   func(sent int)
	batch     int

	mu     sync.Mutex
	nextID uint64
	closed bool
}

// Dial connects to o.Server, sends the hello, and returns the client with the
// role the server actually granted — which may be a downgrade from what was
// asked for.
func Dial(o Options) (*Client, error) {
	if o.Role == "" {
		o.Role = proto.RoleClient
	}
	if o.Role == proto.RoleClient && o.Name == "" {
		return nil, fmt.Errorf("client: a client connection needs a name")
	}

	raw, err := net.Dial("tcp", o.Server)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", o.Server, err)
	}
	c := &Client{
		conn:    raw,
		pc:      proto.NewConn(raw),
		name:    o.Name,
		shares:  o.Shares,
		onBatch: o.OnBatch,
		batch:   o.Batch,
	}

	body, err := json.Marshal(proto.HelloBody{
		Name:      o.Name,
		Role:      o.Role,
		Shares:    o.Shares.Names(),
		Aggregate: o.Aggregate,
	})
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("client: encode hello: %w", err)
	}
	if _, err := c.send(proto.KindHello, body, false); err != nil {
		c.Close()
		return nil, err
	}

	env, err := c.recvWithin(ackTimeout)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("client: waiting for hello reply: %w", err)
	}
	if env.Err != "" {
		c.Close()
		return nil, fmt.Errorf("client: server refused hello: %s", env.Err)
	}
	if env.Kind != proto.KindHelloOK {
		c.Close()
		return nil, fmt.Errorf("client: hello reply kind is %q, want %q", env.Kind, proto.KindHelloOK)
	}
	var ok proto.HelloOKBody
	if err := json.Unmarshal(env.Body, &ok); err != nil {
		c.Close()
		return nil, fmt.Errorf("client: decode hello reply: %w", err)
	}
	c.aggregate = ok.Aggregate
	return c, nil
}

// Name is the name this client registered under.
func (c *Client) Name() string { return c.name }

// Aggregate reports whether the server granted the aggregate role. A client
// that asked for it may still see false here.
func (c *Client) Aggregate() bool { return c.aggregate }

// maxBatchBody is the largest marshalled tree batch the client puts in one
// control frame, deliberately half of proto.MaxPayload.
//
// A node count cannot bound a frame: a frame is limited in bytes and a node's
// path is unbounded, so 4000 nodes with 195-byte paths already marshal to about
// 1.03MB — at the payload limit itself. Half rather than all of it, so the
// envelope's own JSON has room without a second calculation.
const maxBatchBody = proto.MaxPayload / 2

// UploadTree scans the shares and sends the result as tree batches, then closes
// the upload with a tree-done and waits for the server to acknowledge it.
//
// A batch is flushed when it reaches the configured node count or when adding
// one more node would push the marshalled body past maxBatchBody, whichever
// comes first: both limits are real, and the size one cannot be written as a
// node count.
//
// The batches are sent as they are scanned, so the server's index grows while
// the upload runs. OnBatch is called after each batch is sent.
func (c *Client) UploadTree() error {
	batch := c.batchSize()
	var sent int
	var pending []node.Node

	// encode marshals the pending nodes, so the budget is checked against
	// exactly the bytes that would go on the wire.
	encode := func() ([]byte, error) {
		nodes := make([]proto.NodeJSON, len(pending))
		for i, n := range pending {
			nodes[i] = proto.NodeJSON{
				Path:  n.Path,
				Kind:  uint8(n.Kind),
				Size:  n.Size,
				Mode:  n.Mode,
				Mtime: n.Mtime,
			}
		}
		return json.Marshal(proto.TreeBatchBody{Nodes: nodes})
	}

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		b, err := encode()
		if err != nil {
			return fmt.Errorf("client: encode tree batch: %w", err)
		}
		if _, err := c.send(proto.KindTreeBatch, b, false); err != nil {
			return err
		}
		sent += len(pending)
		pending = pending[:0]
		if c.onBatch != nil {
			c.onBatch(sent)
		}
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
		// A lone node is always sent on its own, so a path longer than the
		// whole budget cannot wedge the upload: WriteFrame reports the real
		// limit rather than the client looping for ever trying to fit it.
		if len(pending) > 0 {
			over, err := overflow()
			if err != nil {
				return err
			}
			if over || len(pending) >= batch {
				// Whatever is already buffered is what fits. Send it and wait
				// for the server to take it before reading more of the disk: a
				// client that streamed an entire million-file tree into a socket
				// without ever reading a reply deadlocks both ends once the
				// buffers fill.
				if err := flush(); err != nil {
					return err
				}
				if err := c.drainOne(); err != nil {
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
	// EOF marks this as the last message of the upload. The server refuses a
	// TreeDone without it, because an upload is only ever "the batches, then
	// the end" — there is no other way to say the sequence is over.
	id, err := c.send(proto.KindTreeDone, shares, true)
	if err != nil {
		return err
	}
	if err := c.waitForAck(id); err != nil {
		return err
	}
	return nil
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
	return c.conn.Close()
}

// send writes one control message and returns the ID it was given.
//
// Every send carries an ID: a reply echoes the ID of the request it answers, so
// a client that sent two messages both numbered zero could not tell the replies
// apart.
//
// eof marks the message as the last of a sequence. It is only meaningful for a
// message that ends one — today that is TreeDone — and is false otherwise.
func (c *Client) send(kind string, body []byte, eof bool) (uint64, error) {
	c.mu.Lock()
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

// drainOne reads and discards exactly one reply, failing if the server reported
// an error rather than acknowledging.
//
// The server acknowledges every batch, and those acknowledgements have to be
// consumed. A client that only sends would eventually fill the socket buffers
// and wedge both ends.
func (c *Client) drainOne() error {
	env, err := c.recvWithin(ackTimeout)
	if err != nil {
		return fmt.Errorf("client: waiting for batch acknowledgement: %w", err)
	}
	if env.Err != "" {
		return fmt.Errorf("client: server rejected a tree batch: %s", env.Err)
	}
	return nil
}

// waitForAck reads replies until the one correlating with id arrives, so a
// reply to an earlier request is never mistaken for the final one.
func (c *Client) waitForAck(id uint64) error {
	for {
		env, err := c.recvWithin(ackTimeout)
		if err != nil {
			return fmt.Errorf("client: waiting for upload acknowledgement: %w", err)
		}
		if env.Err != "" {
			return fmt.Errorf("client: server rejected the upload: %s", env.Err)
		}
		if env.ID == id {
			return nil
		}
	}
}

// recvWithin reads one envelope, bounding the wait so a server that goes quiet
// fails the call instead of hanging it.
func (c *Client) recvWithin(d time.Duration) (proto.Envelope, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		return proto.Envelope{}, err
	}
	defer c.conn.SetReadDeadline(time.Time{})
	return c.pc.Recv()
}
