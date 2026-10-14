package proto_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
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

// deadline bounds both ends of a pipe used by these tests. An unbounded pipe
// read hangs until the go test timeout when a regression stops the writer
// without closing it, which is how this file previously produced a ten-minute
// panic instead of a failure.
func deadline(t *testing.T, conns ...net.Conn) {
	t.Helper()
	for _, c := range conns {
		if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			t.Fatalf("SetDeadline: %v", err)
		}
	}
}

func TestConnSendRecv(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	deadline(t, a, b)
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
	deadline(t, a, b)
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

// A zero-byte file is its own stream: a single empty data frame, with no
// content frame before it. Review Focus 5 requires writing empty content to
// work, and this is the shape that case takes at the protocol layer.
func TestConnDataStreamEmptyFileIsOneEmptyFrame(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	deadline(t, a, b)
	ca, cb := proto.NewConn(a), proto.NewConn(b)

	const id = 7
	done := sendAsync(func() error { return ca.SendData(id, nil) })

	got, end, err := cb.RecvData(id)
	if err != nil {
		t.Fatalf("RecvData: %v", err)
	}
	if !end {
		t.Fatal("RecvData end = false, want true: the only frame of a zero-byte file ends the stream")
	}
	if len(got) != 0 {
		t.Fatalf("RecvData payload = %q, want empty", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("SendData: %v", err)
	}

	// Closing the sender makes "nothing more was written" distinguishable from
	// "another frame is waiting": the reader must see a clean end of stream, not
	// a frame. Without this the check would pass even if a content frame had
	// been queued behind the terminator.
	a.Close()
	if _, _, err := cb.RecvData(id); !errors.Is(err, io.EOF) {
		t.Fatalf("RecvData after the zero-byte file's frame = %v, want io.EOF (the empty file is exactly one frame)", err)
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

// Send must hand each frame to the stream in one Write call, and no two Sends
// may be inside a Write at the same time. That second half is the contract the
// shared write lock exists for.
//
// It cannot be observed on a socket: Go serialises a socket's writes internally
// (net.netFD.writeLock), so removing the lock still produces one orderly Write
// per Send and this shape of bug stays hidden. A writer that counts concurrent
// entry can see it, which is what exclusiveWriter does. Holding the entry for a
// moment is what makes the race deterministic rather than merely likely.
func TestConnSendWritesWholeFrame(t *testing.T) {
	const frames = 4
	const rounds = 5
	for round := 0; round < rounds; round++ {
		rec := &recordingWriter{}
		excl := &exclusiveWriter{w: rec}
		ca := proto.NewConn(duplex{w: excl})

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
				t.Fatalf("round %d: Send %d: %v", round, i, err)
			}
		}

		if overlap := excl.maxOverlap(); overlap > 1 {
			t.Fatalf("round %d: up to %d Send calls were inside the stream's Write at once; Send and SendData must share one write lock", round, overlap)
		}

		// Every Write call must contain exactly one whole frame: no more, no
		// less. One Write per frame is what a contiguous frame means.
		if got := rec.count(); got != frames {
			t.Fatalf("round %d: Send made %d Write calls for %d frames, want one each", round, got, frames)
		}
		seen := make(map[uint64]bool, frames)
		for i, call := range rec.snapshot() {
			kind, payload, err := proto.ReadFrame(bytes.NewReader(call))
			if err != nil {
				t.Fatalf("round %d: Write call %d does not hold exactly one whole frame: %v", round, i, err)
			}
			if kind != proto.FrameControl {
				t.Fatalf("round %d: Write call %d frame kind = %d, want %d", round, i, kind, proto.FrameControl)
			}
			var env proto.Envelope
			if err := json.Unmarshal(payload, &env); err != nil {
				t.Fatalf("round %d: Write call %d payload is not one envelope: %v", round, i, err)
			}
			if seen[env.ID] {
				t.Fatalf("round %d: envelope ID %d was written twice", round, env.ID)
			}
			seen[env.ID] = true
		}
		if len(seen) != frames {
			t.Fatalf("round %d: wrote %d distinct envelopes, want %d", round, len(seen), frames)
		}
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
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteFrame short-write error = %v, want it to wrap io.ErrShortWrite", err)
	}
}

// A failed write poisons the stream: part of a frame may already be on the
// wire, so every later write must fail too. Without that, a caller that ignores
// one error keeps appending frames behind a truncated one and the peer
// desynchronises with nothing reporting it.
func TestConnWriteErrorIsSticky(t *testing.T) {
	fw := &failingWriter{accept: 3}
	ca := proto.NewConn(duplex{w: fw})

	env := proto.Envelope{ID: 1, Kind: proto.KindHello, Body: json.RawMessage(`{}`)}
	first := ca.Send(env)
	if first == nil {
		t.Fatal("Send on a writer that truncates returned nil")
	}
	if !errors.Is(first, io.ErrShortWrite) {
		t.Fatalf("first Send error = %v, want it to wrap io.ErrShortWrite", first)
	}

	// Both entry points must keep reporting the failure, and must not write.
	for i := 0; i < 2; i++ {
		if err := ca.Send(env); err == nil {
			t.Fatalf("Send %d after a failed write returned nil; the broken stream stayed in use", i+2)
		}
	}
	if err := ca.SendData(1, []byte("payload")); err == nil {
		t.Fatal("SendData after a failed write returned nil; the broken stream stayed in use")
	}
	// The earlier payload is long enough to have been offered to the writer
	// more than once if the sticky error were not stopping it.
	if fw.calls != 1 {
		t.Fatalf("writer was called %d times after a failed write, want 1", fw.calls)
	}
}

// failingWriter truncates every write to accept bytes and reports a short write.
type failingWriter struct {
	accept int
	calls  int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if len(p) > w.accept {
		return w.accept, nil
	}
	return len(p), nil
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

// exclusiveWriter records what it was handed and, on every Write, counts how
// many Writes are inside it at the same time. It holds the count for a moment
// so a concurrent writer has a window in which to be seen: the counter is what
// reports the overlap, and it cannot be lost the way an unsynchronised append
// can. It exists because a socket, and a plain in-memory writer, both hide this
// — Go serialises socket writes, and a writer that merely appends one slice per
// call cannot report interleaving at all.
type exclusiveWriter struct {
	w io.Writer

	mu    sync.Mutex
	calls [][]byte

	inFlight    int32
	maxInFlight int32
}

func (w *exclusiveWriter) Write(p []byte) (int, error) {
	now := atomic.AddInt32(&w.inFlight, 1)
	for {
		peak := atomic.LoadInt32(&w.maxInFlight)
		if now <= peak || atomic.CompareAndSwapInt32(&w.maxInFlight, peak, now) {
			break
		}
	}

	w.mu.Lock()
	w.calls = append(w.calls, append([]byte(nil), p...))
	w.mu.Unlock()

	// Hold the entry open long enough for the racing goroutine to overlap.
	time.Sleep(time.Millisecond)
	n, err := w.w.Write(p)

	atomic.AddInt32(&w.inFlight, -1)
	return n, err
}

func (w *exclusiveWriter) maxOverlap() int {
	return int(atomic.LoadInt32(&w.maxInFlight))
}

// recordingWriter keeps a copy of every Write call it is handed. Its state is
// mutex-guarded because two Sends reaching it concurrently is exactly the case
// under test, and an unsynchronised append would corrupt the record instead of
// reporting it.
type recordingWriter struct {
	mu    sync.Mutex
	calls [][]byte
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.calls = append(w.calls, append([]byte(nil), p...))
	w.mu.Unlock()
	return len(p), nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

func (w *recordingWriter) snapshot() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.calls...)
}

// duplex adapts an io.Writer into the io.ReadWriteCloser that NewConn takes.
// Reading is unused by these tests.
type duplex struct{ w io.Writer }

func (d duplex) Write(p []byte) (int, error) { return d.w.Write(p) }
func (d duplex) Read([]byte) (int, error)    { return 0, io.EOF }
func (d duplex) Close() error                { return nil }
