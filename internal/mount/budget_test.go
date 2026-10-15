package mount

import "testing"

func TestChunkCacheHitsAndMisses(t *testing.T) {
	c := newChunkCache(1<<20, 4<<20)
	if _, ok := c.get(1, 0); ok {
		t.Fatal("empty cache reported a hit")
	}
	c.put(1, 0, []byte("hello"))
	got, ok := c.get(1, 0)
	if !ok || string(got) != "hello" {
		t.Fatalf("get after put = %q, %v", got, ok)
	}
	if _, ok := c.get(1, 1); ok {
		t.Error("a different chunk must not hit")
	}
	if _, ok := c.get(2, 0); ok {
		t.Error("a different handle must not hit")
	}
}

// The per-open limit bounds one handle; only the global limit bounds the
// process. The original design said memory was pinned at 64 MiB "no matter how
// large the file", which is true for one file and false for N.
func TestPerOpenLimitEvicts(t *testing.T) {
	perOpen := 3 * len("xxxx")
	c := newChunkCache(perOpen, 1<<20)
	for i := int64(0); i < 5; i++ {
		c.put(1, i, []byte("xxxx"))
	}
	if got := c.openTotal(1); got > perOpen {
		t.Errorf("openTotal = %d, exceeds the per-open limit %d", got, perOpen)
	}
}

// The test the original design could not have written, because it claimed a
// single knob bounded everything.
func TestGlobalLimitBoundsAllOpens(t *testing.T) {
	global := 8 * len("xxxx")
	c := newChunkCache(1<<20, global)
	for fh := uint64(1); fh <= 10; fh++ {
		for i := int64(0); i < 20; i++ {
			c.put(fh, i, []byte("xxxx"))
		}
	}
	if got := c.total(); got > global {
		t.Errorf("total = %d, exceeds the global limit %d", got, global)
	}
}

func TestDropOpenReleasesItsMemory(t *testing.T) {
	c := newChunkCache(1<<20, 1<<20)
	c.put(1, 0, []byte("aaaa"))
	c.put(2, 0, []byte("bbbb"))
	before := c.total()
	c.dropOpen(1)
	if c.total() >= before {
		t.Errorf("total did not shrink: %d then %d", before, c.total())
	}
	if _, ok := c.get(1, 0); ok {
		t.Error("dropped handle still hits")
	}
	if _, ok := c.get(2, 0); !ok {
		t.Error("dropping one handle must not disturb another")
	}
}

// A chunk larger than the whole budget must not be cached, and must not panic.
func TestOversizedChunkIsNotCached(t *testing.T) {
	c := newChunkCache(4, 4)
	c.put(1, 0, []byte("this is much longer than the budget"))
	if c.total() > 4 {
		t.Errorf("total = %d, want it bounded by 4", c.total())
	}
}

// The caps bound the memory, but the LRU order is what decides which bytes are
// dropped, and the plan's tests never exercise it: they check the ceilings after
// inserting, which pass whether the cache is LRU or newest-first or random. This
// is the test that pins the actual eviction policy, because a wrongly evicted
// chunk is a correctness-adjacent waste (the next read of it costs a round trip)
// rather than a bound violation.
func TestEvictionIsLeastRecentlyUsed(t *testing.T) {
	// Room for exactly two four-byte chunks.
	c := newChunkCache(1<<20, 8)
	c.put(1, 0, []byte("aaaa"))
	c.put(1, 1, []byte("bbbb"))

	// Touch chunk 0 so chunk 1 becomes the least recently used.
	if _, ok := c.get(1, 0); !ok {
		t.Fatal("chunk 0 should be present before the touch")
	}

	// Inserting a third must evict chunk 1, not the chunk just used.
	c.put(1, 2, []byte("cccc"))

	if _, ok := c.get(1, 1); ok {
		t.Error("chunk 1 was used least recently and should have been evicted")
	}
	if _, ok := c.get(1, 0); !ok {
		t.Error("chunk 0 was used just before the insert and must survive")
	}
	if _, ok := c.get(1, 2); !ok {
		t.Error("the chunk just inserted must be present")
	}
}

// The global cap evicts across handles, so a chunk must be able to go because
// the *process* is full even when the handle being written is under its own cap.
// That is what makes the global cap a bound on the process rather than a
// per-handle bound, and it is what the single global list exists to produce.
//
// The setup is small on purpose, because the interesting difficulty is a trap:
// evictOpen runs before evictGlobal, so a handle's own second chunk is evicted by
// its own cap during the very put that adds it. A test that tried to build up
// several chunks per handle and then watch a cross-handle eviction would find the
// per-open pass had already removed them, and would prove nothing about the
// global one. So each handle here contributes exactly one chunk, well inside its
// own cap, and only the global cap is under pressure.
//
// h1's chunk is inserted first but touched last, so plain FIFO would evict it
// while LRU keeps it. The untouched h2 chunk, one step fresher than h1's at the
// moment of the touch, is the least recently used and must be the one to go.
func TestGlobalEvictionEvictsAcrossHandles(t *testing.T) {
	// One handle may hold two chunks, but the process may hold only two.
	c := newChunkCache(8, 8)

	c.put(1, 0, []byte("aaaa"))
	c.put(2, 0, []byte("bbbb"))
	// Touch h1's so h2's becomes the least recently used overall. Insertion
	// order now disagrees with recency: h1/0 is the oldest insertion and the
	// newest access.
	if _, ok := c.get(1, 0); !ok {
		t.Fatal("h1's chunk should be present before the touch")
	}

	// h3 is a new handle at home in its own cap, so evictOpen has nothing to do
	// for it; the global pass is the only one that can act.
	c.put(3, 0, []byte("cccc"))

	if _, ok := c.get(2, 0); ok {
		t.Error("h2's chunk was the least recently used and should have been " +
			"evicted across handles by the global cap")
	}
	if _, ok := c.get(1, 0); !ok {
		t.Error("h1's chunk was used most recently and must survive, even though " +
			"it was inserted first: that is what distinguishes LRU from FIFO")
	}
	if _, ok := c.get(3, 0); !ok {
		t.Error("the chunk just written must be present")
	}
	if c.total() > 8 {
		t.Errorf("total = %d, want it bounded by the global cap 8", c.total())
	}
}

// A handle's own cap is enforced before the global cap, so adding a chunk that
// puts a handle over its own limit evicts that handle's own oldest chunk rather
// than someone else's. The order of the two passes is the reason: evictOpen runs
// first, so by the time evictGlobal looks, the written handle is already back
// inside its own cap.
//
// This is not incidental. If the passes ran the other way, a handle over its own
// cap would push the eviction onto an unrelated handle and then still be over its
// cap, so its own excess would survive in the cache and the per-open bound would
// be violated by exactly the amount that escaped into someone else's budget.
func TestPerOpenCapIsEnforcedBeforeTheGlobalCap(t *testing.T) {
	// Room for two chunks per handle, four across the process.
	c := newChunkCache(8, 16)

	c.put(1, 0, []byte("aaaa"))
	c.put(1, 1, []byte("bbbb")) // h1 at its cap, nothing evicted
	if _, ok := c.get(1, 1); !ok {
		t.Fatal("h1's second chunk should be present before the touch")
	}
	// Touch h1/0 so it becomes the most recently used and h1/1 the candidate,
	// making the eviction below decided by recency rather than by insertion
	// order -- which is the point, since that is what the cache is for.
	if _, ok := c.get(1, 0); !ok {
		t.Fatal("h1's first chunk should be present before the touch")
	}

	// The third chunk puts h1 over its cap, and the process is still under the
	// global cap, so only the per-open pass can act -- and it must act on h1's
	// own least recently used chunk, which is h1/1 after the touch above.
	c.put(1, 2, []byte("cccc"))

	if _, ok := c.get(1, 1); ok {
		t.Error("h1's own least recently used chunk should have been evicted by " +
			"its own cap, without touching any other handle's budget")
	}
	if _, ok := c.get(1, 0); !ok {
		t.Error("h1's recently touched chunk must survive")
	}
	if _, ok := c.get(1, 2); !ok {
		t.Error("the chunk just written must be present")
	}
	if got := c.openTotal(1); got > 8 {
		t.Errorf("h1 total = %d, want at most its cap 8", got)
	}
}

// A chunk exactly filling the budget is still cacheable. The oversized guard
// compares against the global cap, and an off-by-one there would refuse a
// legitimate chunk rather than break the bound, which is a silent waste.
func TestChunkExactlyFillingTheBudgetIsCached(t *testing.T) {
	c := newChunkCache(1<<20, 4)
	c.put(1, 0, []byte("aaaa"))
	if _, ok := c.get(1, 0); !ok {
		t.Error("a chunk exactly equal to the global cap must be cached")
	}
	if c.total() > 4 {
		t.Errorf("total = %d, want at most 4", c.total())
	}
}

// The per-open cap must be enforced on the handle being written and must not
// touch another handle's bytes, even when the total is far under the global cap.
func TestPerOpenEvictionLeavesOtherHandlesAlone(t *testing.T) {
	c := newChunkCache(4, 1<<20)
	c.put(2, 0, []byte("keep"))

	for i := int64(0); i < 10; i++ {
		c.put(1, i, []byte("xxxx"))
	}

	if c.openTotal(1) > 4 {
		t.Errorf("handle 1 total = %d, want at most 4", c.openTotal(1))
	}
	if c.openTotal(2) != 4 {
		t.Errorf("handle 2 total = %d, want its 4 bytes untouched", c.openTotal(2))
	}
}

// dropOpen must leave nothing behind, including the byte accounting, so that a
// released handle cannot keep occupying the global budget invisibly.
func TestDropOpenLeavesNoAccounting(t *testing.T) {
	c := newChunkCache(1<<20, 1<<20)
	c.put(1, 0, []byte("aaaa"))
	c.put(1, 1, []byte("bbbb"))
	c.dropOpen(1)

	if got := c.openTotal(1); got != 0 {
		t.Errorf("openTotal after drop = %d, want 0", got)
	}
	if got := c.total(); got != 0 {
		t.Errorf("total after dropping the only handle = %d, want 0", got)
	}
	if c.order.Len() != 0 {
		t.Errorf("eviction queue still holds %d entries after drop", c.order.Len())
	}
	// Dropping a handle that was never used must not panic or create an entry.
	c.dropOpen(99)
	if c.order.Len() != 0 {
		t.Errorf("dropping an unknown handle changed the queue to %d entries", c.order.Len())
	}
}

// A put for a key already held is ignored, so the stored bytes are not replaced.
// The read path relies on this: it may re-put a chunk it just fetched.
func TestReputKeepsTheExistingChunk(t *testing.T) {
	c := newChunkCache(1<<20, 1<<20)
	c.put(1, 0, []byte("first"))
	c.put(1, 0, []byte("second"))

	got, ok := c.get(1, 0)
	if !ok {
		t.Fatal("the chunk disappeared after a re-put")
	}
	if string(got) != "first" {
		t.Errorf("re-put replaced the chunk with %q; chunks are immutable", got)
	}
	if c.openTotal(1) != len("first") {
		t.Errorf("re-put double-counted: openTotal = %d, want %d", c.openTotal(1), len("first"))
	}
}
