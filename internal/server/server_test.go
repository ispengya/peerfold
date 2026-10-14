package server_test

import (
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"peerfold/internal/node"
	"peerfold/internal/proto"
	"peerfold/internal/server"
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

	waitFor(t, "the viewer connection to finish", func() bool { return true })
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
