package proto_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/proto"
)

func TestFrameControlRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := []byte("hello, peerfold")

	if err := proto.WriteFrame(&buf, proto.FrameControl, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	kind, got, err := proto.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != proto.FrameControl {
		t.Fatalf("ReadFrame kind = %d, want %d", kind, proto.FrameControl)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ReadFrame payload = %q, want %q", got, want)
	}
}

// A zero-length data frame is how a response stream is terminated, and a
// zero-byte file is one such frame, so the empty payload must survive a
// round trip rather than being confused with "no frame".
func TestFrameDataRoundTripEmptyPayload(t *testing.T) {
	var buf bytes.Buffer

	if err := proto.WriteFrame(&buf, proto.FrameData, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	kind, got, err := proto.ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if kind != proto.FrameData {
		t.Fatalf("ReadFrame kind = %d, want %d", kind, proto.FrameData)
	}
	if len(got) != 0 {
		t.Fatalf("ReadFrame payload = %q, want empty", got)
	}
}

func TestFrameTruncatedHeader(t *testing.T) {
	// Two bytes only: the kind byte plus one of the four length bytes.
	_, _, err := proto.ReadFrame(bytes.NewReader([]byte{byte(proto.FrameControl), 0x00}))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame error = %v, want io.ErrUnexpectedEOF", err)
	}
}

// A clean end of stream at a frame boundary must be distinguishable from a
// truncated frame: callers use io.EOF to mean "the peer closed".
func TestFrameCleanEOFAtBoundary(t *testing.T) {
	_, _, err := proto.ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame error = %v, want io.EOF", err)
	}
}

func TestFrameRejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(byte(proto.FrameControl))
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], proto.MaxPayload+1)
	buf.Write(hdr[:])

	_, _, err := proto.ReadFrame(&buf)
	if err == nil {
		t.Fatal("ReadFrame accepted a payload larger than MaxPayload, want an error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ReadFrame error = %v, want it to contain %q", err, "too large")
	}
}

func TestWriteFrameRejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	err := proto.WriteFrame(&buf, proto.FrameData, make([]byte, proto.MaxPayload+1))
	if err == nil {
		t.Fatal("WriteFrame accepted a payload larger than MaxPayload, want an error")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("WriteFrame error = %v, want it to contain %q", err, "too large")
	}
}
