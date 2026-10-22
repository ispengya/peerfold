package mount

import (
	"sync"

	"github.com/ispengya/peerfold/internal/node"
)

// prefetchDepth bounds how many units ahead a read-ahead fills.
const prefetchDepth = 4

// chunkFlight is one in-flight ranged read. Waiters share its result instead of
// sending the same range over the wire again.
type chunkFlight struct {
	done chan struct{}
	data []byte
	err  error
}

// fetchChunk returns one unit of a file, from the cache or the owner.
//
// Concurrent fills of the same (path, generation, unit) are collapsed. The
// collapsing is keyed by chunk rather than by the whole mount, so reads of
// different files can proceed through the connection pool at the same time;
// the old implementation held one mutex across every miss, which made parallel
// readers queue twice (once in the caller, once on the single connection).
//
// A fill stores its bytes with the generation it started from. An event that
// changed the file while the fill was in flight bumps the generation, and the
// late bytes are then discarded rather than published under the new content.
func (m *Mount) fetchChunk(fh uint64, path string, chunkIdx int64, unit int) ([]byte, error) {
	if data, ok := m.cache.getAt(fh, unit, chunkIdx); ok {
		return data, nil
	}
	key, cacheable := m.cache.key(fh, unit, chunkIdx)
	if !cacheable {
		// A handle with no cache binding should not happen in production, but a
		// direct fetch is still the honest answer rather than a wrong cache key.
		return m.readUnit(path, chunkIdx, unit)
	}

	m.inflightMu.Lock()
	if f, ok := m.inflight[key]; ok {
		m.inflightMu.Unlock()
		<-f.done
		return f.data, f.err
	}
	f := &chunkFlight{done: make(chan struct{})}
	m.inflight[key] = f
	m.inflightMu.Unlock()

	data, err := m.readUnit(path, chunkIdx, unit)
	if err == nil {
		m.cache.putUnitAt(fh, key.gen, unit, chunkIdx, data)
	}
	// A partial answer is not cacheable as whole blocks, but it is still
	// returned: throwing away bytes the owner already sent would make a flaky
	// link turn every retry into a fresh transfer.
	f.data, f.err = data, err
	m.inflightMu.Lock()
	delete(m.inflight, key)
	m.inflightMu.Unlock()
	close(f.done)
	return f.data, f.err
}

// readUnit performs one wire read, bounded to one unit, and trims an owner that
// returns more than asked for.
func (m *Mount) readUnit(path string, chunkIdx int64, unit int) ([]byte, error) {
	data, err := m.call().Read(path, chunkIdx*int64(unit), unit)
	if len(data) > unit {
		data = data[:unit]
	}
	return data, err
}

// noteChunkAccess records this handle's latest read unit and reports whether it
// continues the previous one. Only a sequential reader is worth prefetching
// for: a random reader would pay for bytes it never asked for.
func (m *Mount) noteChunkAccess(fh uint64, chunkIdx int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.files[fh]
	if !ok {
		return false
	}
	sequential := h.lastChunk >= 0 && chunkIdx == h.lastChunk+1
	h.lastChunk = chunkIdx
	return sequential
}

// startPrefetch fills the units after a read-ahead point in the background.
//
// It takes a prefetch token without waiting: when the data path is already
// using the pool, speculative work is skipped rather than queued ahead of the
// next real read. One token covers a whole run of units, so speculative work
// never outnumbers data-path requests.
//
// The units are fetched concurrently, and each is bounded by the same
// connection pool the data path uses. Filling them one after another left every
// extra connection idle and made read-ahead finish only after the reader had
// already caught up.
func (m *Mount) startPrefetch(fh uint64, path string, from int64, unit int) {
	select {
	case m.prefetchTokens <- struct{}{}:
	default:
		return
	}
	depth := m.readConcurrency()
	if depth < prefetchDepth {
		depth = prefetchDepth
	}
	if depth > 2*prefetchDepth {
		depth = 2 * prefetchDepth
	}
	go func() {
		defer func() { <-m.prefetchTokens }()
		sem := make(chan struct{}, m.readConcurrency())
		var wg sync.WaitGroup
		for i := int64(0); i < int64(depth); i++ {
			idx := from + i
			// Stop if the handle was closed, reused for another file, or made
			// dirty: the cached bytes would no longer describe what a reader
			// should see.
			if !m.prefetchStillValid(fh, path) {
				break
			}
			if _, ok := m.cache.getAt(fh, unit, idx); ok {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(idx int64) {
				defer wg.Done()
				defer func() { <-sem }()
				if !m.prefetchStillValid(fh, path) {
					return
				}
				if _, ok := m.cache.getAt(fh, unit, idx); ok {
					return
				}
				_, _ = m.fetchChunk(fh, path, idx, unit)
			}(idx)
		}
		wg.Wait()
	}()
}

// prefetchStillValid reports whether a handle is still reading path and is
// clean. A dirty handle serves reads from its own buffer, so fetching the
// owner's copy would only waste bandwidth.
func (m *Mount) prefetchStillValid(fh uint64, path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.files[fh]
	return ok && h.path == path && h.kind == node.KindFile && h.isClean()
}
