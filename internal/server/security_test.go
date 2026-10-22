package server_test

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

// rejectHello sends one hello and returns the server's reply. It is the
// non-fatalf version of hello(), for handshakes that must be refused.
func rejectHello(t *testing.T, addr, name, role string) proto.Envelope {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial(%s): %v", addr, err)
	}
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	conn := proto.NewConn(c)

	body, err := json.Marshal(proto.HelloBody{Name: name, Role: role})
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
	return env
}

// A name is a path component, not free text. A separator or a dot segment in it
// would put the client's events inside another client's namespace.
func TestServerRejectsNamesThatAreNotOnePathComponent(t *testing.T) {
	s, addr := start(t)

	for _, tc := range []struct {
		name string
		role string
	}{
		{"a/b", proto.RoleClient},
		{"../victim", proto.RoleClient},
		{`a\b`, proto.RoleClient},
		{"/abs", proto.RoleClient},
		{".", proto.RoleViewer},
		{"a/b", proto.RoleViewer},
	} {
		env := rejectHello(t, addr, tc.name, tc.role)
		if env.Kind == proto.KindHelloOK && env.Err == "" {
			t.Errorf("hello name %q role %q was accepted; it is not one path component", tc.name, tc.role)
		}
		if env.Err == "" {
			t.Errorf("hello name %q role %q was refused without a reason", tc.name, tc.role)
		}
	}

	if live := s.Registry().Live(); len(live) != 0 {
		t.Fatalf("Live() = %v, want empty: no refused handshake may register a name", live)
	}
}

// A batch is one scan's output, so a malformed path anywhere in it refuses the
// whole batch rather than indexing the rest and silently dropping the bad node.
func TestServerRefusesABatchWithAnInvalidPath(t *testing.T) {
	s, addr := start(t)
	raw, _ := hello(t, addr, "win", proto.RoleClient, false)
	defer raw.Close()
	conn := proto.NewConn(raw)

	body, err := json.Marshal(proto.TreeBatchBody{Nodes: []proto.NodeJSON{
		{Path: "Documents", Kind: uint8(node.KindDir)},
		{Path: "../victim/x.txt", Kind: uint8(node.KindFile)},
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
		t.Fatalf("a batch with a traversal path was acknowledged: %+v", env)
	}
	if _, ok := s.Index().Lookup("win", "Documents"); ok {
		t.Fatal("the valid half of a refused batch was indexed; a batch must be all or nothing")
	}
}

// Events have no acknowledgement, so an invalid one is dropped rather than
// answered: forwarding it would let one client's path, after path.Dir cleans a
// dot segment, land in another client's cached directory. The connection stays
// usable for valid events.
func TestServerDropsEventsWithInvalidPaths(t *testing.T) {
	_, addr := start(t)

	vraw, _ := hello(t, addr, "viewer", proto.RoleViewer, false)
	defer vraw.Close()
	vconn := proto.NewConn(vraw)

	eraw, _ := hello(t, addr, "evil", proto.RoleClient, false)
	defer eraw.Close()
	econn := proto.NewConn(eraw)

	// The watcher is told that the supplier joined. Consuming it leaves the
	// watcher quiet, so the next read can only be the event under test.
	joined := readWatcherEvent(t, vconn)
	if len(joined) != 1 || joined[0].Path != "evil" {
		t.Fatalf("join event = %+v, want one node at evil", joined)
	}

	body, err := json.Marshal(proto.EventBody{Nodes: []proto.NodeJSON{
		{Path: "../victim/x.txt", Kind: uint8(node.KindDir)},
	}})
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}
	if err := econn.Send(proto.Envelope{Kind: proto.KindEvent, Body: body}); err != nil {
		t.Fatalf("Send event: %v", err)
	}

	if !vconn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)) {
		t.Fatal("SetReadDeadline is not supported")
	}
	if env, err := vconn.RecvAny(); err == nil {
		t.Fatalf("a watcher received an event with a traversal path: %+v", env)
	}
	if !vconn.SetReadDeadline(time.Now().Add(5 * time.Second)) {
		t.Fatal("SetReadDeadline is not supported")
	}

	// The drop is per batch, not a closed connection: a valid event from the
	// same client still arrives, with the client name on it.
	body, err = json.Marshal(proto.EventBody{Nodes: []proto.NodeJSON{
		{Path: "docs/a.txt", Kind: uint8(node.KindFile), Size: 3},
	}})
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}
	if err := econn.Send(proto.Envelope{Kind: proto.KindEvent, Body: body}); err != nil {
		t.Fatalf("Send event: %v", err)
	}
	got := readWatcherEvent(t, vconn)
	if len(got) != 1 || got[0].Path != "evil/docs/a.txt" {
		t.Fatalf("event = %+v, want one node at evil/docs/a.txt", got)
	}
}

func TestServerNegotiatesCompression(t *testing.T) {
	_, addr := start(t)

	for _, tc := range []struct {
		name     string
		compress bool
	}{
		{"plain", false},
		{"compressed", true},
	} {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		c.SetDeadline(time.Now().Add(15 * time.Second))
		conn := proto.NewConn(c)
		body, err := json.Marshal(proto.HelloBody{
			Name: tc.name, Role: proto.RoleClient, Compress: tc.compress,
		})
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
		var ok proto.HelloOKBody
		if err := json.Unmarshal(env.Body, &ok); err != nil {
			t.Fatalf("decode hello reply: %v", err)
		}
		if ok.Compress != tc.compress {
			t.Errorf("hello compress=%v got HelloOK compress=%v", tc.compress, ok.Compress)
		}
		c.Close()
	}
}
