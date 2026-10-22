package mount

import (
	"container/list"
	"strconv"
	"strings"
	"sync"
)

// chunkSize is the default unit a ranged read is broken into.
//
// The number is a latency decision, not a CPU one. A mount on another machine
// pays one round trip per chunk, so the ceiling on sequential throughput is
// roughly chunkSize/RTT: 128 KiB over a 50 ms link is 2.5 MB/s regardless of
// available bandwidth. 512 KiB reduces the round trips fourfold while still
// fitting comfortably inside one 1 MiB wire frame. Options.ChunkSize overrides
// it, and readChunk adapts within the protocol's range.
const chunkSize = 512 << 10

// MaxChunkSize bounds the configured read unit. A read's content is split into
// frames by the wire, so a larger unit only holds more memory for one cache
// entry and delays the first byte of a small read.
const MaxChunkSize = 1 << 20

// cacheBlock is the granularity the read cache stores, independent of the read
// unit.
//
// Keeping the unit out of the cache key is what lets the adaptive unit change
// without throwing away everything already fetched: a 1 MiB read is assembled
// from four of these blocks, whether the previous read fetched them as two
// 512 KiB units or four 256 KiB ones. Before this split, the unit was part of
// the key, so the first adaptive change made every cached byte unreachable and
// the same file was downloaded again.
//
// It must divide every allowed read unit. readChunk normalises its result to a
// multiple of this value.
const cacheBlock = 256 << 10

// blockKey identifies one cached block by its file, the generation of that
// file's content, and the block index.
//
// The generation is what makes invalidation cheap and safe. A cache fill can be
// in flight when an event says the file changed; the fill completes afterwards
// and must not publish the old bytes under the new state. Bumping the
// generation makes every in-flight fill for that path unwritable, without
// waiting for it.
type blockKey struct {
	path  string
	gen   uint64
	block int64
}

// chunkKey identifies one ranged read that is in flight: the file, the content
// generation the read started from, and the unit it covers. It is not a cache
// key; cached bytes are stored by blockKey.
type chunkKey struct {
	path  string
	gen   uint64
	unit  int
	chunk int64
}

// chunkCache holds read chunks for every open file under two limits: a per-file
// cap so one large file cannot crowd out the others, and a global cap so the
// process's memory is actually bounded.
//
// Entries are keyed by path rather than by handle. A file that is closed and
// reopened, or read through two handles at once, reuses the chunks already
// fetched; that is the difference between a build that reads the same
// node_modules package once per worker and one that pays a round trip each
// time. A change to the file invalidates the path, and a reconnect clears the
// whole cache because events may have been missed.
//
// Both limits are enforced by eviction, so a limit is a ceiling that is never
// exceeded rather than a budget that is refused. The alternative — refusing to
// cache when full — would turn a full cache into a latency cliff instead of a
// gentle one. The single exception is a single chunk larger than the whole
// global cap, which is not cached at all because caching it would evict
// everything else and still not fit.
//
// The eviction order is one LRU list across all files. Evicting the globally
// oldest chunk is what makes the global cap a bound on the process rather than
// a bound per file, and it means an idle file loses its chunks to an active
// one, which is the behaviour a cache should have.
type chunkCache struct {
	perOpen int
	global  int
	// bytes is the cached byte count per path. A path with nothing cached is
	// absent rather than present-and-zero, so the map does not accumulate an
	// entry for every file that was ever opened.
	bytes map[string]int
	items map[blockKey][]byte
	// order is the eviction queue, most recently used at the back.
	order *list.List
	// index locates a key in order, so a hit can move it to the back without
	// scanning the list.
	index map[blockKey]*list.Element
	// byPath locates the chunks of one path, so invalidation removes exactly
	// those entries rather than scanning the whole cache per event.
	byPath map[string]map[blockKey]struct{}
	// fhPath maps an open handle to the path it reads. It is the bridge between
	// the handle-keyed read path and the path-keyed cache.
	fhPath map[uint64]string
	// gen is the current content generation of each path. It is bumped by
	// invalidation; a chunk stored under an older generation is never read.
	gen map[string]uint64

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
		bytes:   map[string]int{},
		items:   map[blockKey][]byte{},
		order:   list.New(),
		index:   map[blockKey]*list.Element{},
		byPath:  map[string]map[blockKey]struct{}{},
		fhPath:  map[uint64]string{},
		gen:     map[string]uint64{},
	}
}

// syntheticPath is the cache path used by a test that puts chunks under a bare
// handle number. It is distinct from every real path, which starts with "/".
func syntheticPath(fh uint64) string {
	return "\x00fh/" + strconv.FormatUint(fh, 10)
}

// bind records which file an open handle reads. The path's generation starts at
// one the first time it is seen.
func (c *chunkCache) bind(fh uint64, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bindLocked(fh, path)
}

func (c *chunkCache) bindLocked(fh uint64, path string) {
	c.fhPath[fh] = path
	if _, ok := c.gen[path]; !ok {
		c.gen[path] = 1
	}
}

// unbind forgets a closed handle without discarding the file's chunks. The
// chunks stay available to the next open of the same path.
func (c *chunkCache) unbind(fh uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unbindLocked(fh)
}

func (c *chunkCache) unbindLocked(fh uint64) { delete(c.fhPath, fh) }

// generation returns the current content generation for a handle's path, or 0
// when the handle is not bound. A fill remembers this value and stores its
// bytes with putUnitAt, so a change during the fill cannot be overwritten.
func (c *chunkCache) generation(fh uint64) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, ok := c.fhPath[fh]
	if !ok {
		return 0
	}
	return c.gen[path]
}

// key returns the cache key a handle's chunk would use right now. It is how a
// fill captures the generation it is about to read, and how prefetch names the
// chunk it is filling.
func (c *chunkCache) key(fh uint64, unit int, chunk int64) (chunkKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, ok := c.fhPath[fh]
	if !ok {
		return chunkKey{}, false
	}
	return chunkKey{path: path, gen: c.gen[path], unit: unit, chunk: chunk}, true
}

// get returns one cached block and marks it recently used.
//
// The slice is returned as stored, without copying: the caller copies out of it
// into its own buffer. Handing back the cached slice is what makes a hit cheap,
// and it is safe because nothing writes to a cached block in place — a block is
// immutable once read from the owner. The lock is released before the caller
// touches those bytes, which is sound precisely because no one ever mutates
// them.
func (c *chunkCache) get(fh uint64, block int64) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, ok := c.fhPath[fh]
	if !ok {
		return nil, false
	}
	return c.getBlockLocked(blockKey{path: path, gen: c.gen[path], block: block})
}

// getBlockLocked returns one block and moves it to the back of the LRU order.
// The caller holds the lock.
func (c *chunkCache) getBlockLocked(k blockKey) ([]byte, bool) {
	data, ok := c.items[k]
	if !ok {
		return nil, false
	}
	if el, ok := c.index[k]; ok {
		c.order.MoveToBack(el)
	}
	return data, true
}

// getAt assembles one read unit from the fixed-size blocks.
//
// A unit is a transfer decision, not a storage decision: it may be 256 KiB,
// 512 KiB, or 1 MiB, and whichever size the previous fill used, the blocks it
// produced are reusable here. A missing block means the whole unit misses;
// callers then fetch it and the fill stores its blocks for the next unit size.
//
// A short block is the file's last one, so the unit ends there: the assembled
// result is returned as a hit even though it is shorter than a unit.
func (c *chunkCache) getAt(fh uint64, unit int, chunk int64) ([]byte, bool) {
	if unit <= 0 || unit%cacheBlock != 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	path, ok := c.fhPath[fh]
	if !ok {
		return nil, false
	}
	gen := c.gen[path]
	first := chunk * int64(unit) / int64(cacheBlock)
	out := make([]byte, 0, unit)
	for i := 0; i < unit/cacheBlock; i++ {
		data, ok := c.getBlockLocked(blockKey{path: path, gen: gen, block: first + int64(i)})
		if !ok {
			return nil, false
		}
		out = append(out, data...)
		if len(data) < cacheBlock {
			return out, true
		}
	}
	return out, true
}

// put caches one block at the path's current generation. It is the convenience
// form used by tests and by callers that were not in flight across a change.
func (c *chunkCache) put(fh uint64, block int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path, ok := c.fhPath[fh]
	if !ok {
		path = syntheticPath(fh)
		c.bindLocked(fh, path)
	}
	c.putLocked(blockKey{path: path, gen: c.gen[path], block: block}, data)
}

// putUnitAt stores a fetched unit as its fixed-size blocks. A fill that raced an
// invalidation is discarded: its bytes describe the file as it was, and the
// next read must go back to the owner.
func (c *chunkCache) putUnitAt(fh uint64, gen uint64, unit int, chunk int64, data []byte) {
	if unit <= 0 || unit%cacheBlock != 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	path, ok := c.fhPath[fh]
	if !ok || gen == 0 || c.gen[path] != gen {
		return
	}
	first := chunk * int64(unit) / int64(cacheBlock)
	for off := 0; off < len(data); off += cacheBlock {
		end := off + cacheBlock
		if end > len(data) {
			end = len(data)
		}
		c.putLocked(blockKey{
			path:  path,
			gen:   gen,
			block: first + int64(off/cacheBlock),
		}, data[off:end])
	}
}

// putLocked stores one block. The data is stored as given and must not be
// modified afterwards by the caller; the read path allocates each chunk for
// this purpose.
//
// A put for a key already present is ignored rather than replacing the stored
// bytes. A block's content is immutable, so a second put for the same key is the
// same bytes, and replacing would throw away the position the existing entry has
// in the eviction order.
func (c *chunkCache) putLocked(k blockKey, data []byte) {
	if len(data) > c.global {
		// Larger than the entire budget: caching it would evict everything
		// else and still not fit, so serve it and forget it.
		return
	}
	if _, ok := c.items[k]; ok {
		return
	}
	c.items[k] = data
	c.index[k] = c.order.PushBack(k)
	c.bytes[k.path] += len(data)
	if c.byPath[k.path] == nil {
		c.byPath[k.path] = map[blockKey]struct{}{}
	}
	c.byPath[k.path][k] = struct{}{}
	c.evictOpen(k.path)
	c.evictGlobal()
}

// evictOpen trims one path down to the per-file cap, oldest first.
func (c *chunkCache) evictOpen(path string) {
	for c.bytes[path] > c.perOpen {
		el := c.oldestOf(path)
		if el == nil {
			return
		}
		c.removeElement(el)
	}
}

// evictGlobal trims the whole cache to the global cap, evicting across files
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

// oldestOf finds the least recently used element belonging to one path.
//
// This scans the whole order list, which is O(number of cached chunks) rather
// than O(1). It is acceptable because it only runs while a path is over its
// cap, and the cap bounds how many of that path's chunks are in the list.
func (c *chunkCache) oldestOf(path string) *list.Element {
	for el := c.order.Front(); el != nil; el = el.Next() {
		if el.Value.(blockKey).path == path {
			return el
		}
	}
	return nil
}

func (c *chunkCache) removeElement(el *list.Element) {
	k := el.Value.(blockKey)
	data := c.items[k]
	delete(c.items, k)
	delete(c.index, k)
	c.bytes[k.path] -= len(data)
	if c.bytes[k.path] <= 0 {
		delete(c.bytes, k.path)
	}
	if set := c.byPath[k.path]; set != nil {
		delete(set, k)
		if len(set) == 0 {
			delete(c.byPath, k.path)
		}
	}
	c.order.Remove(el)
}

// invalidate discards every cached chunk for one path and starts a new
// generation, so a fill already in flight cannot repopulate it.
func (c *chunkCache) invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen[path]++
	c.dropPathLocked(path)
}

// invalidateTree discards path and everything beneath it, for a tombstone.
func (c *chunkCache) invalidateTree(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	under := func(p string) bool {
		return p == path || path == "/" || strings.HasPrefix(p, path+"/")
	}
	for p := range c.byPath {
		if under(p) {
			c.gen[p]++
			c.dropPathLocked(p)
		}
	}
	// A bound path with nothing cached still needs its generation bumped: a
	// fill for it may be in flight, and a handle reading it may be open.
	for _, p := range c.fhPath {
		if under(p) {
			c.gen[p]++
		}
	}
	c.gen[path]++
}

// clear discards every cached byte. It is what a reconnect uses: events were
// missed while the connection was down, so no cached byte can be assumed
// current.
//
// Open handles stay bound. Their file's cache was cleared, not their identity,
// and a handle that forgot its path would start caching under a synthetic key
// that no later read ever consults.
func (c *chunkCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes = map[string]int{}
	c.items = map[blockKey][]byte{}
	c.order = list.New()
	c.index = map[blockKey]*list.Element{}
	c.byPath = map[string]map[blockKey]struct{}{}
	for p := range c.gen {
		c.gen[p]++
	}
	for _, p := range c.fhPath {
		if _, ok := c.gen[p]; !ok {
			c.gen[p] = 1
		}
	}
}

// dropPathLocked removes every chunk of one path.
func (c *chunkCache) dropPathLocked(path string) {
	for k := range c.byPath[path] {
		if el, ok := c.index[k]; ok {
			c.removeElement(el)
		}
	}
	delete(c.byPath, path)
	delete(c.bytes, path)
}

// dropOpen releases every chunk belonging to one handle's path and forgets the
// handle. It is kept for tests and for the reconnect path that has to throw a
// handle away; an ordinary close uses unbind so the file's chunks survive.
func (c *chunkCache) dropOpen(fh uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropOpenLocked(fh)
}

// dropOpenLocked is dropOpen without the lock.
func (c *chunkCache) dropOpenLocked(fh uint64) {
	path, ok := c.fhPath[fh]
	if !ok {
		return
	}
	c.dropPathLocked(path)
	delete(c.fhPath, fh)
}

// total is the cached bytes across every file, which is the number the global
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

// openTotal is the cached bytes for one handle's file, which is the number the
// per-file cap bounds. Two handles on the same path report the same total.
func (c *chunkCache) openTotal(fh uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes[c.fhPath[fh]]
}
