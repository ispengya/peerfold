package server_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"peerfold/internal/proto"
)

// A write answer is the same three parts a read answer is — a head control
// frame, a content stream ended by its marker, and a closing control frame — and
// these tests pin that shape at the server rather than through a viewer.
//
// The shape is the contract, and it is worth pinning where it lives. The
// end-to-end tests prove a write works; they cannot say which of the three parts
// a broken version dropped, and a part left unsent does not stay put — frames
// are one sequence, so the next answer is then read from the middle of this one
// and the failure surfaces somewhere else entirely.
//
// Writes differ from reads in one way that these tests also pin: the content
// travels the other way, from the requester to the owner, so every answer's
// stream is empty. An empty stream is still a stream, and still has its marker.

// writeOwner answers write requests on an owner connection.
//
// It reads the request's content stream to its end before answering, which is
// what a real client does and what keeps the connection at a frame boundary. The
// answer is write_ok — the kind that exists so an answer cannot be taken for a
// request, since the two are otherwise identical in shape.
//
// fail names an error to put in the answer's head, or "" for success. seen
// receives one line per answered request so a test can check what the owner was
// asked to do.
func writeOwner(t *testing.T, conn *proto.Conn, fail string, seen chan<- string) {
	t.Helper()
	go func() {
		for {
			env, err := conn.Recv()
			if err != nil {
				return
			}
			if !(env.Kind == proto.KindWrite || env.Kind == proto.KindMkdir ||
				env.Kind == proto.KindRemove || env.Kind == proto.KindRename) {
				return
			}
			// Drain the request's content, whatever kind it is: it is always
			// there, and leaving it would make the next control frame out of a
			// data frame.
			for {
				_, done, err := conn.RecvData(env.ID)
				if err != nil || done {
					break
				}
			}
			if seen != nil {
				select {
				case seen <- env.Kind + " " + string(env.Body):
				default:
				}
			}
			head := proto.Envelope{ID: env.ID, Kind: proto.KindWriteOK, Err: fail}
			if err := conn.Send(head); err != nil {
				return
			}
			if err := conn.SendData(env.ID, nil); err != nil {
				return
			}
			if err := conn.Send(head); err != nil {
				return
			}
		}
	}()
}

// askWrite sends a write request and reads the three parts of its answer.
//
// An answer's stream is always empty, so the loop reads one marker and stops.
// The test asserts that emptiness rather than tolerating it: a write answer that
// carried content would mean the server had confused the direction the bytes
// travel in, and the assertion is what would notice.
func askWrite(t *testing.T, conn *proto.Conn, id uint64, kind string, body []byte, content []byte) (proto.Envelope, []byte, proto.Envelope) {
	t.Helper()
	env := proto.Envelope{ID: id, Kind: kind, Body: body}
	if content != nil {
		// The announcement and its content go out under one hold of the write
		// lock, which is what keeps another frame from landing between them.
		var sent bool
		err := conn.SendAnnounced(id, env, func(_ uint64, send func([]byte) error) error {
			sent = true
			if len(content) > 0 {
				if err := send(content); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("SendAnnounced %s %d: %v", kind, id, err)
		}
		if !sent {
			t.Fatalf("SendAnnounced %s %d never ran its content callback", kind, id)
		}
	} else if err := conn.Send(env); err != nil {
		t.Fatalf("Send %s %d: %v", kind, id, err)
	}

	head, err := conn.Recv()
	if err != nil {
		t.Fatalf("%s %d: head: %v", kind, id, err)
	}
	var got []byte
	for {
		chunk, done, err := conn.RecvData(id)
		if err != nil {
			t.Fatalf("%s %d: stream: %v", kind, id, err)
		}
		if done {
			break
		}
		got = append(got, chunk...)
	}
	tail, err := conn.Recv()
	if err != nil {
		t.Fatalf("%s %d: closing frame: %v", kind, id, err)
	}
	return head, got, tail
}

// checkWriteShape asserts the three parts are present, stamped with the
// requester's own ID, of the kind the requester waits on, and that the stream
// between them was empty.
func checkWriteShape(t *testing.T, id uint64, head proto.Envelope, content []byte, tail proto.Envelope, wantErr string) {
	t.Helper()
	if head.Kind != proto.KindWriteOK {
		t.Fatalf("head kind = %q, want %q", head.Kind, proto.KindWriteOK)
	}
	if head.ID != id {
		t.Fatalf("head id = %d, want %d", head.ID, id)
	}
	if tail.Kind != proto.KindWriteOK {
		t.Fatalf("closing frame kind = %q, want %q", tail.Kind, proto.KindWriteOK)
	}
	if tail.ID != id {
		t.Fatalf("closing frame id = %d, want %d", tail.ID, id)
	}
	if len(content) != 0 {
		t.Fatalf("answer stream = %q, want it empty: a write's content travels the other way", content)
	}
	switch {
	case wantErr == "" && head.Err != "":
		t.Fatalf("head carried %q, want no error", head.Err)
	case wantErr != "" && head.Err == "":
		t.Fatalf("head carried no error, want one mentioning %q", wantErr)
	case wantErr != "" && !strings.Contains(head.Err, wantErr):
		t.Fatalf("head error = %q, want it to mention %q", head.Err, wantErr)
	}
}

// TestServerWriteAnswerIsThreeParts pins the shape for a successful put: the
// answer carries no content of its own, but it still has a stream, and the
// stream still has its marker.
func TestServerWriteAnswerIsThreeParts(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	serve := proto.NewConn(owner)
	seen := make(chan string, 4)
	writeOwner(t, serve, "", seen)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	body, err := json.Marshal(proto.WriteBody{Client: "win", Path: "Documents/a.txt", Mode: proto.WriteModePut})
	if err != nil {
		t.Fatalf("Marshal WriteBody: %v", err)
	}
	head, got, tail := askWrite(t, conn, 1, proto.KindWrite, body, []byte("the content"))
	checkWriteShape(t, 1, head, got, tail, "")

	select {
	case line := <-seen:
		if !strings.HasPrefix(line, proto.KindWrite+" ") {
			t.Fatalf("the owner was asked %q, want a %s request", line, proto.KindWrite)
		}
		if !strings.Contains(line, "Documents/a.txt") {
			t.Fatalf("the owner was asked %q, want it to name the path", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was never asked to write")
	}
}

// TestServerWriteAnswerShapeHoldsOnFailure pins that a failure is an answer too:
// the owner's reason arrives in the head, and the stream and closing frame are
// still sent.
//
// This is the case that matters most, because it is the one that breaks silently.
// A version that dropped the closing frame on failure would pass every success
// test and then put the connection one frame out of step for the next request.
func TestServerWriteAnswerShapeHoldsOnFailure(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	writeOwner(t, proto.NewConn(owner), "the disk is full", nil)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	body, err := json.Marshal(proto.WriteBody{Client: "win", Path: "Documents/a.txt", Mode: proto.WriteModePut})
	if err != nil {
		t.Fatalf("Marshal WriteBody: %v", err)
	}
	head, got, tail := askWrite(t, conn, 1, proto.KindWrite, body, []byte("data"))
	checkWriteShape(t, 1, head, got, tail, "the disk is full")
}

// TestServerWriteUnknownClientKeepsTheAnswerShape pins the shape for a client
// that is not connected. The refusal is an answer, and it is the same three
// parts as any other.
func TestServerWriteUnknownClientKeepsTheAnswerShape(t *testing.T) {
	_, addr := start(t)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	body, err := json.Marshal(proto.WriteBody{Client: "nobody", Path: "Documents/a.txt", Mode: proto.WriteModePut})
	if err != nil {
		t.Fatalf("Marshal WriteBody: %v", err)
	}
	head, got, tail := askWrite(t, conn, 1, proto.KindWrite, body, []byte("data"))
	checkWriteShape(t, 1, head, got, tail, "not connected")
}

// TestServerWriteRejectedRequestKeepsTheAnswerShape pins the shape for requests
// the server refuses before forwarding anything.
//
// The bodies are well-formed JSON, because an envelope's body has to be: the
// rejection under test is the server's, not the encoder's.
func TestServerWriteRejectedRequestKeepsTheAnswerShape(t *testing.T) {
	_, addr := start(t)

	for _, tc := range []struct {
		name string
		id   uint64
		kind string
		body string
		want string
	}{
		{name: "put without a client", id: 11, kind: proto.KindWrite,
			body: `{"client":"","path":"x","mode":"put"}`, want: "client is required"},
		{name: "put without a path", id: 12, kind: proto.KindWrite,
			body: `{"client":"win","path":"","mode":"put"}`, want: "path is required"},
		{name: "mkdir without a client", id: 13, kind: proto.KindMkdir,
			body: `{"client":"","path":"x"}`, want: "client is required"},
		{name: "remove without a client", id: 14, kind: proto.KindRemove,
			body: `{"client":"","path":"x"}`, want: "client is required"},
		{name: "rename without a client", id: 15, kind: proto.KindRename,
			body: `{"client":"","oldPath":"a","newPath":"b"}`, want: "client is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewer, _ := hello(t, addr, "me", "viewer", false)
			defer viewer.Close()
			conn := proto.NewConn(viewer)

			var content []byte
			if tc.kind == proto.KindWrite {
				// A put's content is already on its way when the announcement is
				// refused, so the server reads it off rather than leaving it to
				// be mistaken for the next request.
				content = []byte("some content")
			}
			head, got, tail := askWrite(t, conn, tc.id, tc.kind, []byte(tc.body), content)
			checkWriteShape(t, tc.id, head, got, tail, tc.want)
		})
	}
}

// TestServerRenameAcrossSharesIsRefused pins that a rename whose two ends are in
// different shares is refused before it is forwarded, since a share is a
// directory and no two of them are the same directory.
func TestServerRenameAcrossSharesIsRefused(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	seen := make(chan string, 4)
	writeOwner(t, proto.NewConn(owner), "", seen)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	body, err := json.Marshal(proto.RenameBody{
		Client:  "win",
		OldPath: "Documents/a.txt",
		NewPath: "Photos/a.txt",
	})
	if err != nil {
		t.Fatalf("Marshal RenameBody: %v", err)
	}
	head, got, tail := askWrite(t, conn, 1, proto.KindRename, body, nil)
	checkWriteShape(t, 1, head, got, tail, "span two clients")

	select {
	case line := <-seen:
		t.Fatalf("the owner was asked %q, want the refusal to happen before forwarding", line)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestServerRenameWithinOneShareIsForwarded pins the other half: the refusal is
// about the two ends, not about renames.
func TestServerRenameWithinOneShareIsForwarded(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	seen := make(chan string, 4)
	writeOwner(t, proto.NewConn(owner), "", seen)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	body, err := json.Marshal(proto.RenameBody{
		Client:  "win",
		OldPath: "Documents/a.txt",
		NewPath: "Documents/b.txt",
	})
	if err != nil {
		t.Fatalf("Marshal RenameBody: %v", err)
	}
	head, got, tail := askWrite(t, conn, 1, proto.KindRename, body, nil)
	checkWriteShape(t, 1, head, got, tail, "")

	select {
	case line := <-seen:
		if !strings.HasPrefix(line, proto.KindRename+" ") {
			t.Fatalf("the owner was asked %q, want a %s request", line, proto.KindRename)
		}
		if !strings.Contains(line, "Documents/a.txt") || !strings.Contains(line, "Documents/b.txt") {
			t.Fatalf("the owner was asked %q, want both ends", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was never asked to rename")
	}
}

// TestServerMkdirAndRemoveAreForwarded pins that the two named write requests
// take the same path and get the same answer shape as a put.
func TestServerMkdirAndRemoveAreForwarded(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	seen := make(chan string, 4)
	writeOwner(t, proto.NewConn(owner), "", seen)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	cases := []struct {
		id   uint64
		kind string
		body any
	}{
		{1, proto.KindMkdir, proto.MkdirBody{Client: "win", Path: "Documents/newdir"}},
		{2, proto.KindRemove, proto.RemoveBody{Client: "win", Path: "Documents/a.txt"}},
	}
	for _, tc := range cases {
		raw, err := json.Marshal(tc.body)
		if err != nil {
			t.Fatalf("Marshal %s: %v", tc.kind, err)
		}
		head, got, tail := askWrite(t, conn, tc.id, tc.kind, raw, nil)
		checkWriteShape(t, tc.id, head, got, tail, "")
	}

	for i := 0; i < len(cases); i++ {
		select {
		case line := <-seen:
			if !strings.Contains(line, cases[i].kind) {
				t.Fatalf("the owner was asked %q, want a %s request", line, cases[i].kind)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the owner was never asked to %s", cases[i].kind)
		}
	}
}

// TestServerWritesOnOneConnectionStayInStep pins the property the three-part
// shape exists to protect: after a success, a failure and a refusal, a read on
// the same connection still gets its own answer rather than a frame left over
// from an earlier one.
func TestServerWritesOnOneConnectionStayInStep(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()

	// The owner answers writes and reads together, so both can happen on the one
	// connection the test drives.
	go func() {
		conn := proto.NewConn(owner)
		for {
			env, err := conn.Recv()
			if err != nil {
				return
			}
			// Every write request carries a stream, whatever its kind, and it
			// has to be read off before the next control frame means anything.
			switch env.Kind {
			case proto.KindWrite, proto.KindMkdir, proto.KindRemove, proto.KindRename:
				for {
					_, done, err := conn.RecvData(env.ID)
					if err != nil || done {
						break
					}
				}
			}
			switch env.Kind {
			case proto.KindWrite, proto.KindMkdir, proto.KindRemove, proto.KindRename:
				head := proto.Envelope{ID: env.ID, Kind: proto.KindWriteOK}
				if err := conn.Send(head); err != nil {
					return
				}
				if err := conn.SendData(env.ID, nil); err != nil {
					return
				}
				if err := conn.Send(head); err != nil {
					return
				}
			case proto.KindRead:
				head := proto.Envelope{ID: env.ID, Kind: proto.KindReadOK}
				if err := conn.Send(head); err != nil {
					return
				}
				if err := conn.SendBody(env.ID, []byte("read back")); err != nil {
					return
				}
				if err := conn.Send(head); err != nil {
					return
				}
			}
		}
	}()

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	// A successful write, then one the server refuses outright, then a read.
	putBody, err := json.Marshal(proto.WriteBody{Client: "win", Path: "Documents/a.txt", Mode: proto.WriteModePut})
	if err != nil {
		t.Fatalf("Marshal WriteBody: %v", err)
	}
	head, got, tail := askWrite(t, conn, 1, proto.KindWrite, putBody, []byte("first"))
	checkWriteShape(t, 1, head, got, tail, "")

	head, got, tail = askWrite(t, conn, 2, proto.KindWrite, []byte(`{"client":"","path":"x","mode":"put"}`), []byte("refused content"))
	checkWriteShape(t, 2, head, got, tail, "client is required")

	head, got, tail = askRead(t, conn, 3, "win", "Documents/a.txt")
	checkReadShape(t, 3, head, tail, "")
	if string(got) != "read back" {
		t.Fatalf("read after writes = %q, want %q: the connection is out of step", got, "read back")
	}
}
