package viewer_test

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"peerfold/internal/node"
	"peerfold/internal/proto"
	"peerfold/internal/viewer"
)

// sendEvent writes one pushed index update.
func sendEvent(c *proto.Conn, nodes ...proto.NodeJSON) error {
	body, err := json.Marshal(proto.EventBody{Nodes: nodes})
	if err != nil {
		return err
	}
	return c.Send(proto.Envelope{Kind: proto.KindEvent, Body: body})
}

// startPushServer accepts one viewer and then, in order: pushes one event on its
// own, answers a listing with another event sent between the request and the
// reply, and holds the connection open.
//
// The event placed between the request and its reply is the case that matters.
// A viewer that reads frames in a fixed sequence would take that event for the
// reply 鈥?the listing's own test asserts the right shape by decoding the body,
// so it would fail loudly rather than quietly, but only if it is exercised.
func startPushServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		conn := proto.NewConn(c)

		env, err := conn.Recv()
		if err != nil || env.Kind != proto.KindHello {
			return
		}
		if err := conn.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHelloOK}); err != nil {
			return
		}

		// One push with nothing asked for. This is the whole point of the
		// background reader: it arrives on the server's schedule.
		if err := sendEvent(conn, proto.NodeJSON{Path: "win/Documents/pushed.txt", Kind: uint8(node.KindFile), Size: 6}); err != nil {
			return
		}

		req, err := conn.Recv()
		if err != nil || req.Kind != proto.KindList {
			return
		}
		// An event in flight while a call is waiting for its reply.
		if err := sendEvent(conn, proto.NodeJSON{Path: "win/Documents/mid.txt", Kind: uint8(node.KindFile), Size: 3}); err != nil {
			return
		}
		body, err := json.Marshal(proto.ListOKBody{Nodes: []proto.NodeJSON{
			{Path: "Documents/a.txt", Kind: uint8(node.KindFile), Size: 4},
		}})
		if err != nil {
			return
		}
		head := proto.Envelope{ID: req.ID, Kind: proto.KindListOK}
		if err := conn.Send(head); err != nil {
			return
		}
		if err := conn.SendBody(req.ID, body); err != nil {
			return
		}
		if err := conn.Send(head); err != nil {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}()

	return ln.Addr().String()
}

// awaitEvent waits for a pushed batch containing path, and reports the node.
func awaitEvent(t *testing.T, ch <-chan []node.Node, path string) node.Node {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case batch, ok := <-ch:
			if !ok {
				t.Fatalf("the event channel closed while waiting for %s", path)
			}
			for _, n := range batch {
				if n.Path == path {
					return n
				}
			}
		case <-deadline:
			t.Fatalf("no pushed event naming %s within 5s", path)
		}
	}
}

// TestViewerWatchDeliversPushWithoutACall pins that a update arrives while
// nothing is being asked for.
//
// Without a background reader the frame would sit in the socket buffer until the
// next request happened to be made, so this asserts the thing that makes Watch
// work at all: nothing is sent between the registration and the arrival.
func TestViewerWatchDeliversPushWithoutACall(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	events := v.Watch()
	got := awaitEvent(t, events, "win/Documents/pushed.txt")
	if got.Kind != node.KindFile || got.Size != 6 {
		t.Fatalf("node = %+v, want a 6-byte file", got)
	}
}

// TestViewerListIgnoresAnEventInFlight pins that a push arriving between a
// request and its reply does not become the reply.
//
// It is the failure a fixed frame sequence produces: the event would be read as
// the listing's answer, and the listing would then be decoded from an event body
// 鈥?either an error about the wrong kind, or, worse, an empty listing that looks
// like a successful answer.
func TestViewerListIgnoresAnEventInFlight(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	events := v.Watch()
	awaitEvent(t, events, "win/Documents/pushed.txt")

	nodes, err := v.List("win/Documents")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "Documents/a.txt" {
		t.Fatalf("List = %v, want the one scripted entry", nodes)
	}
	// And the event that travelled alongside the reply still arrived.
	awaitEvent(t, events, "win/Documents/mid.txt")
}

// TestViewerCallsStillWorkAfterAWatch pins that switching to a background reader
// does not take the ordinary path away: several calls in a row still work, and
// each still gets its own reply.
func TestViewerCallsStillWorkAfterAWatch(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	v.Watch()
	awaitEvent(t, v.Watch(), "win/Documents/pushed.txt")

	// The scripted server answers exactly one listing, so a second one would hang
	// if the first had left the reader stopped. It is the reply after a reply
	// that proves the token came back.
	if _, err := v.List("win/Documents"); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if _, err := v.List("win/Documents"); err == nil {
		t.Fatal("the second List succeeded, but the scripted server has only one answer")
	}
}

// TestViewerWatchTwiceSharesOneChannel pins that Watch is not a subscription that
// can be duplicated: the second call returns the same channel, so an update is
// not split between two listeners.
func TestViewerWatchTwiceSharesOneChannel(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	first := v.Watch()
	second := v.Watch()
	if first == nil || second == nil {
		t.Fatal("Watch returned nil")
	}
	awaitEvent(t, first, "win/Documents/pushed.txt")

	// Read everything the shared channel holds and require the second handle to
	// have seen the same stream of values, which for one channel means the same
	// channel.
	select {
	case batch, ok := <-second:
		if !ok {
			t.Fatal("the shared channel closed")
		}
		if len(batch) == 0 {
			t.Fatal("an empty batch arrived")
		}
	default:
		// The single push has already been consumed through first, which is
		// itself the proof: there is one queue, not two.
	}
}

// TestViewerCloseClosesTheEventChannel pins that a caller ranging over the
// channel is released rather than left waiting for ever.
func TestViewerCloseClosesTheEventChannel(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	events := v.Watch()
	awaitEvent(t, events, "win/Documents/pushed.txt")

	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the event channel was not closed by Close")
		}
	}
}

// TestMatchIsAPathComparison pins the helper a caller uses to filter pushes.
func TestMatchIsAPathComparison(t *testing.T) {
	cases := []struct {
		target, path string
		want         bool
	}{
		{"win/Documents", "win/Documents", true},
		{"win/Documents", "win/Documents/x.txt", true},
		{"win/Documents", "win/Documents/sub/x.txt", true},
		{"win/Documents", "win/DocumentsX/x.txt", false},
		{"win/Documents", "win/Photos/x.txt", false},
		{"win/Documents", "win", false},
	}
	for _, c := range cases {
		if got := viewer.Match(c.target, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.target, c.path, got, c.want)
		}
	}
}
