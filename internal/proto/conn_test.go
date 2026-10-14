package proto_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"peerfold/internal/proto"
)

// sendAsync runs fn in a goroutine and returns a channel carrying its error.
// net.Pipe is unbuffered, so a sender blocks until the receiver reads; running
// the send off the test goroutine keeps the test from deadlocking, and the
// channel keeps the error from being swallowed.
func sendAsync(fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- fn() }()
	return ch
}

func TestConnSendRecv(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	body, err := json.Marshal(proto.HelloBody{Name: "laptop", Role: proto.RoleClient, Shares: []string{"docs"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := proto.Envelope{ID: 7, Kind: proto.KindHello, Body: body}

	done := sendAsync(func() error { return ca.Send(want) })

	got, err := cb.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.ID != want.ID {
		t.Fatalf("Recv ID = %d, want %d", got.ID, want.ID)
	}
	if got.Kind != want.Kind {
		t.Fatalf("Recv Kind = %q, want %q", got.Kind, want.Kind)
	}
	if !bytes.Equal(got.Body, want.Body) {
		t.Fatalf("Recv Body = %s, want %s", got.Body, want.Body)
	}
}

func TestConnDataStream(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	chunks := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	const id = 42

	done := sendAsync(func() error {
		for _, chunk := range chunks {
			if err := ca.SendData(id, chunk); err != nil {
				return err
			}
		}
		// A zero-length data frame ends the stream; a zero-byte file is
		// exactly one of these.
		return ca.SendData(id, nil)
	})

	for i, want := range chunks {
		got, end, err := cb.RecvData(id)
		if err != nil {
			t.Fatalf("RecvData chunk %d: %v", i, err)
		}
		if end {
			t.Fatalf("RecvData chunk %d: end = true, want false", i)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("RecvData chunk %d = %q, want %q", i, got, want)
		}
	}

	got, end, err := cb.RecvData(id)
	if err != nil {
		t.Fatalf("RecvData terminator: %v", err)
	}
	if !end {
		t.Fatal("RecvData terminator: end = false, want true")
	}
	if len(got) != 0 {
		t.Fatalf("RecvData terminator payload = %q, want empty", got)
	}

	if err := <-done; err != nil {
		t.Fatalf("SendData: %v", err)
	}
}

// Concurrent Send calls must not desynchronise the stream: every envelope has
// to arrive whole, exactly once.
//
// This runs on a real TCP loopback rather than net.Pipe for two reasons: a pipe
// Write blocks until the peer reads it, which is what previously wedged this
// test into the ten-minute go test timeout, and a pipe delivers each Write
// atomically so it cannot show stream corruption at all.
//
// Bodies must be well-formed JSON, because Envelope.Body is a json.RawMessage
// and encoding it validates and compacts the bytes: a body that is not JSON
// fails in Send's marshal, before a single byte reaches the wire.
//
// Known limit, verified rather than assumed: this test does NOT prove that Send
// and SendData share a write lock, because Go serialises a socket's writes
// internally (net.netFD.writeLock). Removing the lock and re-running this test
// still passes — see task-2-report.md. The lock is therefore justified by
// reasoning about the contract, not by this test.
func TestConnConcurrentSend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	type accepted struct {
		c   net.Conn
		err error
	}
	acc := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		acc <- accepted{c, err}
	}()

	sender, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer sender.Close()

	res := <-acc
	if res.err != nil {
		t.Fatalf("Accept: %v", res.err)
	}
	receiver := res.c
	defer receiver.Close()

	ca, cb := proto.NewConn(sender), proto.NewConn(receiver)

	const senders = 8
	const bodyLen = 256 << 10
	pad := make([]byte, bodyLen)
	for j := range pad {
		pad[j] = byte('a' + j%26)
	}
	bodies := make([][]byte, senders)
	for i := range bodies {
		body := append([]byte(fmt.Sprintf(`{"n":%d,"pad":"`, i)), pad...)
		bodies[i] = append(body, '"', '}')
	}

	// Deadlines turn "the senders wedged on a full socket" into a reported
	// failure instead of a hang that eats the go test timeout. The payloads
	// total ~2 MiB, past any loopback buffer.
	if err := sender.SetWriteDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if err := receiver.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, senders)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ca.Send(proto.Envelope{ID: uint64(i), Kind: proto.KindHello, Body: bodies[i]})
		}(i)
	}

	// Each sender sends exactly one envelope, so this loop consumes all of
	// them. There is deliberately no early break: the reader keeps taking bytes
	// until every sender is off the socket.
	got := make([]proto.Envelope, 0, senders)
	for i := 0; i < senders; i++ {
		env, err := cb.Recv()
		if err != nil {
			t.Fatalf("Recv %d of %d: %v", i+1, senders, err)
		}
		got = append(got, env)
	}

	// Clear the deadlines so the wait below is not racing them.
	receiver.SetReadDeadline(time.Time{})
	sender.SetWriteDeadline(time.Time{})
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	// Every body must arrive byte-identical, exactly once.
	seen := make(map[uint64]bool, senders)
	for _, env := range got {
		if seen[env.ID] {
			t.Fatalf("Recv envelope ID %d twice", env.ID)
		}
		seen[env.ID] = true
		if int(env.ID) >= senders {
			t.Fatalf("Recv unexpected envelope ID %d", env.ID)
		}
		if !bytes.Equal(env.Body, bodies[env.ID]) {
			t.Fatalf("Recv body for ID %d is %d bytes, want %d and identical", env.ID, len(env.Body), len(bodies[env.ID]))
		}
	}
	if len(seen) != senders {
		t.Fatalf("Recv %d distinct envelope IDs, want %d", len(seen), senders)
	}
}

// Send and SendData hand their frame to the stream in a single Write call. That
// is the observable half of the shared-write-lock contract: when the writer
// takes the whole frame at once, a reader can never see another frame's bytes
// in the middle of this one.
//
// Note what this does not do: it does not prove the lock is taken, and it does
// not prove a missing lock would corrupt anything, because Go serialises a
// socket's writes internally (net.netFD.writeLock). Establishing the lock's
// necessity needs the race detector, which this machine cannot build — see
// task-2-report.md.
func TestConnSendWritesWholeFrame(t *testing.T) {
	rec := &recordingWriter{}
	ca := proto.NewConn(duplex{w: rec})

	const frames = 4
	body := json.RawMessage(fmt.Sprintf(`{"pad":%q}`, string(bytes.Repeat([]byte("a"), 1<<10))))

	var wg sync.WaitGroup
	errs := make([]error, frames)
	for i := 0; i < frames; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ca.Send(proto.Envelope{ID: uint64(i), Kind: proto.KindHello, Body: body})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	// Every Write call must contain exactly one whole frame: no more, no less.
	if len(rec.calls) != frames {
		t.Fatalf("Send made %d Write calls for %d frames, want one each", len(rec.calls), frames)
	}
	seen := make(map[uint64]bool, frames)
	for i, call := range rec.calls {
		kind, payload, err := proto.ReadFrame(bytes.NewReader(call))
		if err != nil {
			t.Fatalf("Write call %d does not hold exactly one whole frame: %v", i, err)
		}
		if kind != proto.FrameControl {
			t.Fatalf("Write call %d frame kind = %d, want %d", i, kind, proto.FrameControl)
		}
		var env proto.Envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("Write call %d payload is not one envelope: %v", i, err)
		}
		if seen[env.ID] {
			t.Fatalf("envelope ID %d was written twice", env.ID)
		}
		seen[env.ID] = true
	}
	if len(seen) != frames {
		t.Fatalf("wrote %d distinct envelopes, want %d", len(seen), frames)
	}
}

// A writer that stops short must not corrupt the stream. Write's contract lets
// it accept fewer bytes than it was offered, and net.Conn can return a short
// write; if that byte count is discarded, the rest of the frame is dropped
// silently and the peer resynchronises on garbage.
//
// This is an independent robustness defect found while reworking the
// concurrency test.
func TestWriteFrameReportsShortWrite(t *testing.T) {
	sw := &shortWriter{accept: 5}
	err := proto.WriteFrame(sw, proto.FrameControl, []byte("0123456789"))
	if err == nil {
		t.Fatal("WriteFrame accepted a short write without reporting it; the frame's tail is silently lost")
	}
	if !strings.Contains(err.Error(), "short write") {
		t.Fatalf("WriteFrame short-write error = %v, want it to mention a short write", err)
	}
}

// shortWriter accepts at most accept bytes from any Write and reports exactly
// how many it took, like a stream that filled up mid-frame.
type shortWriter struct{ accept int }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.accept {
		return w.accept, nil
	}
	return len(p), nil
}

// recordingWriter keeps a copy of every Write call it is handed.
type recordingWriter struct{ calls [][]byte }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.calls = append(w.calls, append([]byte(nil), p...))
	return len(p), nil
}

// duplex adapts an io.Writer into the io.ReadWriteCloser that NewConn takes.
// Reading is unused by these tests.
type duplex struct{ w io.Writer }

func (d duplex) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d duplex) Read([]byte) (int, error)    { return 0, io.EOF }
func (d duplex) Close() error                { return nil }
