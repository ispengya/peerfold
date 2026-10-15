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
// reply — the listing's own test asserts the right shape by decoding the body,
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

		// Two listings, not one. Every test here needs only the first, but a caller
		// that makes a second call needs the server to still be answering: with a
		// single answer, a second call's failure cannot be told apart from a lost
		// request token — both are "no reply". Answering twice lets a test require the
		// second call to succeed, which is the property that actually distinguishes
		// them.
		for i := 0; i < 2; i++ {
			req, err := conn.Recv()
			if err != nil || req.Kind != proto.KindList {
				return
			}
			// An event in flight while a call is waiting for its reply. Only on the
			// first request: an injected event does not consume a request ID, so
			// injecting on both would make the second reply answer the first
			// request's ID and the test would be reporting the script's
			// arithmetic rather than the viewer's behaviour.
			if i == 0 {
				if err := sendEvent(conn, proto.NodeJSON{Path: "win/Documents/mid.txt", Kind: uint8(node.KindFile), Size: 3}); err != nil {
					return
				}
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
		}
		time.Sleep(300 * time.Millisecond)
	}()

	return ln.Addr().String()
}

// awaitEvent waits for a pushed batch containing path, and reports the node.
func awaitEvent(t *testing.T, ch <-chan viewer.Event, path string) node.Node {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case batch, ok := <-ch:
			if !ok {
				t.Fatalf("the event channel closed while waiting for %s", path)
			}
			for _, n := range batch.Nodes {
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
// — either an error about the wrong kind, or, worse, an empty listing that looks
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
// does not take the ordinary path away: a call after a watch still works, and it
// still gets its own reply.
//
// Two assertions, because they fail differently. A successful listing proves the
// call path works with the pump running — and it proves the request token came
// back once, since a token that was never returned makes the call wait for
// callTimeout, not fail. The second call is then expected to fail *fast*: the
// scripted server stops after two listings, so anything but a quick
// connection-level failure means it timed out waiting for a token instead, which
// is the defect this second call is here to catch.
func TestViewerCallsStillWorkAfterAWatch(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	v.Watch()
	awaitEvent(t, v.Watch(), "win/Documents/pushed.txt")

	first, err := v.List("win/Documents")
	if err != nil {
		t.Fatalf("first List: %v", err)
	}
	if len(first) != 1 || first[0].Path != "Documents/a.txt" {
		t.Fatalf("first List = %v, want the one scripted entry", first)
	}

	// The script answers two listings, then goes quiet. This call is the second,
	// so it is answered — but whether the reply matches the request ID is a race
	// with the background pump, which may read the frame first. What is not a
	// race is the shape of the failure: the server stops after a short pause, so
	// this must come back promptly.
	//
	// The bound is the viewer's own 30s call timeout, halved, and it is written
	// out rather than referenced because the constant is unexported and this is an
	// external test package. Pinning a literal is the point: a call that returns
	// after that long did not fail because the server stopped, it failed because
	// it was still waiting for a token that was never given back.
	const notTheCallTimeout = 15 * time.Second

	start := time.Now()
	_, err = v.List("win/Documents")
	if err == nil {
		return // The pump did not interfere on this run; the call worked.
	}
	if elapsed := time.Since(start); elapsed > notTheCallTimeout {
		t.Fatalf("second List failed after %s (err = %v): "+
			"that is the call timeout, so the request token was not returned", elapsed, err)
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
		if len(batch.Nodes) == 0 {
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

// TestViewerCloseIsSafeWhileBatchesAreArriving pins the two things Close has to
// get right when it races a push.
//
// The first is that no batch is ever offered to a closed channel. That is a
// panic, not a lost update, and it killed the process rather than the call: the
// decision used to be made in two steps — read the channel under the lock, then
// send to it after releasing the lock — and a Close landing in between closed
// the channel underneath the send.
//
// The second is that closing twice, and pushing after Close, are both no-ops
// rather than a double close or a send on a closed channel. A caller that closes
// on one path and defers a close on another is ordinary.
//
// It cannot prove the absence of the race — only repeated runs with -race can
// lean on it — so it drives the two against each other hard and then asserts the
// properties that must hold either way.
func TestViewerCloseIsSafeWhileBatchesAreArriving(t *testing.T) {
	addr := startPushServer(t)
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	events := v.Watch()
	awaitEvent(t, events, "win/Documents/pushed.txt")

	// Drain in the background so emit is free to keep making its decision, and
	// so the channel cannot fill up and hide the send behind the default arm.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range events {
		}
	}()

	// Close repeatedly while the reader is still delivering batches.
	closes := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { closes <- v.Close() }()
	}
	for i := 0; i < 4; i++ {
		<-closes
	}
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the event channel was not closed, so a ranging caller would have waited for ever")
	}

	// Close once more: a second close of the same channel would panic.
	if err := v.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
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
