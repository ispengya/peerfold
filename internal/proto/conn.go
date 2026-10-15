package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
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

// SendWithin writes env as a control frame, giving up after d.
//
// It exists for a message one end sends without being asked — an index update
// pushed to a watcher — where the writer must not be held up by a reader that
// has stopped reading. A peer that stops reading fills its socket buffer, and an
// unbounded write to it blocks until the buffer drains, which for a reader that
// never reads again is never. One peer's misbehaviour would then become every
// peer's, since the writer is the one fanning the message out.
//
// A deadline needs a socket, so on a connection that is not one the write is
// attempted without a bound. That case is produced by tests that drive a Conn
// over an in-memory pipe; there is no buffer to fill when the writer and the
// reader are the same process.
func (c *Conn) SendWithin(env Envelope, d time.Duration) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("proto: encode control envelope: %w", err)
	}
	dc, ok := c.rw.(deadliner)
	if !ok {
		return c.Send(env)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.werr != nil {
		return fmt.Errorf("proto: write on a broken connection: %w", c.werr)
	}
	if err := dc.SetWriteDeadline(time.Now().Add(d)); err != nil {
		return fmt.Errorf("proto: set write deadline: %w", err)
	}
	// The deadline is cleared whatever happens, so it bounds this send rather
	// than every write that follows it. Leaving it set would make an unrelated
	// later write fail with a timeout it never asked for.
	defer func() { _ = dc.SetWriteDeadline(time.Time{}) }()

	if err := WriteFrame(c.rw, FrameControl, payload); err != nil {
		c.werr = err
		return err
	}
	return nil
}

// deadliner is an io.ReadWriteCloser that can bound one write, which is what a
// net.Conn provides.
type deadliner interface {
	SetWriteDeadline(t time.Time) error
}

// Recv reads the next control frame, passing over any pushed index update.
//
// Skipping is the right default for a peer that receives updates it has not
// asked for — a viewer — because an update can otherwise land between a request
// and its reply and be mistaken for the reply. A peer that must see every
// control frame uses RecvAny: the server is such a peer, because a client's
// update is a frame it has to act on rather than one it passes over.
func (c *Conn) Recv() (Envelope, error) {
	return c.recvControl(nil)
}

// RecvAny reads the next control frame, whatever its kind.
func (c *Conn) RecvAny() (Envelope, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return c.readControl()
}

// RecvEvent reads the next control frame that is not a pushed index update,
// calling handle for each update it passes.
//
// It exists for the reader that publishes updates: it is Recv with somewhere to
// put what is skipped.
func (c *Conn) RecvEvent(handle func(Envelope)) (Envelope, error) {
	return c.recvControl(handle)
}

// recvControl reads frames until one is not an update, and returns it.
//
// Every read of a viewer's connection goes through here or through RecvData, and
// both pass over updates: the server pushes them whenever any supplying client
// reports a change, so one can land between a reply's head frame and its body,
// or between two chunks of a read's content. A body reader that took one for
// data would desynchronise the stream.
func (c *Conn) recvControl(handle func(Envelope)) (Envelope, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		env, err := c.readControl()
		if err != nil {
			return Envelope{}, err
		}
		if env.Kind != KindEvent {
			return env, nil
		}
		if handle != nil {
			handle(env)
		}
	}
}

// readControl reads exactly one frame and requires it to be a control frame. The
// caller holds rmu.
func (c *Conn) readControl() (Envelope, error) {
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
	return c.sendDataLocked(id, chunk)
}

// sendDataLocked writes one data frame. The caller holds wmu, and the caller is
// responsible for checking werr first if it cares about the reason for a refusal.
func (c *Conn) sendDataLocked(id uint64, chunk []byte) error {
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
//
// A pushed update that lands between two chunks is passed over rather than
// mistaken for data: see recvControl.
func (c *Conn) RecvData(id uint64) ([]byte, bool, error) {
	return c.RecvDataEvent(id, nil)
}

// RecvDataEvent is RecvData with somewhere to put two things RecvData has to
// throw away: a pushed index update that lands between two chunks, and a control
// frame that reports the stream was cut short.
//
// Both are passed to handle. An update needs a publisher, and a truncation needs
// a reader that will act on it — RecvData has neither, so both are dropped
// there, which is right for a caller reading a stream whose failure it cannot
// report or undo.
//
// A control frame that is not an update arrives here when a sender's content
// failed part-way and it told the receiver so before ending the stream; the
// envelope carries the reason and the reader must treat the content as
// incomplete. This is the only way a receiver can tell a whole stream from a
// prefix: the end marker is identical either way, so without the envelope a
// truncated file would be published as a complete one.
func (c *Conn) RecvDataEvent(id uint64, handle func(Envelope)) ([]byte, bool, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		kind, payload, err := ReadFrame(c.rw)
		if err != nil {
			return nil, false, err
		}
		if kind == FrameControl {
			var env Envelope
			if err := json.Unmarshal(payload, &env); err != nil {
				// A control frame this reader cannot read is still a control
				// frame: it is not data, so it cannot be mistaken for a chunk,
				// and dropping it keeps the stream in step.
				continue
			}
			if handle != nil {
				handle(env)
			}
			continue
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

// SendStreamError tells the peer that a content stream was cut short, so it can
// tell a prefix of a file from the whole of one.
//
// It exists as a method because both ends need it: the server when the content
// it was forwarding fails, and a viewer when its own local reader dies part-way
// through sending a file. Either way the end marker alone cannot say it — "the
// stream ended" and "the stream is all there" are the same frame — so a receiver
// that is not told publishes the truncated bytes as a complete file.
//
// The envelope goes out on id with the reason in Err. It is not checked for
// success: the caller is already failing, and a connection that cannot carry the
// notice is a connection whose reader sees the failure anyway.
func (c *Conn) SendStreamError(id uint64, cause error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.sendStreamErrorLocked(id, cause)
}

// sendStreamErrorLocked is SendStreamError for a caller that already holds wmu —
// which SendAnnounced does, for the whole of a stream.
func (c *Conn) sendStreamErrorLocked(id uint64, cause error) {
	payload, err := json.Marshal(Envelope{ID: id, Err: cause.Error()})
	if err != nil {
		return
	}
	if c.werr != nil {
		return
	}
	if err := WriteFrame(c.rw, FrameControl, payload); err != nil {
		// Remembered like every other write failure. The first write error is
		// terminal for this connection, and a notice that failed to reach the
		// peer is one the peer will never act on — so leaving werr clear would
		// let the next writer attempt a connection already known to be broken
		// and report its own unrelated failure instead of this one.
		c.werr = err
	}
}

// SendAnnounced writes a request frame and then, while still holding the write
// lock, the content stream that belongs to it.
//
// The lock is what this method exists for. A write is an announcement followed
// by content on the same stream ID, and a second sender slipping a frame between
// the two would leave the peer reading a data frame where it expected a control
// frame. Holding the lock across both keeps the pair indivisible.
//
// content receives the stream ID the announcement was stamped with, and a
// function that writes one content chunk under the lock this method holds. A nil
// content means the announcement stands alone, which is what a write with no
// bytes (mkdir, remove, rename) sends.
//
// content may also return a notice alongside its error — an Envelope to send to
// the receiver before the end marker, on the same stream. It exists for
// forwarding: a proxy between two peers can read a truncation notice from one
// and has to pass it on to the other, and the notice is a control frame of this
// stream rather than data of it. A nil notice means there is nothing to pass on,
// which is the normal failure.
//
// The stream is always terminated with the end marker, so the peer can tell
// "the content has ended" from "the connection stalled".
func (c *Conn) SendAnnounced(id uint64, env Envelope, content func(id uint64, send func([]byte) error) (Envelope, error)) error {
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
	if content != nil {
		send := func(chunk []byte) error {
			if len(chunk) > MaxChunk {
				return fmt.Errorf("proto: content chunk %d bytes too large (max %d)", len(chunk), MaxChunk)
			}
			return c.sendDataLocked(id, chunk)
		}
		if notice, err := content(id, send); err != nil {
			// The content failed part-way, and the receiver has to be told,
			// because the end marker below cannot say it: "the stream ended" and
			// "the stream is all there" look identical on the wire, so a stream
			// that stopped after three bytes of a megabyte would be taken for a
			// complete three-byte file and published at the target name.
			//
			// A notice carried out of content takes precedence: it is the
			// upstream peer's own words about why the content stopped, and this
			// end is only the relay. Otherwise the reason here is the one to
			// send. The frame is written under the same lock as the rest of the
			// stream, so it cannot land anywhere except between two chunks of
			// this stream.
			//
			// Best-effort: the notice's own failure is not reported, because the
			// failure being reported is the one that matters and a broken
			// connection reports itself to the receiver anyway.
			cause := err
			if notice.Err != "" {
				cause = errors.New(notice.Err)
			}
			c.sendStreamErrorLocked(id, cause)
			_ = c.sendDataLocked(id, nil)
			return err
		}
	}
	if err := c.sendDataLocked(id, nil); err != nil {
		return err
	}
	return nil
}

// SetReadDeadline bounds the next reads on this connection, or clears the bound
// when d is the zero time.
//
// The server uses it to bound a cleanup read: a put whose forwarding failed has
// content on the wire that must be read off to leave the connection at a frame
// boundary, but the requester may be part way through sending it and nobody is
// going to ask for the rest. Without a bound that cleanup read waits for as long
// as the requester cares to take.
//
// A stream that cannot be bounded says so by returning false, which is not an
// error: a pipe used in a test has no deadline, and a caller that needs a bound
// to be safe has to decide what to do about its absence. Only the read framing
// changes here, never the frame boundaries.
func (c *Conn) SetReadDeadline(t time.Time) bool {
	d, ok := c.rw.(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		return false
	}
	return d.SetReadDeadline(t) == nil
}

// Close closes the underlying stream.
func (c *Conn) Close() error {
	return c.rw.Close()
}
