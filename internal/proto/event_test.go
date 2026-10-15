package proto_test

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
	"time"

	"peerfold/internal/proto"
)

// pushEnvelope is a pushed index update: a control frame, no stream, and an ID
// of zero because it answers nothing.
func pushEnvelope(t *testing.T, path string) proto.Envelope {
	t.Helper()
	body, err := json.Marshal(proto.EventBody{Nodes: []proto.NodeJSON{{Path: path, Kind: 1, Size: 3}}})
	if err != nil {
		t.Fatalf("Marshal EventBody: %v", err)
	}
	return proto.Envelope{Kind: proto.KindEvent, Body: body}
}

// TestRecvSkipsAPushedUpdate pins that an update landing between a request and
// its reply does not become the reply.
//
// This is the frame order a viewer sees when the server handles another client's
// change first, and it is why Recv passes updates over rather than returning the
// next frame whatever it is: a caller waiting for its reply would otherwise read
// an event and fail with an ID that does not match.
func TestRecvSkipsAPushedUpdate(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline(t, a, b)
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	want := proto.Envelope{ID: 9, Kind: proto.KindListOK}
	done := sendAsync(func() error {
		if err := ca.Send(pushEnvelope(t, "win/Documents/a.txt")); err != nil {
			return err
		}
		return ca.Send(want)
	})

	got, err := cb.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Kind != want.Kind || got.ID != want.ID {
		t.Fatalf("Recv = kind %q id %d, want kind %q id %d", got.Kind, got.ID, want.Kind, want.ID)
	}
}

// TestRecvEventHandsTheUpdateOver pins the other half: a reader that wants the
// updates gets them, in order, rather than skipping them silently.
func TestRecvEventHandsTheUpdateOver(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline(t, a, b)
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	reply := proto.Envelope{ID: 4, Kind: proto.KindHelloOK}
	done := sendAsync(func() error {
		if err := ca.Send(pushEnvelope(t, "win/one")); err != nil {
			return err
		}
		if err := ca.Send(pushEnvelope(t, "win/two")); err != nil {
			return err
		}
		return ca.Send(reply)
	})

	var seen []string
	got, err := cb.RecvEvent(func(env proto.Envelope) {
		var b proto.EventBody
		if err := json.Unmarshal(env.Body, &b); err != nil {
			t.Errorf("decode handed-over update: %v", err)
			return
		}
		for _, n := range b.Nodes {
			seen = append(seen, n.Path)
		}
	})
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Kind != reply.Kind || got.ID != reply.ID {
		t.Fatalf("RecvEvent = kind %q id %d, want kind %q id %d", got.Kind, got.ID, reply.Kind, reply.ID)
	}
	if len(seen) != 2 || seen[0] != "win/one" || seen[1] != "win/two" {
		t.Fatalf("handed-over updates = %v, want [win/one win/two] in order", seen)
	}
}

// TestRecvAnyDoesNotSkipAPushedUpdate pins that the server's read is the
// unskipping one.
//
// A client's update is a frame the server has to act on. When Recv skipped
// updates everywhere, the server silently discarded every change a client
// reported, and the whole watch path produced nothing — which is exactly what a
// test that only exercised the client side would not have caught.
func TestRecvAnyDoesNotSkipAPushedUpdate(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline(t, a, b)
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	want := pushEnvelope(t, "win/Documents/a.txt")
	done := sendAsync(func() error { return ca.Send(want) })

	got, err := cb.RecvAny()
	if err != nil {
		t.Fatalf("RecvAny: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.Kind != proto.KindEvent {
		t.Fatalf("RecvAny kind = %q, want %q: the update was skipped", got.Kind, proto.KindEvent)
	}
}

// TestRecvBodySkipsAPushedUpdateMidStream pins that an update between two chunks
// of a reply body does not desynchronise the stream.
//
// The failure it guards against is "expected a data frame, got frame kind 1":
// the body reader takes the control frame for a chunk, and every later read of
// that connection is one frame out of step. It happened in practice, but only
// under load, so the frame order is written out here instead of being waited
// for.
func TestRecvBodySkipsAPushedUpdateMidStream(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline(t, a, b)
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	const id = 11
	want := []byte(`{"nodes":[{"path":"Documents/a.txt"}]}`)
	sent := sendAsync(func() error {
		if err := ca.SendData(id, want[:10]); err != nil {
			return err
		}
		// An update in flight while the body is still arriving.
		if err := ca.Send(pushEnvelope(t, "win/Documents/b.txt")); err != nil {
			return err
		}
		if err := ca.SendData(id, want[10:]); err != nil {
			return err
		}
		return ca.SendData(id, nil)
	})

	// The read runs in its own goroutine because net.Pipe has no buffer: the
	// sender cannot finish its second write until the first has been read, so a
	// test that read only after sending would deadlock rather than fail.
	type result struct {
		body []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		body, err := cb.RecvBody(id)
		got <- result{body, err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("RecvBody: %v", r.err)
		}
		if !bytes.Equal(r.body, want) {
			t.Fatalf("RecvBody = %q, want %q", r.body, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RecvBody did not return: the update between the chunks was not skipped")
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// TestRecvBodyEndsOnTheEndMarkerWhenOnlyAnUpdateFollows pins the end of the
// stream, not just its middle: the update arrives before the zero-length marker,
// so a reader that stopped skipping would wait for a chunk that never comes.
func TestRecvBodyEndsOnTheEndMarkerWhenOnlyAnUpdateFollows(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline(t, a, b)
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	const id = 12
	want := []byte("body")
	done := sendAsync(func() error {
		if err := ca.SendData(id, want); err != nil {
			return err
		}
		if err := ca.Send(pushEnvelope(t, "win/Documents/c.txt")); err != nil {
			return err
		}
		return ca.SendData(id, nil)
	})

	type result struct {
		body []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		body, err := cb.RecvBody(id)
		got <- result{body, err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("RecvBody: %v", r.err)
		}
		if !bytes.Equal(r.body, want) {
			t.Fatalf("RecvBody = %q, want %q", r.body, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RecvBody did not end: the update before the end marker was not skipped")
	}
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
}
