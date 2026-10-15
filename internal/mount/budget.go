package mount

import (
	"container/list"
	"sync"
)

// chunkSize is the unit a ranged read is broken into.
//
// proto.MaxPayload is 1 MiB and the probe measured the per-frame cost rising
// with frame size (64 KiB took 533us, 256 KiB 1000us, 1 MiB 3000us), so a full
// frame is the worst choice. 128 KiB stays well inside one frame with room to
// spare, and it is large enough that a sequential reader of a big file is not
// paying a request per few kilobytes.
//
// The measurement also shows the cost is not linear in size — 64 KiB is the
// cheapest per byte of those three — so this is deliberately not tuned to the
// smallest frame. Smaller chunks mean more round trips, and a round trip costs
// far more than the per-frame overhead it saves.
const chunkSize = 128 << 10

type chunkKey struct {
	fh    uint64
	chunk int64
}

// chunkCache holds read chunks for every open file under two limits: a per-open
// cap so one large file cannot crowd out the others, and a global cap so the
// process's memory is actually bounded.
//
// The first design had only the per-open cap and described it as bounding
// everything, which is true for one open and false for N. With N opens each
// respecting 64 MiB, the process holds N x 64 MiB, and the CLI would have had a
// knob that bounded nothing in the case that matters.
//
// Both limits are enforced by eviction, so a limit is a ceiling that is never
// exceeded rather than a budget that is refused. The alternative — refusing to
// cache when full — would turn a full cache into a latency cliff instead of a
// gentle one. The single exception is a single chunk larger than the whole
// global cap, which is not cached at all because caching it would evict
// everything else and still not fit.
//
// The eviction order is one LRU list across all handles. Evicting the globally
// oldest chunk is what makes the global cap a bound on the process rather than
// a bound per handle, and it means a handle that is idle loses its chunks to
// one that is active, which is the behaviour a cache should have.
type chunkCache struct {
	perOpen int
	global  int
	// bytes is the cached byte count per handle. A handle with nothing cached
	// is absent rather than present-and-zero, so the map does not accumulate
	// entries for every handle that was ever opened.
	bytes map[uint64]int
	items map[chunkKey][]byte
	// order is the eviction queue, most recently used at the back.
	order *list.List
	// index locates a key in order, so a hit can move it to the back without
	// scanning the list.
	index map[chunkKey]*list.Element

	// mu guards everything above.
	//
	// cgofuse calls the filesystem's methods from several threads at once, so
	// two Read calls on different handles can reach this cache in parallel. The
	// lock is here rather than in the caller because the cache's own eviction
	// accounting is what a race would corrupt: two concurrent puts can each
	// observe the total as within the cap and each add, leaving the cache
	// permanently over its bound with no later operation to notice.
	//
	// A plain mutex rather than a read-write one, because a get mutates the
	// recency order: even reads are writes as far as this structure is
	// concerned.
	mu sync.Mutex
}

func newChunkCache(perOpen, global int) *chunkCache {
	return &chunkCache{
		perOpen: perOpen,
		global:  global,
		bytes:   map[uint64]int{},
		items:   map[chunkKey][]byte{},
		order:   list.New(),
		index:   map[chunkKey]*list.Element{},
	}
}

// get returns a cached chunk and marks it recently used.
//
// The slice is returned as stored, without copying: the caller is the read path,
// which copies out of it into the caller's buffer. Handing back the cached slice
// is what makes a hit cheap, and it is safe because nothing writes to a cached
// chunk in place — a chunk is immutable once read from the owner. The lock is
// released before the caller touches those bytes, which is sound precisely
// because no one ever mutates them.
func (c *chunkCache) get(fh uint64, chunk int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := chunkKey{fh, chunk}
	data, ok := c.items[k]
	if !ok {
		return nil, false
	}
	if el, ok := c.index[k]; ok {
		c.order.MoveToBack(el)
	}
	return data, true
}

// put caches one chunk. The data is stored as given and must not be modified
// afterwards by the caller; the read path allocates each chunk for this purpose.
//
// A put for a key already present is ignored rather than replacing the stored
// bytes. A chunk's content is immutable, so a second put for the same key is the
// same bytes, and replacing would throw away the position the existing entry has
// in the eviction order.
func (c *chunkCache) put(fh uint64, chunk int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(data) > c.global {
		// Larger than the entire budget: caching it would evict everything
		// else and still not fit, so serve it and forget it.
		return
	}
	k := chunkKey{fh, chunk}
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = data
	c.index[k] = c.order.PushBack(k)
	c.bytes[fh] += len(data)
	c.evictOpen(fh)
	c.evictGlobal()
}

// evictOpen trims one handle down to the per-open cap, oldest first.
func (c *chunkCache) evictOpen(fh uint64) {
	for c.bytes[fh] > c.perOpen {
		el := c.oldestOf(fh)
		if el == nil {
			return
		}
		c.removeElement(el)
	}
}

// evictGlobal trims the whole cache to the global cap, evicting across handles
// oldest-first. This is what keeps N concurrent opens bounded.
func (c *chunkCache) evictGlobal() {
	// totalLocked, not total: this runs with mu held, and the exported wrapper
	// would take the same mutex again and self-deadlock.
	for c.totalLocked() > c.global {
		el := c.order.Front()
		if el == nil {
			return
		}
		c.removeElement(el)
	}
}

// oldestOf finds the least recently used element belonging to one handle.
//
// This scans the whole order list, which is O(number of cached chunks) rather
// than O(1). It is acceptable because it only runs while a handle is over its
// cap, and the per-open cap bounds how many of that handle's chunks are in the
// list. If the per-open cap ever grows to hold many chunks, this is the function
// to replace with a per-handle list.
func (c *chunkCache) oldestOf(fh uint64) *list.Element {
	for el := c.order.Front(); el != nil; el = el.Next() {
		if el.Value.(chunkKey).fh == fh {
			return el
		}
	}
	return nil
}

func (c *chunkCache) removeElement(el *list.Element) {
	k := el.Value.(chunkKey)
	data := c.items[k]
	delete(c.items, k)
	delete(c.index, k)
	c.bytes[k.fh] -= len(data)
	if c.bytes[k.fh] <= 0 {
		delete(c.bytes, k.fh)
	}
	c.order.Remove(el)
}

// dropOpen releases every chunk belonging to one handle. It is called when a
// file is released, so that an idle file's cache does not occupy the budget
// that the global cap is supposed to be protecting.
func (c *chunkCache) dropOpen(fh uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropOpenLocked(fh)
}

// dropOpenLocked is dropOpen without the lock. It exists so that a caller that
// already holds the Mount's lock can release a handle's memory while acquiring
// only one lock at a time, which is what keeps the lock order from ever being a
// question. See the comment on Mount.mu.
func (c *chunkCache) dropOpenLocked(fh uint64) {
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if el.Value.(chunkKey).fh == fh {
			c.removeElement(el)
		}
		el = next
	}
	delete(c.bytes, fh)
}

// total is the cached bytes across every handle, which is the number the global
// cap bounds.
func (c *chunkCache) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.totalLocked()
}

func (c *chunkCache) totalLocked() int {
	n := 0
	for _, b := range c.bytes {
		n += b
	}
	return n
}

// openTotal is the cached bytes for one handle, which is the number the
// per-open cap bounds.
func (c *chunkCache) openTotal(fh uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes[fh]
}
