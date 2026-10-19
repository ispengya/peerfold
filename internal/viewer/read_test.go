package viewer_test

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/proto"
	"github.com/ispengya/peerfold/internal/viewer"
)

// answer is what the fake server does when it is asked for a file.
type answer struct {
	// content is the file body. A nil body is an empty file, which is a
	// successful read: the failure of a read travels in the head frame.
	content []byte
	// err, when set, is the head frame's error. It is what every failure looks
	// like on the wire, and the content stream is empty either way.
	err string
}

// startFakeServer accepts one viewer, completes the handshake, and then answers
// each read with the next scripted answer.
//
// It is a real TCP listener rather than a pipe so the frames travel the same way
// they do in production: the point of these tests is the frame sequence, and a
// second reader or a missing frame is exactly what a pipe would hide.
func startFakeServer(t *testing.T, answers []answer) string {
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

		// Handshake: read hello, answer hello_ok.
		env, err := conn.Recv()
		if err != nil || env.Kind != proto.KindHello {
			return
		}
		if err := conn.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHelloOK}); err != nil {
			return
		}

		for _, a := range answers {
			req, err := conn.Recv()
			if err != nil {
				return
			}
			if req.Kind != proto.KindRead {
				return
			}
			// The three parts, in order, all stamped with the requester's ID.
			head := proto.Envelope{ID: req.ID, Kind: proto.KindReadOK, Err: a.err}
			if err := conn.Send(head); err != nil {
				return
			}
			if a.err == "" && len(a.content) > 0 {
				if err := conn.SendBody(req.ID, a.content); err != nil {
					return
				}
			} else if err := conn.SendData(req.ID, nil); err != nil {
				return
			}
			if err := conn.Send(head); err != nil {
				return
			}
		}
		// Hold the connection open so the viewer does not see a close as a
		// leftover read failure.
		time.Sleep(200 * time.Millisecond)
	}()

	return ln.Addr().String()
}

// TestViewerCatReadsTheContent pins the successful round trip.
func TestViewerCatReadsTheContent(t *testing.T) {
	addr := startFakeServer(t, []answer{{content: []byte("hello world")}})
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	got, err := v.Cat("win/Documents/a.txt")
	if err != nil {
		t.Fatalf("Cat: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("Cat = %q, want %q", got, "hello world")
	}
}

// TestViewerCatOfAnEmptyFileIsASuccess pins the case that used to be reported as
// a failure: an empty stream with no head error is a zero-byte file.
func TestViewerCatOfAnEmptyFileIsASuccess(t *testing.T) {
	addr := startFakeServer(t, []answer{{content: nil}})
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	got, err := v.Cat("win/Documents/empty.txt")
	if err != nil {
		t.Fatalf("Cat of an empty file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Cat = %q, want nothing", got)
	}
}

// TestViewerCatReportsTheHeadError pins that a failed read is reported from the
// head frame. The stream is empty here exactly as it is for an empty file, so
// the head is the only thing that can carry the reason.
func TestViewerCatReportsTheHeadError(t *testing.T) {
	addr := startFakeServer(t, []answer{{err: "no such file"}})
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	_, err = v.Cat("win/Documents/nope.txt")
	if err == nil {
		t.Fatal("Cat = nil error, want the head frame's failure")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("Cat error = %q, want it to mention %q", err, "no such file")
	}
}

// TestViewerCatTwiceOnOneConnection pins that every read consumes all three
// parts of the previous answer.
//
// This is the viewer-side half of the same guarantee the server-side test
// covers: a read that stopped after the content would leave its closing frame to
// be read as the next answer, and the frame the viewer then parsed would be
// wrong in a way no single read can reveal.
func TestViewerCatTwiceOnOneConnection(t *testing.T) {
	addr := startFakeServer(t, []answer{
		{content: []byte("first")},
		{content: []byte("second")},
	})
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	for i, want := range []string{"first", "second"} {
		got, err := v.Cat("win/Documents/a.txt")
		if err != nil {
			t.Fatalf("Cat #%d: %v", i+1, err)
		}
		if string(got) != want {
			t.Fatalf("Cat #%d = %q, want %q", i+1, got, want)
		}
	}
}

// TestViewerReadReturnsARange pins that the range travels with the request, so a
// server honouring it is asked for the right bytes.
func TestViewerReadReturnsARange(t *testing.T) {
	addr := startFakeServer(t, []answer{{content: []byte("23456")}})
	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	got, err := v.Read("win/Documents/a.txt", 2, 5)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "23456" {
		t.Fatalf("Read = %q, want %q", got, "23456")
	}
}

// TestViewerReadEncodesTheRange pins the request body itself, since a viewer
// that dropped the offset would still get a well-formed answer.
func TestViewerReadEncodesTheRange(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	bodies := make(chan proto.ReadBody, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		conn := proto.NewConn(c)
		env, err := conn.Recv()
		if err != nil {
			return
		}
		_ = conn.Send(proto.Envelope{ID: env.ID, Kind: proto.KindHelloOK})
		req, err := conn.Recv()
		if err != nil {
			return
		}
		var b proto.ReadBody
		if err := json.Unmarshal(req.Body, &b); err == nil {
			bodies <- b
		}
		head := proto.Envelope{ID: req.ID, Kind: proto.KindReadOK}
		_ = conn.Send(head)
		_ = conn.SendData(req.ID, nil)
		_ = conn.Send(head)
		time.Sleep(200 * time.Millisecond)
	}()

	v, err := viewer.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer v.Close()

	if _, err := v.Read("win/Documents/a.txt", 7, 11); err != nil {
		t.Fatalf("Read: %v", err)
	}

	select {
	case b := <-bodies:
		if b.Client != "win" {
			t.Errorf("Client = %q, want %q", b.Client, "win")
		}
		if b.Path != "Documents/a.txt" {
			t.Errorf("Path = %q, want %q", b.Path, "Documents/a.txt")
		}
		if b.Off != 7 {
			t.Errorf("Off = %d, want 7", b.Off)
		}
		if b.N != 11 {
			t.Errorf("N = %d, want 11", b.N)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the read request")
	}
}
