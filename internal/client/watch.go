package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/ispengya/peerfold/internal/logging"
	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

// Watch starts reporting filesystem changes under this client's shares to the
// server, which folds them into the aggregated index and pushes them on to every
// watcher.
//
// It is the difference between a tree that is a snapshot and one that is live:
// the upload answers "what is there now" once, and this answers "what just
// changed" from then on.
//
// The returned Closer stops the watching. Closing the client also stops it. A
// change that could not be sent is dropped rather than retried: the connection
// is either up, in which case the next change gets through, or down, in which
// case nothing will.
func (c *Client) Watch() (io.Closer, error) {
	// seq numbers the batches this client sends. It advances on every attempted
	// send rather than every successful one, so a batch that fails to send leaves
	// a hole: the next batch carries the number after the gap. Nothing below
	// retries or reports a failed push -- a change that could not be sent is
	// dropped by design -- so the hole is the only trace it leaves, and without it
	// the aggregated tree would describe the state before that change with no way
	// for anyone to know.
	var seq uint64
	return Watch(c.shares, func(nodes []node.Node) error {
		body, err := json.Marshal(proto.EventBody{Seq: seq, Nodes: toNodeJSON(nodes)})
		if err != nil {
			return fmt.Errorf("client: encode event: %w", err)
		}
		seq++
		// One line per batch, not per path: a build's batch can carry hundreds
		// of nodes, and printing every path turns a diagnostic into a flood.
		logging.Infof("client: push %d node(s) seq=%d", len(nodes), seq-1)
		// Sent without a waiter, because an index update is not a question and
		// the server sends nothing back for it. Registering a waiter would leave
		// one entry in the waiting map for every change, for ever.
		_, err = c.send(proto.KindEvent, body, false)
		return err
	})
}

// Watch reports changes under every share as they happen, in batches.
//
// It is what makes the aggregated tree live rather than a snapshot taken at
// connection time: the scanner answers "what is there now" once, and this
// answers "what just changed" for as long as it runs.
//
// emit is called with a batch of nodes covering one or more filesystem events.
// A changed path is reported with its current kind, size and mtime; a path that
// has gone is reported with node.KindDeleted, which is an instruction rather
// than a kind any filesystem can report.
//
// The returned Closer stops the watcher and releases its handles. Closing it
// twice is safe, and emit is not called after Close returns.
func Watch(sh Shares, emit func(events []node.Node) error) (io.Closer, error) {
	if emit == nil {
		return nil, errors.New("client: watch needs something to emit to")
	}
	inner, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("client: watch: %w", err)
	}

	w := &watch{
		sh:      sh,
		emit:    emit,
		fsw:     inner,
		done:    make(chan struct{}),
		pending: map[string]node.Node{},
		roots:   make([]watchRoot, 0, len(sh.names)),
	}
	// The roots are collected longest-first once, at start: a path is mapped
	// back to the share that contains it by the first root that matches, and
	// with shares nested inside each other the shortest root would otherwise
	// claim a path that belongs to a longer one.
	for _, name := range sh.Names() {
		root, ok := sh.Root(name)
		if !ok {
			continue
		}
		// A share that is itself excluded is not watched at all. Watching it
		// and filtering its events afterwards would be the same result at
		// higher cost, and would leave the exclusion's meaning spread across
		// two places.
		if sh.Excluded(root) {
			continue
		}
		w.roots = append(w.roots, watchRoot{name: name, root: root})
		w.addTree(root)
	}
	sort.Slice(w.roots, func(i, j int) bool {
		return len(w.roots[i].root) > len(w.roots[j].root)
	})

	go w.run()
	return w, nil
}

// watchRoot pairs a share name with the absolute directory behind it.
type watchRoot struct {
	name string
	root string
}

// eventBatchWindow and maxEventBatch bound how long events are coalesced and
// how many distinct paths one push may carry.
//
// A build that creates twenty thousand files is twenty thousand fsnotify
// events. Sent one per frame, they cost twenty thousand index updates, twenty
// thousand broadcasts, and a watcher channel overflow that makes the mount
// discard every cached listing. Coalescing a short window turns the same work
// into tens of batches, and deduplicating by path means a file that is created,
// written and closed is reported once with its final size.
//
// The window is short enough that an interactive change is still pushed in a
// blink, and the count bound keeps one batch far below the wire's frame limit.
const (
	eventBatchWindow = 50 * time.Millisecond
	maxEventBatch    = 512
)

// watch turns filesystem events into node updates.
type watch struct {
	sh   Shares
	emit func([]node.Node) error
	fsw  *fsnotify.Watcher

	mu     sync.Mutex
	closed bool
	// handles is how many directories are being watched, so a test or a
	// diagnostic can see the recursion actually happened.
	handles int

	// pending is the latest node per path in the current batch, and order
	// records every path as it changed. On flush, walking order backwards and
	// keeping the first occurrence of each path gives one node per path in
	// last-change order, which is what keeps a delete that follows a create
	// from being applied before it.
	//
	// Both are touched only by the watcher's own goroutine.
	pending map[string]node.Node
	order   []string
	timer   *time.Timer
	timerC  <-chan time.Time

	roots []watchRoot
	done  chan struct{}
	once  sync.Once
}

// Close stops the watcher and releases its handles. It is idempotent.
func (w *watch) Close() error {
	w.once.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		close(w.done)
	})
	return w.fsw.Close()
}

// addTree registers a directory and everything beneath it.
//
// Recursion is not optional. fsnotify watches one directory at a time and does
// not descend, so a watcher that added only the share roots would see a new
// subdirectory appear and then never see anything created inside it — the
// events for its contents would simply never arrive, and the tree would be
// silently wrong from then on.
func (w *watch) addTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that cannot be read is skipped rather than failing
			// the whole walk. A watcher that refused to start because one
			// subtree was unreadable would lose every other subtree with it.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if w.sh.Excluded(path) {
			// An excluded subtree is not descended into: the exclusions exist
			// so that something inside a share — a mount of the aggregated tree
			// itself, most of all — is never reported, and walking into one
			// would defeat that.
			return filepath.SkipDir
		}
		w.add(path)
		return nil
	})
}

// add registers one directory, ignoring a repeat.
func (w *watch) add(dir string) {
	if err := w.fsw.Add(dir); err != nil {
		// A directory can vanish between the walk and the Add. That is a race
		// with the filesystem, not a failure of the watcher: the event that
		// removed it either already arrived or is about to.
		return
	}
	w.mu.Lock()
	w.handles++
	w.mu.Unlock()
}

// addAndEmitTree watches and reports every entry under a directory that has
// just appeared.
//
// One walk does both jobs. The watcher used to add watches with one recursive
// walk and then report the contents with a second one, so creating a directory
// containing a thousand files walked those thousand entries twice — on the
// event path, while the build that made them was still writing.
//
// The directory itself is not reported here: the caller does that with the
// event's own information.
func (w *watch) addAndEmitTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if w.sh.Excluded(path) {
			// An excluded subtree is not descended into, exactly as Scan skips
			// it, so the watcher and the scan agree on what the tree contains.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// The new directory and every directory under it need their own
			// watch, or events inside them are never delivered.
			w.add(path)
		}
		if path == dir {
			return nil
		}
		rel, ok := w.relPath(path)
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// These nodes stand in for events that were missed, so they have to
		// match what Scan would have produced for the same entries. They used
		// to leave Mode unset on directories and drop anything that was not a
		// regular file, which meant a subtree read after a new directory
		// appeared disagreed with the same subtree read by the initial scan:
		// a directory arrived with mode 0, and a symlink or a Windows junction
		// vanished from a tree the scan had put it in.
		kind := classify(info.Mode(), d.IsDir())
		n := node.Node{
			Path:  rel,
			Kind:  kind,
			Mode:  uint32(info.Mode()),
			Mtime: info.ModTime().UnixNano(),
		}
		if kind == node.KindFile {
			n.Size = info.Size()
		}
		w.enqueue([]node.Node{n})
		return nil
	})
}

// run is the watcher's only reader of the fsnotify channels.
func (w *watch) run() {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case <-w.timerC:
			// The coalescing window closed. timerC is nil unless a batch is
			// open, so a quiet watcher parks on the fsnotify channels alone.
			w.flush()
		case _, ok := <-w.fsw.Errors:
			// An overflow or a watch that could not be established arrives here.
			// It is not fatal to the watcher, and there is nothing this code can
			// do about it: the affected subtree simply stops reporting until
			// something recreates it. The connection keeps running.
			if !ok {
				return
			}
		}
	}
}

// handle turns one filesystem event into an update and emits it.
func (w *watch) handle(ev fsnotify.Event) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	if w.sh.Excluded(ev.Name) {
		return
	}
	rel, ok := w.relPath(ev.Name)
	if !ok {
		return
	}

	// A removed path is reported as an instruction to drop it, and nothing
	// about it can be read: it is gone, and the size and mtime a stat would
	// return belong to whatever comes next at that name, if anything.
	if ev.Op&fsnotify.Remove != 0 || ev.Op&fsnotify.Rename != 0 {
		w.enqueue([]node.Node{{Path: rel, Kind: node.KindDeleted}})
		return
	}

	info, err := os.Lstat(ev.Name)
	if err != nil {
		// The event names something that is already gone, so it is a removal
		// that arrived through a create. Treat it as one.
		if os.IsNotExist(err) {
			w.enqueue([]node.Node{{Path: rel, Kind: node.KindDeleted}})
		}
		return
	}
	// A symlink or a reparse point (a Windows junction) gets a node of its own
	// rather than being dropped. It used to return here, which meant a link
	// the scan had indexed could never be updated again for the life of the
	// connection: the index kept saying KindFile with the length of the target
	// string while the thing on disk changed.
	//
	// classify is the same function Scan uses, so the two paths cannot
	// disagree about what a given entry is.
	kind := classify(info.Mode(), info.IsDir())
	if kind != node.KindFile && kind != node.KindDir {
		w.enqueue([]node.Node{{
			Path:  rel,
			Kind:  kind,
			Mode:  uint32(info.Mode()),
			Mtime: info.ModTime().UnixNano(),
		}})
		return
	}
	if info.IsDir() {
		if ev.Op&fsnotify.Create != 0 {
			// Watch the new directory and report what is already inside it.
			//
			// Adding a watch takes effect from the moment it lands, so anything
			// created between the directory appearing and the watch being
			// registered produces events nobody was listening for. That window
			// is not theoretical: creating a directory and filling it is one
			// operation to the code that does it — `MkdirAll`, an unpacking
			// tool, a copy — and on Windows the whole thing can land inside the
			// window. Reading the subtree closes it rather than narrowing it,
			// and the entries it produces are the same nodes the missed events
			// would have carried.
			w.addAndEmitTree(ev.Name)
		}
		w.enqueue([]node.Node{{Path: rel, Kind: node.KindDir, Mode: uint32(info.Mode())}})
		return
	}
	if !info.Mode().IsRegular() {
		// classify already sends anything with a type bit as a link, so this
		// is a guard rather than a decision. Say so instead of returning
		// silently: a change that reaches here is one the index will not learn
		// about, and silence is how the old symlink bug stayed hidden.
		return
	}

	w.enqueue([]node.Node{{
		Path:  rel,
		Kind:  node.KindFile,
		Size:  info.Size(),
		Mode:  uint32(info.Mode()),
		Mtime: info.ModTime().UnixNano(),
	}})
}

// enqueue adds nodes to the open batch, starting its window if this is the
// first one and flushing immediately if the count bound is reached.
//
// Only the watcher's goroutine calls this, so the pending map needs no lock.
// Coalescing by path is not just an optimisation: a create followed by a write
// and a stat produces several events for one file, and applying them
// separately made the index and every watcher do that work several times.
func (w *watch) enqueue(nodes []node.Node) {
	if len(nodes) == 0 {
		return
	}
	for _, n := range nodes {
		w.pending[n.Path] = n
		w.order = append(w.order, n.Path)
	}
	if w.timerC == nil {
		if w.timer == nil {
			w.timer = time.NewTimer(eventBatchWindow)
		} else {
			w.timer.Reset(eventBatchWindow)
		}
		w.timerC = w.timer.C
	}
	if len(w.pending) >= maxEventBatch {
		w.flush()
	}
}

// flush sends the open batch, in last-change order and with one node per path.
//
// Walking order backwards and keeping the first occurrence of each path is what
// gives "latest state, latest position": a path that was created and then
// deleted appears once, at the position of the delete, so a watcher applies the
// operations in an order that ends in the truth.
func (w *watch) flush() {
	if w.timer != nil && w.timerC != nil {
		if !w.timer.Stop() {
			select {
			case <-w.timer.C:
			default:
			}
		}
	}
	w.timerC = nil
	if len(w.pending) == 0 {
		w.order = w.order[:0]
		return
	}

	seen := make(map[string]struct{}, len(w.pending))
	reverse := make([]node.Node, 0, len(w.pending))
	for i := len(w.order) - 1; i >= 0; i-- {
		p := w.order[i]
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		reverse = append(reverse, w.pending[p])
	}
	nodes := make([]node.Node, len(reverse))
	for i := range reverse {
		nodes[len(reverse)-1-i] = reverse[i]
	}
	clear(w.pending)
	w.order = w.order[:0]
	nodes = dropUnderDeleted(nodes)

	// A failed emit is not retried and is not fatal: the client keeps running,
	// and the connection is still serving reads and writes. What it is not is
	// repaired. There is no periodic rescan in v1 — the index is seeded once by
	// the upload and then only ever moved by these events — so a batch that
	// fails here leaves the server's tree describing the state before that
	// change. The same is true of an fsnotify overflow, where the kernel drops
	// events because the buffer filled. The mount's sequence-gap detection is
	// what makes a lost send visible instead of silent.
	if len(nodes) > 0 {
		_ = w.emit(nodes)
	}
}

// dropUnderDeleted removes nodes whose ancestor is deleted in the same batch.
//
// A directory deletion takes its whole subtree with it. If a stale create for a
// child arrives after the directory's tombstone in the same window, applying
// both would leave the child in the index under a directory that is gone. The
// tombstone already says everything about the subtree, so the child's node is
// noise.
func dropUnderDeleted(nodes []node.Node) []node.Node {
	byPath := make(map[string]node.Node, len(nodes))
	for _, n := range nodes {
		byPath[n.Path] = n
	}
	out := nodes[:0]
	for _, n := range nodes {
		p := n.Path
		shadowed := false
		for {
			i := strings.LastIndexByte(p, '/')
			if i < 0 {
				break
			}
			p = p[:i]
			if anc, ok := byPath[p]; ok && anc.Kind == node.KindDeleted {
				shadowed = true
				break
			}
		}
		if !shadowed {
			out = append(out, n)
		}
	}
	return out
}

// relPath maps an absolute path back to its "<share>/<rel>" form, and reports
// whether it belonged to any share.
//
// The mapping is by prefix, and the roots are sorted longest-first when the
// watcher starts, so a share nested inside another resolves to the inner one.
func (w *watch) relPath(abs string) (string, bool) {
	abs = filepath.Clean(abs)
	for _, r := range w.roots {
		if !node.IsUnder(r.root, abs) {
			continue
		}
		rel, err := filepath.Rel(r.root, abs)
		if err != nil {
			continue
		}
		if rel == "." {
			return r.name, true
		}
		// The index speaks slash-separated paths on every platform, so the
		// separator a Windows Rel returns is translated here rather than being
		// left for whoever compares paths next.
		return r.name + "/" + strings.ReplaceAll(rel, string(filepath.Separator), "/"), true
	}
	return "", false
}
