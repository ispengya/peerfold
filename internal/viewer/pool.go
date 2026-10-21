package viewer

import (
	"errors"
	"io"
	"sync"

	"github.com/ispengya/peerfold/internal/node"
)

// ErrPoolClosed reports that a pooled call was attempted after Close.
var ErrPoolClosed = errors.New("viewer: connection pool is closed")

// ConnPool runs reads and writes over several viewer connections.
//
// One viewer connection carries one request at a time (viewer.stream), so a
// mount built on a single connection serialises every miss in a directory walk,
// every file a parallel build reads, and every save. A pool removes that
// artificial queue while keeping the primary connection, the one that watches
// for updates, out of the data path entirely.
//
// Connections are opened lazily and reused. A connection on which a call failed
// is closed rather than returned: a timed-out frame leaves the stream's frame
// boundaries unknown, so the only safe recovery is a new connection.
type ConnPool struct {
	primary *Viewer
	size    int

	// tokens bounds the calls in flight, and idle holds the connections that
	// are ready to take one. tokens is acquired before idle so the number of
	// connections the pool will ever open is the same as the number of calls it
	// will ever run at once.
	tokens chan struct{}
	idle   chan *Viewer

	mu      sync.Mutex
	created int
	closed  bool
	done    chan struct{}
	once    sync.Once
}

// NewConnPool builds a pool of at most size data connections to the primary's
// server. size is clamped to at least one.
func NewConnPool(primary *Viewer, size int) *ConnPool {
	if size < 1 {
		size = 1
	}
	return &ConnPool{
		primary: primary,
		size:    size,
		tokens:  make(chan struct{}, size),
		idle:    make(chan *Viewer, size),
		done:    make(chan struct{}),
	}
}

// Size is the number of data connections the pool may open.
func (p *ConnPool) Size() int { return p.size }

// Read runs one ranged read on a pooled connection.
func (p *ConnPool) Read(path string, off int64, n int) ([]byte, error) {
	v, err := p.acquire()
	if err != nil {
		return nil, err
	}
	out, err := v.Read(path, off, n)
	p.release(v, err)
	return out, err
}

// Put runs one whole-file write on a pooled connection.
func (p *ConnPool) Put(path string, r io.Reader, durable bool) error {
	v, err := p.acquire()
	if err != nil {
		return err
	}
	err = v.PutWithDurability(path, r, durable)
	p.release(v, err)
	return err
}

// List runs one listing on a pooled connection.
func (p *ConnPool) List(path string) ([]node.Node, error) {
	v, err := p.acquire()
	if err != nil {
		return nil, err
	}
	out, err := v.List(path)
	p.release(v, err)
	return out, err
}

// Mkdir creates one directory on a pooled connection.
func (p *ConnPool) Mkdir(path string) error {
	v, err := p.acquire()
	if err != nil {
		return err
	}
	err = v.Mkdir(path)
	p.release(v, err)
	return err
}

// Remove deletes one path on a pooled connection.
func (p *ConnPool) Remove(path string) error {
	v, err := p.acquire()
	if err != nil {
		return err
	}
	err = v.Remove(path)
	p.release(v, err)
	return err
}

// Rename moves one path on a pooled connection.
func (p *ConnPool) Rename(oldPath, newPath string) error {
	v, err := p.acquire()
	if err != nil {
		return err
	}
	err = v.Rename(oldPath, newPath)
	p.release(v, err)
	return err
}

// acquire takes a token and a connection, dialling another connection when the
// pool is below its size.
func (p *ConnPool) acquire() (*Viewer, error) {
	select {
	case p.tokens <- struct{}{}:
	case <-p.done:
		return nil, ErrPoolClosed
	}

	select {
	case v := <-p.idle:
		return v, nil
	default:
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.tokens
		return nil, ErrPoolClosed
	}
	canDial := p.created < p.size
	if canDial {
		p.created++
	}
	p.mu.Unlock()

	if canDial {
		v, err := p.primary.NewReadViewer()
		if err == nil {
			return v, nil
		}
		p.mu.Lock()
		p.created--
		p.mu.Unlock()
		<-p.tokens
		return nil, err
	}

	select {
	case v := <-p.idle:
		return v, nil
	case <-p.done:
		<-p.tokens
		return nil, ErrPoolClosed
	}
}

// release returns a usable connection to the idle set, or closes a broken one.
func (p *ConnPool) release(v *Viewer, callErr error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()

	if callErr != nil || closed {
		_ = v.Close()
		p.mu.Lock()
		p.created--
		p.mu.Unlock()
		<-p.tokens
		return
	}
	select {
	case p.idle <- v:
	default:
		// Unreachable while tokens and size are equal, but closing is the safe
		// answer if that invariant is ever changed: never leak a connection.
		_ = v.Close()
		p.mu.Lock()
		p.created--
		p.mu.Unlock()
	}
	<-p.tokens
}

// Close stops new calls and closes every idle data connection. A call already in
// flight closes its connection when it returns. The primary is not closed here:
// its owner is the caller that opened it.
func (p *ConnPool) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		close(p.done)
		p.mu.Unlock()
		for {
			select {
			case v := <-p.idle:
				_ = v.Close()
				p.mu.Lock()
				p.created--
				p.mu.Unlock()
			default:
				return
			}
		}
	})
	return nil
}
