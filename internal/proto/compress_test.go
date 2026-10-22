package proto_test

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"

	"github.com/ispengya/peerfold/internal/proto"
)

// countingConn counts the bytes one side writes, so a test can assert what
// actually crossed the wire rather than what the application asked to send.
type countingConn struct {
	net.Conn
	written atomic.Int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

// sendAndRecv writes one chunk on out and reads it on in, concurrently because
// net.Pipe is synchronous.
func sendAndRecv(t *testing.T, out, in *proto.Conn, body []byte) []byte {
	t.Helper()
	errs := make(chan error, 1)
	go func() {
		if err := out.SendData(1, body); err != nil {
			errs <- err
			return
		}
		errs <- out.SendData(1, nil)
	}()
	got, done, err := in.RecvData(1)
	if err != nil {
		t.Fatalf("RecvData: %v", err)
	}
	if done {
		t.Fatal("RecvData ended before the chunk")
	}
	if _, done, err := in.RecvData(1); err != nil || !done {
		t.Fatalf("RecvData end marker: done=%v err=%v", done, err)
	}
	if err := <-errs; err != nil {
		t.Fatalf("SendData: %v", err)
	}
	return got
}

func TestCompressedDataRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"compressible", bytes.Repeat([]byte("the quick brown fox\n"), 16<<10)},
		{"incompressible", func() []byte {
			b := make([]byte, 256<<10)
			if _, err := rand.Read(b); err != nil {
				t.Fatal(err)
			}
			return b
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			a, b := proto.NewConn(left), proto.NewConn(right)
			a.SetCompression(true)
			b.SetCompression(true)

			if got := sendAndRecv(t, a, b, tc.body); !bytes.Equal(got, tc.body) {
				t.Fatalf("round trip changed %d bytes", len(tc.body))
			}
		})
	}
}

// Compressible content must actually take fewer bytes on the wire; content that
// compression cannot help must stay at its original size instead of paying for
// an expanded frame.
func TestCompressionOnlyCostsBytesWhenItSavesThem(t *testing.T) {
	compressible := bytes.Repeat([]byte("function render() { return 1 }\n"), 8<<10)
	random := make([]byte, 256<<10)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}

	measure := func(body []byte) int64 {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		counted := &countingConn{Conn: left}
		a, b := proto.NewConn(counted), proto.NewConn(right)
		a.SetCompression(true)
		b.SetCompression(true)
		if got := sendAndRecv(t, a, b, body); !bytes.Equal(got, body) {
			t.Fatal("round trip changed the bytes")
		}
		return counted.written.Load()
	}

	if got := measure(compressible); got >= int64(len(compressible))/2 {
		t.Errorf("compressible body crossed the wire as %d bytes, want well under %d", got, len(compressible)/2)
	}
	// A raw frame is 5 bytes of header plus an 8-byte stream id, and the end
	// marker is one more empty frame.
	rawBound := int64(len(random) + 13 + 13)
	if got := measure(random); got > rawBound {
		t.Errorf("incompressible body crossed the wire as %d bytes, want at most the raw frame size %d", got, rawBound)
	}
}

// A compressed frame is bounded on expansion. A frame that would inflate past
// one protocol chunk is refused rather than allocated.
func TestACompressedFrameCannotExpandPastOneChunk(t *testing.T) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, proto.MaxChunk+1)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 8+len(buf.Bytes()))
	binary.BigEndian.PutUint64(payload[:8], 9)
	copy(payload[8:], buf.Bytes())

	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := proto.NewConn(left)
	go func() {
		_ = proto.WriteFrame(right, proto.FrameCompressed, payload)
	}()

	if _, _, err := conn.RecvData(9); err == nil {
		t.Fatal("a frame expanding past MaxChunk was accepted")
	}
}
