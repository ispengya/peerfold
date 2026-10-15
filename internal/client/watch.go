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

	"github.com/fsnotify/fsnotify"

	"peerfold/internal/node"
	"peerfold/internal/proto"
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
	return Watch(c.shares, func(nodes []node.Node) error {
		body, err := json.Marshal(proto.EventBody{Nodes: toNodeJSON(nodes)})
		if err != nil {
			return fmt.Errorf("client: encode event: %w", err)
		}
		// Sent without a waiter, because an index update is not a question and
		// the server sends nothing back for it. Registering a waiter would leave
		// one entry in the waiting map for every change, for ever.
		if _, err := c.send(proto.KindEvent, body, false); err != nil {
			return err
		}
		return nil
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
		sh:    sh,
		emit:  emit,
		fsw:   inner,
		done:  make(chan struct{}),
		roots: make([]watchRoot, 0, len(sh.names)),
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

// emitTree reports every entry under dir, one node at a time.
//
// It is for a directory that has just appeared, whose own event cannot describe
// what is already inside it. Excluded subtrees are skipped without being
// descended into, exactly as the scanner skips them, so the watcher and the
// scan agree on what the tree contains.
func (w *watch) emitTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == dir {
			// The caller reports the directory itself, with the event's own
			// information.
			return nil
		}
		if w.sh.Excluded(path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, ok := w.relPath(path)
		if !ok {
			return nil
		}
		if d.IsDir() {
			w.send([]node.Node{{Path: rel, Kind: node.KindDir}})
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		w.send([]node.Node{{
			Path:  rel,
			Kind:  node.KindFile,
			Size:  info.Size(),
			Mode:  uint32(info.Mode().Perm()),
			Mtime: info.ModTime().UnixNano(),
		}})
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
		w.send([]node.Node{{Path: rel, Kind: node.KindDeleted}})
		return
	}

	info, err := os.Lstat(ev.Name)
	if err != nil {
		// The event names something that is already gone, so it is a removal
		// that arrived through a create. Treat it as one.
		if os.IsNotExist(err) {
			w.send([]node.Node{{Path: rel, Kind: node.KindDeleted}})
		}
		return
	}
	// A symlink is reported as whatever it points at, or not at all: the index
	// holds real entries, and a link is not one.
	if info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.IsDir() {
		if ev.Op&fsnotify.Create != 0 {
			// A new directory is watched from now on, or the next file created
			// inside it is never noticed.
			w.addTree(ev.Name)
			// It is also read, and that is not belt-and-braces. Adding a watch
			// takes effect from the moment it lands, so anything created
			// between the directory appearing and the watch being registered
			// produces events nobody was listening for. That window is not
			// theoretical: creating a directory and filling it is one operation
			// to the code that does it — `MkdirAll`, an unpacking tool, a copy —
			// and on Windows the whole thing can land inside the window. The
			// directory's own events would then be complete and its contents
			// would be missing for as long as the connection lasts.
			//
			// Reading the subtree closes the window rather than narrowing it,
			// and the entries it produces are the same nodes the missed events
			// would have carried. Emitting them twice is harmless: an index
			// entry is written by path, so the same node applied again is the
			// same state.
			w.emitTree(ev.Name)
		}
		w.send([]node.Node{{Path: rel, Kind: node.KindDir, Mode: uint32(info.Mode().Perm())}})
		return
	}
	if !info.Mode().IsRegular() {
		return
	}

	w.send([]node.Node{{
		Path:  rel,
		Kind:  node.KindFile,
		Size:  info.Size(),
		Mode:  uint32(info.Mode().Perm()),
		Mtime: info.ModTime().UnixNano(),
	}})
}

// send hands a batch to emit, ignoring a failure.
//
// A failed emit is not retried and is not fatal: the client keeps running, and
// the connection is still serving reads and writes. What it is not is repaired.
// There is no periodic rescan in v1 — the index is seeded once by the upload and
// then only ever moved by these events — so a batch that fails here leaves the
// server's tree describing the state before that change, and the change stays
// invisible until something else touches the same path or the client reconnects.
// The same is true of an fsnotify overflow, where the kernel drops events because
// the buffer filled.
//
// That is a real limitation rather than a safety net, and it is written down in
// docs/peerfold-cli.md. The fix is a periodic Scan-and-diff, which this version
// does not have.
func (w *watch) send(nodes []node.Node) {
	if len(nodes) == 0 {
		return
	}
	_ = w.emit(nodes)
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
