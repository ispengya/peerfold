package server_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/proto"
)

// A read answer is three parts in every direction, and these tests pin that
// shape at the server rather than through a viewer. The end-to-end tests prove
// the feature works; they cannot say which of the three parts a version that
// broke it actually dropped, and the shape is the whole contract: frames are one
// sequence, so a part left unsent does not stay put -- it becomes the next
// answer, and the failure then appears somewhere else entirely.

// serveReads keeps answering read requests on an owner connection until it is
// closed. Each answer is read_ok, the kind that exists so an answer cannot be
// mistaken for a request: neither carries a body, so classification by the
// presence of one cannot work.
func serveReads(t *testing.T, conn *proto.Conn, content []byte, fail string) {
	t.Helper()
	go func() {
		for {
			env, err := conn.Recv()
			if err != nil {
				return
			}
			if env.Kind != proto.KindRead {
				return
			}
			head := proto.Envelope{ID: env.ID, Kind: proto.KindReadOK, Err: fail}
			if err := conn.Send(head); err != nil {
				return
			}
			if env.Err == "" && fail == "" && len(content) > 0 {
				if err := conn.SendBody(env.ID, content); err != nil {
					return
				}
			} else if err := conn.SendData(env.ID, nil); err != nil {
				return
			}
			if err := conn.Send(head); err != nil {
				return
			}
		}
	}()
}

// askRead sends one read and returns the three parts the server answered with.
//
// The end marker is what ends the content, so the reader stops there: it is the
// only frame in the stream that means "no more content", and the frame after it
// is the closing control frame.
func askRead(t *testing.T, conn *proto.Conn, id uint64, client, path string) (proto.Envelope, []byte, proto.Envelope) {
	t.Helper()
	body, err := json.Marshal(proto.ReadBody{Client: client, Path: path, Off: 0, N: -1})
	if err != nil {
		t.Fatalf("Marshal ReadBody: %v", err)
	}
	if err := conn.Send(proto.Envelope{ID: id, Kind: proto.KindRead, Body: body}); err != nil {
		t.Fatalf("Send read %d: %v", id, err)
	}
	head, err := conn.Recv()
	if err != nil {
		t.Fatalf("read %d: head: %v", id, err)
	}
	var got []byte
	for {
		chunk, done, err := conn.RecvData(id)
		if err != nil {
			t.Fatalf("read %d: content: %v", id, err)
		}
		if done {
			break
		}
		got = append(got, chunk...)
	}
	tail, err := conn.Recv()
	if err != nil {
		t.Fatalf("read %d: closing frame: %v", id, err)
	}
	return head, got, tail
}

// checkReadShape asserts the three parts are all present, all stamped with the
// requester's own ID, and all of the kind the requester is waiting on.
//
// Every part is checked on every path. A version that dropped the closing frame
// on the success path or the content on the failure path would still return the
// right bytes, and would still look correct until a second read arrived.
func checkReadShape(t *testing.T, id uint64, head proto.Envelope, tail proto.Envelope, wantErr string) {
	t.Helper()
	if head.Kind != proto.KindReadOK {
		t.Fatalf("head kind = %q, want %q", head.Kind, proto.KindReadOK)
	}
	if head.ID != id {
		t.Fatalf("head id = %d, want %d", head.ID, id)
	}
	if tail.Kind != proto.KindReadOK {
		t.Fatalf("closing frame kind = %q, want %q", tail.Kind, proto.KindReadOK)
	}
	if tail.ID != id {
		t.Fatalf("closing frame id = %d, want %d", tail.ID, id)
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

// TestServerReadAnswerIsThreeParts pins the whole shape for a successful read:
// a head frame, the content, and a closing frame.
func TestServerReadAnswerIsThreeParts(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	serveReads(t, proto.NewConn(owner), []byte("hello world"), "")

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	head, got, tail := askRead(t, conn, 1, "win", "Documents/a.txt")
	checkReadShape(t, 1, head, tail, "")
	if string(got) != "hello world" {
		t.Fatalf("content = %q, want %q", got, "hello world")
	}
}

// TestServerReadEmptyFileIsAnEmptyStream pins that a zero-byte file is an answer
// with no error and no content. An empty stream is the whole of it: a marker
// frame inside the stream cannot exist, because a zero-length frame is what ends
// the stream.
func TestServerReadEmptyFileIsAnEmptyStream(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	serveReads(t, proto.NewConn(owner), nil, "")

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	head, got, tail := askRead(t, conn, 1, "win", "Documents/empty.txt")
	checkReadShape(t, 1, head, tail, "")
	if len(got) != 0 {
		t.Fatalf("content = %q, want nothing", got)
	}
}

// TestServerReadFailureKeepsTheAnswerShape is the case that used to hang: a read
// that fails must answer with all three parts too, because the next request is
// read from wherever this answer stopped.
//
// The failure is reported in the head frame and nowhere else. The stream is
// empty, exactly as it is for a zero-byte file, so a caller that ignored the
// head could not tell the two apart -- which is why the head is the only place
// the reason belongs.
func TestServerReadFailureKeepsTheAnswerShape(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	serveReads(t, proto.NewConn(owner), nil, "no such file")

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	head, got, tail := askRead(t, conn, 1, "win", "Documents/nope.txt")
	checkReadShape(t, 1, head, tail, "no such file")
	if len(got) != 0 {
		t.Fatalf("content = %q, want nothing: the reason is in the head frame", got)
	}
}

// TestServerReadUnknownClientKeepsTheAnswerShape pins the offline case, where
// the server answers on its own because there is nobody to forward to. The
// answer still has all three parts.
func TestServerReadUnknownClientKeepsTheAnswerShape(t *testing.T) {
	_, addr := start(t)

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	head, got, tail := askRead(t, conn, 2, "nobody", "x")
	checkReadShape(t, 2, head, tail, "nobody")
	if len(got) != 0 {
		t.Fatalf("content = %q, want nothing", got)
	}
}

// TestServerAnswersReadsInOrderOnOneConnection pins that each answer is sent
// whole before the next begins.
//
// A version that skipped the closing frame on any path would leave this
// connection one frame out of step, and the second read would be assembled from
// the end of the first answer. One connection and two reads is the smallest
// arrangement where that shows: with a fresh connection per read, the leftover
// frame is simply discarded with the connection.
func TestServerAnswersReadsInOrderOnOneConnection(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	serveReads(t, proto.NewConn(owner), []byte("payload"), "")

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	conn := proto.NewConn(viewer)

	for id := uint64(1); id <= 3; id++ {
		head, got, tail := askRead(t, conn, id, "win", "Documents/a.txt")
		checkReadShape(t, id, head, tail, "")
		if string(got) != "payload" {
			t.Fatalf("read %d content = %q, want %q", id, got, "payload")
		}
	}
}

// TestServerReadOfAnEmptyFileThenAFullOne pins the two answers apart on one
// connection. They differ only in the head frame's error and in whether the
// stream carried anything, and both are still three parts.
func TestServerReadOfAnEmptyFileThenAFullOne(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()

	// One owner, two reads, one of which has content. The owner decides by path.
	conn := proto.NewConn(owner)
	go func() {
		for {
			env, err := conn.Recv()
			if err != nil {
				return
			}
			if env.Kind != proto.KindRead {
				return
			}
			var b proto.ReadBody
			if err := json.Unmarshal(env.Body, &b); err != nil {
				return
			}
			head := proto.Envelope{ID: env.ID, Kind: proto.KindReadOK}
			if err := conn.Send(head); err != nil {
				return
			}
			if strings.HasSuffix(b.Path, "empty.txt") {
				if err := conn.SendData(env.ID, nil); err != nil {
					return
				}
			} else if err := conn.SendBody(env.ID, []byte("full")); err != nil {
				return
			}
			if err := conn.Send(head); err != nil {
				return
			}
		}
	}()

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	vc := proto.NewConn(viewer)

	head, got, tail := askRead(t, vc, 1, "win", "Documents/empty.txt")
	checkReadShape(t, 1, head, tail, "")
	if len(got) != 0 {
		t.Fatalf("empty read content = %q, want nothing", got)
	}

	head, got, tail = askRead(t, vc, 2, "win", "Documents/full.txt")
	checkReadShape(t, 2, head, tail, "")
	if string(got) != "full" {
		t.Fatalf("full read content = %q, want %q", got, "full")
	}
}

// TestServerReadRejectedRequestKeepsTheAnswerShape pins the shape for a request
// the server rejects before forwarding anything. A refusal is an answer too, and
// it has the same three parts.
//
// The bodies here are well-formed JSON, because an envelope's body has to be:
// the rejection under test is the server's, not the encoder's.
func TestServerReadRejectedRequestKeepsTheAnswerShape(t *testing.T) {
	_, addr := start(t)

	for _, tc := range []struct {
		name string
		id   uint64
		body string
		want string
	}{
		{name: "no client", id: 7, body: `{"client":"","path":"x"}`, want: "client is required"},
		{name: "negative offset", id: 8, body: `{"client":"win","path":"x","off":-1}`, want: "offset must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewer, _ := hello(t, addr, "me", "viewer", false)
			defer viewer.Close()
			conn := proto.NewConn(viewer)

			if err := conn.Send(proto.Envelope{ID: tc.id, Kind: proto.KindRead, Body: []byte(tc.body)}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			head, err := conn.Recv()
			if err != nil {
				t.Fatalf("head: %v", err)
			}
			if _, _, err := conn.RecvData(tc.id); err != nil {
				t.Fatalf("content: %v", err)
			}
			tail, err := conn.Recv()
			if err != nil {
				t.Fatalf("closing frame: %v", err)
			}
			checkReadShape(t, tc.id, head, tail, tc.want)
		})
	}
}

// TestServerReadOwnerLeavingMidAnswerIsNotSilent pins that an owner that
// disappears while its answer is being delivered produces an answer rather than
// a hang.
func TestServerReadOwnerLeavingMidAnswerIsNotSilent(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)

	conn := proto.NewConn(owner)
	go func() {
		env, err := conn.Recv()
		if err != nil || env.Kind != proto.KindRead {
			return
		}
		head := proto.Envelope{ID: env.ID, Kind: proto.KindReadOK}
		_ = conn.Send(head)
		_ = conn.SendData(env.ID, nil)
		// The closing frame is never sent: the owner leaves here.
	}()

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	vc := proto.NewConn(viewer)

	body, err := json.Marshal(proto.ReadBody{Client: "win", Path: "x", Off: 0, N: -1})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := vc.Send(proto.Envelope{ID: 1, Kind: proto.KindRead, Body: body}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The owner goes away without closing its answer. The answer that is already
	// in hand is delivered; the connection ending is what the server sees next.
	_ = owner.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = vc.Recv()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the server never answered a read whose owner left mid-answer")
	}
}

// The head must reach the requester before the owner's content does. Buffering
// the whole answer first would still deliver the right bytes, so only a test
// that holds the content back can tell the two designs apart.
func TestReadHeadIsForwardedBeforeTheContent(t *testing.T) {
	_, addr := start(t)
	owner, _ := hello(t, addr, "win", "client", false)
	defer owner.Close()
	oc := proto.NewConn(owner)

	release := make(chan struct{})
	go func() {
		env, err := oc.Recv()
		if err != nil || env.Kind != proto.KindRead {
			return
		}
		head := proto.Envelope{ID: env.ID, Kind: proto.KindReadOK}
		if err := oc.Send(head); err != nil {
			return
		}
		// The head is on the wire; the content deliberately waits until the
		// requester has been observed receiving it.
		<-release
		_ = oc.SendBody(env.ID, []byte("payload"))
		_ = oc.Send(head)
	}()

	viewer, _ := hello(t, addr, "me", "viewer", false)
	defer viewer.Close()
	vc := proto.NewConn(viewer)

	body, err := json.Marshal(proto.ReadBody{Client: "win", Path: "Documents/a.txt", Off: 0, N: -1})
	if err != nil {
		t.Fatalf("Marshal ReadBody: %v", err)
	}
	if err := vc.Send(proto.Envelope{ID: 1, Kind: proto.KindRead, Body: body}); err != nil {
		t.Fatalf("Send read: %v", err)
	}

	_ = viewer.SetReadDeadline(time.Now().Add(2 * time.Second))
	head, err := vc.Recv()
	if err != nil {
		close(release)
		t.Fatalf("the head did not arrive while the content was held: %v", err)
	}
	if head.Err != "" {
		close(release)
		t.Fatalf("head error = %q", head.Err)
	}
	close(release)

	var got []byte
	for {
		chunk, done, err := vc.RecvData(1)
		if err != nil {
			t.Fatalf("content: %v", err)
		}
		if done {
			break
		}
		got = append(got, chunk...)
	}
	if string(got) != "payload" {
		t.Errorf("content = %q, want %q", got, "payload")
	}
	if _, err := vc.Recv(); err != nil {
		t.Fatalf("closing frame: %v", err)
	}
	_ = viewer.SetReadDeadline(time.Time{})
}
