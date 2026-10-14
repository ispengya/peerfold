package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// dataHeaderLen is the big-endian stream id that prefixes every data frame's
// payload.
const dataHeaderLen = 8

// Conn frames a byte stream into peerfold control envelopes and data chunks.
//
// One read lock and one write lock are held. The read side is safe for a
// single reader; the write side serialises Send and SendData, which is what
// keeps two concurrent sends from interleaving their frames.
type Conn struct {
	rw io.ReadWriteCloser

	rmu sync.Mutex
	wmu sync.Mutex
}

// NewConn wraps rw. The Conn owns rw: Close closes it.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw}
}

// Send writes env as a control frame.
func (c *Conn) Send(env Envelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("proto: encode control envelope: %w", err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.rw, FrameControl, payload)
}

// Recv reads the next control frame. A data frame in that position is a
// protocol violation rather than something to skip past.
func (c *Conn) Recv() (Envelope, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	kind, payload, err := ReadFrame(c.rw)
	if err != nil {
		return Envelope{}, err
	}
	if kind != FrameControl {
		return Envelope{}, fmt.Errorf("proto: expected a control frame, got frame kind %d", kind)
	}
	var env Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return Envelope{}, fmt.Errorf("proto: decode control envelope: %w", err)
	}
	return env, nil
}

// SendData writes one chunk of the response stream identified by id. A
// zero-length chunk ends the stream, and a zero-byte file is exactly one such
// frame.
func (c *Conn) SendData(id uint64, chunk []byte) error {
	payload := make([]byte, dataHeaderLen+len(chunk))
	binary.BigEndian.PutUint64(payload[:dataHeaderLen], id)
	copy(payload[dataHeaderLen:], chunk)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.rw, FrameData, payload)
}

// RecvData reads the next chunk of the response stream identified by id. The
// second result reports that the stream ended, which the peer signals with a
// zero-length chunk.
func (c *Conn) RecvData(id uint64) ([]byte, bool, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	kind, payload, err := ReadFrame(c.rw)
	if err != nil {
		return nil, false, err
	}
	if kind != FrameData {
		return nil, false, fmt.Errorf("proto: expected a data frame, got frame kind %d", kind)
	}
	if len(payload) < dataHeaderLen {
		return nil, false, fmt.Errorf("proto: data frame carries %d bytes, want at least %d for the stream id", len(payload), dataHeaderLen)
	}
	if got := binary.BigEndian.Uint64(payload[:dataHeaderLen]); got != id {
		return nil, false, fmt.Errorf("proto: data frame is for stream %d, want %d", got, id)
	}
	chunk := payload[dataHeaderLen:]
	if len(chunk) == 0 {
		return nil, true, nil
	}
	return chunk, false, nil
}

// Close closes the underlying stream.
func (c *Conn) Close() error {
	return c.rw.Close()
}
