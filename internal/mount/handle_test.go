package mount

import (
	"bytes"
	"testing"

	"github.com/ispengya/peerfold/internal/node"
)

func TestHandleAllocationAndLookup(t *testing.T) {
	m := newTestMount(t)
	a := m.newHandleForTest("/a.txt", node.KindFile)
	b := m.newHandleForTest("/b.txt", node.KindFile)
	if a == b {
		t.Fatal("two handles share a number")
	}
	if _, ok := m.handle(a); !ok {
		t.Error("allocated handle not found")
	}
	m.closeHandle(a)
	if _, ok := m.handle(a); ok {
		t.Error("closed handle still found")
	}
	if _, ok := m.handle(b); !ok {
		t.Error("closing one handle disturbed another")
	}
	// A closed handle must not be handed out again while the process runs,
	// because the kernel may still hold the old number.
	if c := m.newHandleForTest("/c.txt", node.KindFile); c == a {
		t.Error("handle number was recycled")
	}
}

func TestWriteInsideAndBeyondCurrentSize(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	if code := h.write(0, []byte("hello")); code != 0 {
		t.Fatalf("write = %d", code)
	}
	if got := string(h.content()); got != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	if !h.dirty {
		t.Error("a write must mark the handle dirty")
	}
	// A write past the end must be zero-filled, not silently joined.
	if code := h.write(8, []byte("!")); code != 0 {
		t.Fatalf("sparse write = %d", code)
	}
	if got := string(h.content()); got != "hello\x00\x00\x00!" {
		t.Errorf("sparse content = %q", got)
	}
}

func TestWriteOverwritesInPlace(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)
	h.write(0, []byte("hello"))
	h.write(1, []byte("EY"))
	if got := string(h.content()); got != "hEYlo" {
		t.Errorf("content = %q, want %q", got, "hEYlo")
	}
}

// A write that would push the buffer past its limit must fail loudly. Silently
// truncating would be "saving quietly loses data", which is the failure this
// whole layer exists to avoid.
func TestWriteOverLimitIsRefused(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 8
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)
	if code := h.write(0, []byte("12345678")); code != 0 {
		t.Fatalf("exactly at the limit should succeed, got %d", code)
	}
	if code := h.write(8, []byte("x")); code != -ENOSPC {
		t.Errorf("write past the limit = %d, want -ENOSPC (%d)", code, -ENOSPC)
	}
	// And the earlier content must be intact.
	if got := string(h.content()); got != "12345678" {
		t.Errorf("content after refusal = %q, want it unchanged", got)
	}
}

func TestTruncateShrinksAndGrows(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)
	h.write(0, []byte("hello world"))
	if code := h.truncateTo(5); code != 0 {
		t.Fatalf("truncate = %d", code)
	}
	if got := string(h.content()); got != "hello" {
		t.Errorf("after shrink = %q, want %q", got, "hello")
	}
	if !h.dirty {
		t.Error("truncate must mark the handle dirty")
	}
	if code := h.truncateTo(8); code != 0 {
		t.Fatalf("grow = %d", code)
	}
	if got := string(h.content()); got != "hello\x00\x00\x00" {
		t.Errorf("after grow = %q", got)
	}
}

func TestSeededSizeSurvivesNoWrites(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)
	h.size = 100
	if h.dirty {
		t.Error("a fresh handle must not be dirty")
	}
	if len(h.content()) != 100 {
		t.Errorf("content length = %d, want 100", len(h.content()))
	}
}

// The limit bounds the bytes a handle holds, not the number of bytes written
// over its life. An editor that rewrites the same region repeatedly writes many
// more bytes than the file contains, and a cumulative count would refuse it --
// reporting ENOSPC on a file that never grew, which is a false failure in the
// middle of an ordinary save.
func TestWriteLimitCountsHeldBytesNotCumulativeBytes(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 8
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	if code := h.write(0, []byte("12345678")); code != 0 {
		t.Fatalf("initial write = %d", code)
	}
	// Rewrite the whole region in two pieces. The file is still 8 bytes and the
	// handle still holds 8 bytes, so both must succeed.
	if code := h.write(0, []byte("abcd")); code != 0 {
		t.Errorf("overwrite first half = %d, want 0: the handle still holds 8 bytes", code)
	}
	if code := h.write(4, []byte("efgh")); code != 0 {
		t.Errorf("overwrite second half = %d, want 0: the handle still holds 8 bytes", code)
	}
	if got := string(h.content()); got != "abcdefgh" {
		t.Errorf("content = %q, want %q", got, "abcdefgh")
	}
	// And the limit still bites when the handle really does grow.
	if code := h.write(8, []byte("x")); code != -ENOSPC {
		t.Errorf("growing past the limit = %d, want -ENOSPC", code)
	}
}

// A write is copied, so a caller reusing its buffer after a write cannot corrupt
// what the handle accumulated. cgofuse hands the same buffer back for every
// read and passes caller-owned slices to Write, so aliasing the argument would
// be a data-corruption bug rather than a performance choice.
func TestWriteCopiesItsArgument(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	buf := []byte("hello")
	h.write(0, buf)
	copy(buf, "XXXXX") // the caller reuses its buffer

	if got := string(h.content()); got != "hello" {
		t.Errorf("content = %q, want %q: the write must copy its argument", got, "hello")
	}
}

// An overlapping write must win over the earlier one for the bytes it covers and
// leave the rest of the earlier run intact. The offsets are the keys of the map,
// so two overlapping runs are both kept and content() applies them in offset
// order -- which gives the right answer only because the later write starts at a
// higher offset. This pins that.
func TestOverlappingWritesComposeByOffset(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	h.write(0, []byte("abcdef"))
	h.write(2, []byte("XY")) // inside the first run
	if got := string(h.content()); got != "abXYef" {
		t.Errorf("content = %q, want %q", got, "abXYef")
	}

	// A run that starts earlier but extends into a later one. Applying in offset
	// order means the later-offset write wins for the overlap, which is what a
	// sequential log of the writes gives.
	h.write(0, []byte("12")) // shorter run at the start
	if got := string(h.content()); got != "12XYef" {
		t.Errorf("content = %q, want %q", got, "12XYef")
	}
}

func TestTruncateRefusesNegative(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)
	if code := h.truncateTo(-1); code != -EINVAL {
		t.Errorf("truncateTo(-1) = %d, want -EINVAL", code)
	}
}

func TestWriteRefusesNegativeOffset(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)
	if code := h.write(-1, []byte("x")); code != -EINVAL {
		t.Errorf("write at a negative offset = %d, want -EINVAL", code)
	}
}

// A refusal must not leave a mark: the offset must not extend the size, and the
// handle must not become dirty. Otherwise a failed write would still change what
// a later flush sends.
func TestRefusedWriteChangesNothing(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 4
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	if code := h.write(0, []byte("abcd")); code != 0 {
		t.Fatalf("the write that fills the limit = %d, want 0", code)
	}
	beforeSize, beforeHeld := h.size, h.heldBytes()

	if code := h.write(100, []byte("x")); code != -ENOSPC {
		t.Fatalf("write past the limit = %d, want -ENOSPC", code)
	}
	if h.size != beforeSize {
		t.Errorf("size = %d after a refused write, want %d", h.size, beforeSize)
	}
	if h.heldBytes() != beforeHeld {
		t.Errorf("held bytes = %d after a refused write, want %d", h.heldBytes(), beforeHeld)
	}
	if _, ok := h.writes[100]; ok {
		t.Error("a refused write must not be recorded")
	}
	if got := string(h.content()); got != "abcd" {
		t.Errorf("content = %q after a refused write, want it unchanged", got)
	}
}

func TestHandleStoresItsPathAndKind(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/中文/文件.txt", node.KindFile)
	h, ok := m.handle(fh)
	if !ok {
		t.Fatal("handle not found")
	}
	if h.path != "/中文/文件.txt" {
		t.Errorf("path = %q", h.path)
	}
	if h.kind != node.KindFile {
		t.Errorf("kind = %v, want KindFile", h.kind)
	}
	if h.fh != fh {
		t.Errorf("handle records fh %d, want %d", h.fh, fh)
	}
}

// The write limit comes from the options at open time, so a handle opened before
// the option was changed keeps the limit it was opened with. Reading the option
// live would mean handles in the same process disagreeing about their own cap.
func TestWriteLimitIsCapturedAtOpenTime(t *testing.T) {
	m := newTestMount(t)
	m.opts.WriteLimit = 4
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	m.opts.WriteLimit = 1 << 20 // raised after the open

	// Within the captured limit, so it must be accepted even though the option
	// now says something else.
	if code := h.write(0, []byte("abcd")); code != 0 {
		t.Errorf("write within the captured limit = %d, want 0", code)
	}
	// Past the captured limit, so it must be refused: the option's new value
	// must not apply to a handle that was already open.
	if code := h.write(4, []byte("e")); code != -ENOSPC {
		t.Errorf("write past the captured limit = %d, want -ENOSPC", code)
	}
	if h.writeLimit != 4 {
		t.Errorf("the handle's limit is %d, want the 4 it opened with", h.writeLimit)
	}
}

// closeHandle must release the handle's cached chunks as well as its table
// entry, or a long session opening and closing files would keep paying for
// chunks nothing can read any more.
func TestCloseHandleReleasesItsCache(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	m.cache.put(fh, 0, []byte("cached"))
	if m.cache.total() == 0 {
		t.Fatal("the cache did not accept the chunk, so this test proves nothing")
	}

	m.closeHandle(fh)

	if got := m.cache.total(); got != 0 {
		t.Errorf("cache total = %d after closing the only handle, want 0", got)
	}
}

// The splice has five branches and hand-written cases cannot be trusted to hit
// them in the combinations that matter, so this compares the whole mechanism
// against a byte slice playing the same writes -- the simplest possible thing
// that is obviously right.
//
// A plain slice is the right reference because it models exactly the semantics
// claimed: a write covers the bytes it addresses, extends the file if it reaches
// past the end, and leaves everything else alone. Any divergence between the two
// is a bug in the interval splicing.
//
// The sequence is fixed rather than seeded randomly so a failure is reproducible,
// and it is long enough and wide enough to make overlap, containment and
// adjacency all occur. writeLimit is left generous so this tests the content
// model rather than the cap.
func TestSpliceMatchesAReferenceByteSlice(t *testing.T) {
	type op struct {
		off  int64
		data string
	}
	// Each round writes a longer run and then a shorter one inside it, then a run
	// that straddles the boundary, then one in the gap. Between rounds the file
	// grows, so later rounds overlap earlier ones.
	ops := []op{
		{0, "abcdef"},
		{2, "XY"},             // inside the first run
		{4, "12"},             // inside the first run, after the previous write
		{0, "P"},              // covers the very start
		{6, "QRSTU"},          // starts at the end of the first run and extends past it
		{1, "mnopq"},          // straddles several earlier runs
		{10, "!"},             // in the sparse region past everything, extending the file
		{8, "zz"},             // inside the gap between the last two
		{0, "ABCDEFGHIJKLMN"}, // one long run covering most of the file
		{15, "w"},             // at the very last byte
	}

	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	var ref []byte
	for i, o := range ops {
		if code := h.write(o.off, []byte(o.data)); code != 0 {
			t.Fatalf("op %d write(%d, %q) = %d", i, o.off, o.data, code)
		}
		// Apply the same write to the reference.
		end := int(o.off) + len(o.data)
		for len(ref) < end {
			ref = append(ref, 0)
		}
		copy(ref[o.off:], o.data)

		if got := h.content(); !bytes.Equal(got, ref) {
			t.Fatalf("after op %d write(%d, %q):\n got %q\nwant %q",
				i, o.off, o.data, got, ref)
		}
		if h.size != int64(len(ref)) {
			t.Fatalf("after op %d: size = %d, want %d", i, h.size, len(ref))
		}
	}

	// The intervals must still be disjoint, which is the invariant content()
	// depends on for being order-independent.
	spans := make([][2]int64, 0, len(h.writes))
	for s, d := range h.writes {
		spans = append(spans, [2]int64{s, s + int64(len(d))})
	}
	for i := range spans {
		for j := range spans {
			if i == j {
				continue
			}
			if spans[i][0] < spans[j][1] && spans[j][0] < spans[i][1] {
				t.Fatalf("intervals overlap: %v and %v", spans[i], spans[j])
			}
		}
	}
	// And the accounting must match what is actually stored. It is not simply the
	// file's length: the operations above leave a hole, and a hole is a region the
	// file reports as zeros without anything having been written there. So the
	// expectation is the number of non-zero bytes, which for this sequence is
	// exactly what the interval map should hold.
	nonZero := 0
	for _, b := range ref {
		if b != 0 {
			nonZero++
		}
	}
	if h.heldBytes() != nonZero {
		t.Errorf("heldBytes = %d, want %d: the intervals should hold exactly the "+
			"bytes written and not since replaced", h.heldBytes(), nonZero)
	}
	if h.size != int64(len(ref)) {
		t.Errorf("size = %d, want %d", h.size, len(ref))
	}
}

// A write that lands entirely inside an existing interval must split it in two
// and preserve both tails. This is the case the plan's per-call-run design got
// wrong, and it is stated as its own test as well as covered by the reference
// comparison above, because it is the exact shape that used to lose bytes.
func TestWriteInsideArunSplitsAndPreservesBothTails(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	h.write(0, []byte("abcdef"))
	if code := h.write(2, []byte("XY")); code != 0 {
		t.Fatalf("inner write = %d", code)
	}
	if got := string(h.content()); got != "abXYef" {
		t.Errorf("content = %q, want %q: both tails of the outer write must survive",
			got, "abXYef")
	}
	if len(h.writes) != 3 {
		t.Errorf("the interval map holds %d intervals, want 3 (head, write, tail)", len(h.writes))
	}
	if h.heldBytes() != 6 {
		t.Errorf("heldBytes = %d, want 6, the file's length", h.heldBytes())
	}
}

// A write that reaches exactly the end of an existing interval is adjacent, not
// overlapping: nothing is overwritten and nothing is lost.
func TestAdjacentWritesDoNotOverwrite(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	h.write(0, []byte("abc"))
	h.write(3, []byte("def"))
	if got := string(h.content()); got != "abcdef" {
		t.Errorf("content = %q, want %q", got, "abcdef")
	}
	if h.heldBytes() != 6 {
		t.Errorf("heldBytes = %d, want 6", h.heldBytes())
	}
}

// Truncating must drop the intervals past the new end and clip the one that
// straddles it, then heldBytes must reflect exactly what is left. Holding bytes
// past the end would let them reappear if the file were grown again, which is
// not what truncation means.
func TestTruncateClipsIntervalsAndAccounting(t *testing.T) {
	m := newTestMount(t)
	fh := m.newHandleForTest("/f", node.KindFile)
	h, _ := m.handle(fh)

	h.write(0, []byte("abcdefghij")) // one interval, 0..10
	h.write(20, []byte("XY"))        // a second, sparse

	if code := h.truncateTo(5); code != 0 {
		t.Fatalf("truncate = %d", code)
	}
	if got := string(h.content()); got != "abcde" {
		t.Errorf("content = %q, want %q", got, "abcde")
	}
	if len(h.writes) != 1 {
		t.Errorf("the interval map holds %d entries, want 1: the sparse run was past the end", len(h.writes))
	}
	if h.heldBytes() != 5 {
		t.Errorf("heldBytes = %d, want 5", h.heldBytes())
	}

	// Growing back must give zeros, not the old bytes.
	if code := h.truncateTo(8); code != 0 {
		t.Fatalf("grow = %d", code)
	}
	if got := string(h.content()); got != "abcde\x00\x00\x00" {
		t.Errorf("content after grow = %q, want zeros where the bytes were dropped", got)
	}
	if h.heldBytes() != 5 {
		t.Errorf("heldBytes = %d after growing back, want 5: growth adds no held bytes", h.heldBytes())
	}
}
