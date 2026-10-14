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
//
// A write error is terminal. Once a write has failed, part of a frame may
// already be on the wire, so every later Send and SendData returns that first
// error rather than appending another frame behind a truncated one and letting
// the peer desynchronise silently. Callers must Close the Conn and dial again.
type Conn struct {
	rw io.ReadWriteCloser

	rmu sync.Mutex
	wmu sync.Mutex

	// werr is the first write error, remembered so later writes fail loudly
	// instead of continuing on a stream whose frame boundaries are lost.
	werr error
}

// NewConn wraps rw. The Conn owns rw: Close closes it.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw}
}

// Send writes env as a control frame. Once a write has failed on this Conn,
// Send reports that first error instead of writing, because the stream's frame
// boundaries are no longer trustworthy.
func (c *Conn) Send(env Envelope) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("proto: encode control envelope: %w", err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.werr != nil {
		return fmt.Errorf("proto: write on a broken connection: %w", c.werr)
	}
	if err := WriteFrame(c.rw, FrameControl, payload); err != nil {
		c.werr = err
		return err
	}
	return nil
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
// frame. Chunks must be at most MaxChunk bytes; a larger one is rejected before
// anything is allocated for it.
func (c *Conn) SendData(id uint64, chunk []byte) error {
	if len(chunk) > MaxChunk {
		return fmt.Errorf("proto: data chunk %d bytes too large (max %d)", len(chunk), MaxChunk)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.werr != nil {
		return fmt.Errorf("proto: write on a broken connection: %w", c.werr)
	}
	payload := make([]byte, dataHeaderLen+len(chunk))
	binary.BigEndian.PutUint64(payload[:dataHeaderLen], id)
	copy(payload[dataHeaderLen:], chunk)
	if err := WriteFrame(c.rw, FrameData, payload); err != nil {
		c.werr = err
		return err
	}
	return nil
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

// SendBody writes body as the response stream identified by id, in chunks, then
// ends the stream.
//
// It makes a response of unbounded length possible: a listing of a million files
// is far past one frame's payload, and building it as one buffer would allocate
// the whole thing before finding that out. Here the sender only ever holds one
// chunk.
//
// A zero-byte body is still sent as no chunk at all, followed by the end marker:
// a stream of exactly one end marker. The caller must therefore treat an empty
// reply as a streamed one, which means the envelope's Body must be nil to mark
// a stream — never an empty non-nil slice.
func (c *Conn) SendBody(id uint64, body []byte) error {
	for len(body) > 0 {
		n := len(body)
		if n > MaxChunk {
			n = MaxChunk
		}
		if err := c.SendData(id, body[:n]); err != nil {
			return err
		}
		body = body[n:]
	}
	return c.SendData(id, nil)
}

// RecvBody reads a response stream identified by id until the peer ends it, and
// returns the concatenation.
//
// One frame is read at a time and appended, so memory grows with the body. The
// alternative for a listing would be a cursor, which the protocol does not have:
// until it does, a caller that cannot hold the body must not ask for it.
func (c *Conn) RecvBody(id uint64) ([]byte, error) {
	var body []byte
	for {
		chunk, done, err := c.RecvData(id)
		if err != nil {
			return nil, err
		}
		if done {
			return body, nil
		}
		body = append(body, chunk...)
	}
}

// Close closes the underlying stream.
func (c *Conn) Close() error {
	return c.rw.Close()
}
