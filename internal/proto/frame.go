// Package proto carries peerfold's wire protocol: length-prefixed frames over
// a single TCP connection, with JSON envelopes for control messages and raw
// chunked frames for file content.
package proto

import (
	"encoding/binary"
	"fmt"
	"io"
)

// FrameKind distinguishes a control frame (one JSON Envelope) from a data
// frame (one chunk of a response body).
type FrameKind uint8

const (
	FrameControl FrameKind = 1
	FrameData    FrameKind = 2
	// FrameCompressed is a data frame whose content (after the stream id) has
	// been compressed. It is only sent on a connection where both ends agreed
	// to compression in the handshake; FrameData remains valid on every
	// connection and is used for content compression does not help.
	FrameCompressed FrameKind = 3
)

// MaxPayload bounds one frame's payload. A response body larger than this is
// streamed as several data frames rather than rejected, so this is a chunk
// size as much as a limit.
const MaxPayload = 1 << 20

// frameHeaderLen is one kind byte plus four big-endian payload-length bytes.
const frameHeaderLen = 5

// MaxChunk is the largest content a data frame can carry. A data frame's
// payload is an 8-byte stream id followed by the content, so content is capped
// this much below MaxPayload; a caller streaming a file should slice on this
// rather than on MaxPayload.
const MaxChunk = MaxPayload - dataHeaderLen

// WriteFrame writes one frame: a kind byte, the payload length as a big-endian
// uint32, then the payload bytes.
//
// A writer may legitimately accept fewer bytes than it was offered, and a
// net.Conn can return a short write when its buffer fills. That count is
// therefore checked: discarding it would drop the tail of the frame and leave
// the peer resynchronising on whatever bytes followed, which is the silent data
// loss this protocol must not do.
func WriteFrame(w io.Writer, kind FrameKind, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("proto: frame payload %d bytes too large (max %d)", len(payload), MaxPayload)
	}
	frame := make([]byte, frameHeaderLen, frameHeaderLen+len(payload))
	frame[0] = byte(kind)
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	frame = append(frame, payload...)
	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return fmt.Errorf("proto: short write of frame: wrote %d of %d bytes: %w", n, len(frame), io.ErrShortWrite)
	}
	return nil
}

// ReadFrame reads one frame. A clean end of stream at a frame boundary reports
// io.EOF; a stream cut part-way through a frame reports io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (FrameKind, []byte, error) {
	var header [frameHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("proto: frame payload %d bytes too large (max %d)", n, MaxPayload)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return FrameKind(header[0]), payload, nil
}
