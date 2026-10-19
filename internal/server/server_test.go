package server_test

import (
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
	"github.com/ispengya/peerfold/internal/server"
)

// start brings a server up on a loopback port and returns it with its address.
// The address is known before Serve is called, because New binds the listener.
func start(t *testing.T) (*server.Server, string) {
	t.Helper()
	s := server.New("127.0.0.1:0")
	if s.Addr() == nil {
		t.Fatal("Addr() is nil before Serve: New must bind the listener")
	}
	go func() {
		if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { s.Close() })
	return s, s.Addr().String()
}

// hello dials, sends one hello, and returns the connection plus the decoded
// reply. The caller owns the connection.
func hello(t *testing.T, addr, name, role string, aggregate bool) (net.Conn, proto.Envelope) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial(%s): %v", addr, err)
	}
	if err := c.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	conn := proto.NewConn(c)

	body, err := json.Marshal(proto.HelloBody{Name: name, Role: role, Aggregate: aggregate})
	if err != nil {
		t.Fatalf("Marshal hello: %v", err)
	}
	if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
		t.Fatalf("Send hello: %v", err)
	}
	env, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv hello reply: %v", err)
	}
	if env.Kind != proto.KindHelloOK {
		t.Fatalf("hello reply kind = %q, want %q (err %q)", env.Kind, proto.KindHelloOK, env.Err)
	}
	if env.Err != "" {
		t.Fatalf("hello reply carried an error: %q", env.Err)
	}
	return c, env
}

func helloOKBody(t *testing.T, env proto.Envelope) proto.HelloOKBody {
	t.Helper()
	var b proto.HelloOKBody
	if err := json.Unmarshal(env.Body, &b); err != nil {
		t.Fatalf("decode HelloOKBody: %v", err)
	}
	return b
}

// waitFor polls until cond holds, so a test never depends on how quickly a
// closed connection's handler gets around to unregistering.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestServerHelloGrantsAggregateToFirstClient(t *testing.T) {
	s, addr := start(t)

	c1, env1 := hello(t, addr, "a", proto.RoleClient, true)
	defer c1.Close()
	got1 := helloOKBody(t, env1)
	if !got1.Aggregate {
		t.Fatal("first client asked for aggregate and was refused")
	}
	if got1.Name != "a" {
		t.Fatalf("HelloOK name = %q, want a", got1.Name)
	}

	// The second client is downgraded, and the downgrade is visible on the
	// wire rather than only in the registry.
	c2, env2 := hello(t, addr, "b", proto.RoleClient, true)
	defer c2.Close()
	got2 := helloOKBody(t, env2)
	if got2.Aggregate {
		t.Fatal("second client also got the aggregate role; v1 allows exactly one")
	}
	if got2.Name != "b" {
		t.Fatalf("HelloOK name = %q, want b", got2.Name)
	}

	if owner, ok := s.Registry().AggregateOwner(); !ok || owner != "a" {
		t.Fatalf("AggregateOwner() = (%q, %v), want (a, true)", owner, ok)
	}
}

func TestServerViewerIsNotInRegistry(t *testing.T) {
	s, addr := start(t)
	vc, env := hello(t, addr, "viewer-1", proto.RoleViewer, false)
	defer vc.Close()

	if owner, ok := s.Registry().AggregateOwner(); ok {
		t.Fatalf("a viewer became the aggregate owner: %q", owner)
	}
	// Give any (incorrect) registration a chance to land before asserting.
	time.Sleep(50 * time.Millisecond)
	if live := s.Registry().Live(); len(live) != 0 {
		t.Fatalf("Registry.Live() = %v, want empty: a viewer is not a supplier", live)
	}
	// A viewer still gets a well-formed hello reply.
	if b := helloOKBody(t, env); b.Name != "viewer-1" {
		t.Fatalf("HelloOK name = %q, want viewer-1", b.Name)
	}
}

func TestServerDisconnectUnregistersAndClearsIndex(t *testing.T) {
	s, addr := start(t)

	c1, _ := hello(t, addr, "a", proto.RoleClient, true)
	defer c1.Close()
	c2, _ := hello(t, addr, "b", proto.RoleClient, false)

	s.Index().Apply("b", []node.Node{{Path: "Documents", Kind: node.KindDir}})
	if _, ok := s.Index().Lookup("b", "Documents"); !ok {
		t.Fatal("index did not accept a node for b")
	}

	if err := c2.Close(); err != nil {
		t.Fatalf("Close second connection: %v", err)
	}

	waitFor(t, "b to be unregistered", func() bool {
		return len(s.Registry().Live()) == 1
	})
	if live := s.Registry().Live(); live[0] != "a" {
		t.Fatalf("Live() = %v, want [a]", live)
	}
	// A departing client's nodes must not linger in the aggregated tree.
	waitFor(t, "b's index entries to be dropped", func() bool {
		_, ok := s.Index().Lookup("b", "Documents")
		return !ok
	})
	// The aggregate owner is unaffected by a plain supplier leaving.
	if owner, ok := s.Registry().AggregateOwner(); !ok || owner != "a" {
		t.Fatalf("AggregateOwner() = (%q, %v), want (a, true)", owner, ok)
	}
}

func TestServerCloseStopsListening(t *testing.T) {
	s, addr := start(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		t.Fatal("Dial succeeded after Close")
	}
	// Close is idempotent enough for cleanup paths.
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Closing the connections is the only thing that unblocks a handler parked in a
// read, and Serve — not Close — is the drain point. Without the connection
// close, a handler stays in Recv forever and Serve never returns.
func TestServerCloseReleasesParkedHandlers(t *testing.T) {
	s := server.New("127.0.0.1:0")
	if s.Addr() == nil {
		t.Fatal("Addr() is nil before Serve")
	}
	addr := s.Addr().String()
	serveDone := make(chan error, 1)
	go func() { serveDone <- s.Serve() }()

	c, env := hello(t, addr, "a", proto.RoleClient, true)
	if b := helloOKBody(t, env); !b.Aggregate {
		t.Fatal("the only client was not granted the aggregate role")
	}
	_ = c // leave it open: the handler is now parked in Recv

	live := []string{}
	waitFor(t, "the client to register", func() bool {
		live = s.Registry().Live()
		return len(live) == 1
	})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v after Close, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after Close: a handler is still parked in a read")
	}

	// Serve only returns once the handlers have exited, so by here the client
	// must be gone from the registry.
	if live := s.Registry().Live(); len(live) != 0 {
		t.Fatalf("Live() = %v after Serve returned, want empty", live)
	}
}

// An unrecognized role must be refused rather than quietly granted the viewer
// path, which would tell a misconfigured client it succeeded.
func TestServerRejectsUnknownRole(t *testing.T) {
	s, addr := start(t)

	// Each refusal ends the connection, so each role needs its own.
	for _, role := range []string{"", "Client", "viewer ", "admin"} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("Dial for role %q: %v", role, err)
		}
		c.SetDeadline(time.Now().Add(15 * time.Second))
		conn := proto.NewConn(c)

		body, err := json.Marshal(proto.HelloBody{Name: "a", Role: role})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
			t.Fatalf("Send for role %q: %v", role, err)
		}
		env, err := conn.Recv()
		if err != nil {
			t.Fatalf("Recv for role %q: %v", role, err)
		}
		if env.Err == "" {
			t.Errorf("role %q was accepted, want a refusal", role)
		}
		c.Close()
	}
	if live := s.Registry().Live(); len(live) != 0 {
		t.Fatalf("Live() = %v, want empty: no rejected hello may register", live)
	}
}

func TestServerRejectsNonHelloFirstMessage(t *testing.T) {
	_, addr := start(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	conn := proto.NewConn(c)

	if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindList, Body: json.RawMessage(`{"client":"a","path":""}`)}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	env, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	// Task 5 only implements hello; anything else must be refused explicitly
	// rather than answered with a plausible-looking success.
	if env.Kind != proto.KindList {
		t.Fatalf("refusal kind = %q, want %q (the request's kind is echoed back)", env.Kind, proto.KindList)
	}
	if env.Err == "" {
		t.Fatal("the refuse-everything-else path answered with no error; Task 5 implements hello only")
	}
}

// A malformed hello must be refused without registering anything. This cannot
// use the hello helper, which expects a successful reply.
func TestServerRejectsBadHello(t *testing.T) {
	s, addr := start(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	conn := proto.NewConn(c)

	body, err := json.Marshal(proto.HelloBody{Role: proto.RoleClient}) // no name
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	env, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if env.Err == "" {
		t.Fatal("a nameless client was accepted; a hello with no name must be refused")
	}
	if live := s.Registry().Live(); len(live) != 0 {
		t.Fatalf("Live() = %v, want empty: a nameless client must not be registered", live)
	}
}

// A viewer connection that goes away must not disturb the suppliers.
func TestServerViewerDisconnectIsHarmless(t *testing.T) {
	s, addr := start(t)
	c1, _ := hello(t, addr, "a", proto.RoleClient, true)
	defer c1.Close()
	vc, _ := hello(t, addr, "v", proto.RoleViewer, false)
	vc.Close()

	// A viewer is never in the registry, so its departure has nothing to
	// observe; the sleep gives a (wrong) registration time to show up.
	time.Sleep(50 * time.Millisecond)
	if live := s.Registry().Live(); len(live) != 1 || live[0] != "a" {
		t.Fatalf("Live() = %v, want [a]", live)
	}
}

// Two clients with the same name must not both hold a slot.
func TestServerDuplicateNameIsRejected(t *testing.T) {
	s, addr := start(t)
	c1, _ := hello(t, addr, "a", proto.RoleClient, false)
	defer c1.Close()

	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(15 * time.Second))
	conn := proto.NewConn(c2)
	body, _ := json.Marshal(proto.HelloBody{Name: "a", Role: proto.RoleClient})
	if err := conn.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	env, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if env.Kind == proto.KindHelloOK && env.Err == "" {
		t.Fatal("a duplicate name was welcomed; it must be refused")
	}
	if live := s.Registry().Live(); len(live) != 1 {
		t.Fatalf("Live() = %v, want one entry", live)
	}
}

// The IdleTimeout-style bound: a connection that sends nothing must not wedge
// the accept loop or hold a registry slot forever.
func TestServerAcceptsAfterASilentConnection(t *testing.T) {
	s, addr := start(t)

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer silent.Close()

	// A real client is still served while the silent one sits there.
	c1, _ := hello(t, addr, "a", proto.RoleClient, true)
	defer c1.Close()
	if live := s.Registry().Live(); len(live) != 1 {
		t.Fatalf("Live() = %v, want [a]: a silent connection must not be registered", live)
	}
}

// TestServerIndexesTreeBatchImmediately pins that a batch is applied on arrival
// rather than accumulated until the upload ends: the tree is meant to grow
// while the upload runs, which is what lets a client show progress.
func TestServerIndexesTreeBatchImmediately(t *testing.T) {
	s, addr := start(t)
	raw, _ := hello(t, addr, "laptop", proto.RoleClient, false)
	defer raw.Close()
	conn := proto.NewConn(raw)

	sendBatch(t, conn, 2, []proto.NodeJSON{
		{Path: "Documents", Kind: uint8(node.KindDir)},
	})

	// No TreeDone has been sent, so the batch must already be visible.
	if _, ok := s.Index().Lookup("laptop", "Documents"); !ok {
		t.Fatal("a batch is only indexed at the end of the upload; it must be applied on arrival")
	}
}

// TestServerTreeDoneKeepsTheConnection pins the difference between ending an
// upload and ending a connection. The index belongs to the connection, so a
// server that closed on TreeDone would delete the tree the client just
// published — after telling it the upload succeeded.
func TestServerTreeDoneKeepsTheConnection(t *testing.T) {
	s, addr := start(t)
	raw, _ := hello(t, addr, "laptop", proto.RoleClient, false)
	defer raw.Close()
	conn := proto.NewConn(raw)

	sendBatch(t, conn, 2, []proto.NodeJSON{
		{Path: "Documents", Kind: uint8(node.KindDir)},
	})
	done, err := json.Marshal(proto.TreeDoneBody{Shares: []string{"Documents"}})
	if err != nil {
		t.Fatalf("Marshal tree done: %v", err)
	}
	if err := conn.Send(proto.Envelope{ID: 3, Kind: proto.KindTreeDone, Body: done, EOF: true}); err != nil {
		t.Fatalf("Send tree done: %v", err)
	}
	ack, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv tree-done ack: %v", err)
	}
	if ack.Err != "" {
		t.Fatalf("tree-done ack carried an error: %q", ack.Err)
	}
	if ack.ID != 3 {
		t.Errorf("ack ID = %d, want 3", ack.ID)
	}

	// The connection must still work, and the tree must still be there.
	if _, ok := s.Index().Lookup("laptop", "Documents"); !ok {
		t.Fatal("the tree was dropped when the upload finished; TreeDone must not close the connection")
	}
	if live := s.Registry().Live(); len(live) != 1 || live[0] != "laptop" {
		t.Fatalf("Live() = %v, want [laptop]", live)
	}

	// A second upload over the same connection must be accepted, which also
	// proves the loop is still reading.
	sendBatch(t, conn, 4, []proto.NodeJSON{
		{Path: "Documents/second.txt", Kind: uint8(node.KindFile)},
	})
	if _, ok := s.Index().Lookup("laptop", "Documents/second.txt"); !ok {
		t.Fatal("the connection stopped serving after TreeDone")
	}
}

// TestServerRejectsTreeDoneWithoutEOF pins the end-of-upload marker.
//
// An upload is several batches followed by one TreeDone, and every one of them
// travels as a control frame. The EOF flag is what distinguishes "this was the
// last message of the upload" from "there may be more", so a TreeDone without
// it is an unfinished upload and must not be acknowledged as complete.
func TestServerRejectsTreeDoneWithoutEOF(t *testing.T) {
	s, addr := start(t)
	raw, _ := hello(t, addr, "laptop", proto.RoleClient, false)
	defer raw.Close()
	conn := proto.NewConn(raw)

	sendBatch(t, conn, 2, []proto.NodeJSON{
		{Path: "Documents", Kind: uint8(node.KindDir)},
	})
	done, err := json.Marshal(proto.TreeDoneBody{Shares: []string{"Documents"}})
	if err != nil {
		t.Fatalf("Marshal tree done: %v", err)
	}
	// Deliberately no EOF.
	if err := conn.Send(proto.Envelope{ID: 3, Kind: proto.KindTreeDone, Body: done}); err != nil {
		t.Fatalf("Send tree done: %v", err)
	}

	ack, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv tree-done reply: %v", err)
	}
	if ack.Err == "" {
		t.Fatal("a TreeDone without the EOF marker was accepted; an upload must be marked as finished")
	}
	// The refusal must not take the connection down: the client can still
	// send the correctly marked TreeDone and finish.
	if err := conn.Send(proto.Envelope{ID: 4, Kind: proto.KindTreeDone, Body: done, EOF: true}); err != nil {
		t.Fatalf("Send marked tree done: %v", err)
	}
	ack2, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv marked tree-done reply: %v", err)
	}
	if ack2.Err != "" {
		t.Fatalf("a correctly marked TreeDone was refused too: %q", ack2.Err)
	}
	if _, ok := s.Index().Lookup("laptop", "Documents"); !ok {
		t.Fatal("the tree is missing after a correctly marked upload")
	}
}

// TestServerRejectsTreeUploadFromViewer pins that only a registered client
// owns index entries: a viewer supplies nothing, so an upload from one must be
// refused rather than silently indexed under an empty name.
func TestServerRejectsTreeUploadFromViewer(t *testing.T) {
	s, addr := start(t)
	raw, _ := hello(t, addr, "watcher", proto.RoleViewer, false)
	defer raw.Close()
	conn := proto.NewConn(raw)

	body, err := json.Marshal(proto.TreeBatchBody{Nodes: []proto.NodeJSON{
		{Path: "Documents", Kind: uint8(node.KindDir)},
	}})
	if err != nil {
		t.Fatalf("Marshal batch: %v", err)
	}
	if err := conn.Send(proto.Envelope{ID: 2, Kind: proto.KindTreeBatch, Body: body}); err != nil {
		t.Fatalf("Send batch: %v", err)
	}
	env, err := conn.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if env.Err == "" {
		t.Fatalf("a viewer's tree upload was accepted; reply = %+v", env)
	}
	if got := s.Index().Len(); got != 0 {
		t.Errorf("Len() = %d, want 0: a refused upload must index nothing", got)
	}
}

func sendBatch(t *testing.T, conn *proto.Conn, id uint64, nodes []proto.NodeJSON) {
	t.Helper()
	body, err := json.Marshal(proto.TreeBatchBody{Nodes: nodes})
	if err != nil {
		t.Fatalf("Marshal batch: %v", err)
	}
	if err := conn.Send(proto.Envelope{ID: id, Kind: proto.KindTreeBatch, Body: body}); err != nil {
		t.Fatalf("Send batch: %v", err)
	}
	// The server acknowledges every batch; consume it so the next read is the
	// frame the test is actually waiting for.
	if _, err := conn.Recv(); err != nil {
		t.Fatalf("Recv batch ack: %v", err)
	}
}
