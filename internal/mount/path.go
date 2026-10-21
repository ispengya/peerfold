package mount

import (
	"log"
	"path"
	"strings"
	"time"

	"github.com/ispengya/peerfold/internal/node"
)

// dirCacheEntry is one cached directory listing.
//
// byName is what makes a lookup O(1): lookup resolves a path by asking its
// parent's listing, and scanning that listing for every Getattr turned a
// directory of N entries into O(N) work per entry, or O(N^2) for the kernel's
// walk of the whole directory. fetched is when the wire answered; each entry
// expires on the listing clock rather than the kernel's attribute clock,
// because Explorer refreshes attributes far more often than a directory
// actually changes.
type dirCacheEntry struct {
	children []node.Node
	byName   map[string]node.Node
	fetched  time.Time
}

// listingCall is one in-flight List for a directory. Concurrent callers that
// miss the same cache entry wait on done instead of each sending the whole
// directory over the wire.
type listingCall struct {
	done     chan struct{}
	children []node.Node
	err      error
}

// maxDirCacheEntries bounds how many directories are remembered. The entries
// themselves are bounded by the tree, but a mount pointed at a huge federation
// should not accumulate one slice per directory it ever listed.
const maxDirCacheEntries = 4096

// listingTTL is how long a directory listing may answer lookups.
//
// It is independent of EntryTimeout because the two describe different things:
// the kernel's entry cache is expired by Explorer's refresh loop every few
// seconds, while the listing itself changes only when a path inside the
// directory changes -- and events invalidate it immediately when that happens.
// A short listing TTL therefore bought no freshness and cost one whole-directory
// transfer every few seconds.
func (m *Mount) listingTTL() time.Duration {
	if m.opts.ListingTimeout > 0 {
		return m.opts.ListingTimeout
	}
	return 30 * time.Second
}

// listDir returns dir's children, reusing a recent listing.
//
// lookup() resolves paths by listing the parent. Without this cache every
// Getattr and Open was a whole-directory transfer over the wire, so a directory
// the kernel walked entry by entry produced one listing per entry. The cached
// copy is returned, never the one held under the lock, because Readdir sorts
// the slice it is given and two readers must not share one backing array.
//
// A miss is also collapsed: two concurrent Readdirs of the same directory wait
// for one wire answer rather than sending two. The waiter reports cached=true
// because it did not pay a round trip of its own.
func (m *Mount) listDir(dir string) (children []node.Node, cached bool, err error) {
	ttl := m.listingTTL()

	m.listingMu.Lock()
	if e, ok := m.listings[dir]; ok && time.Since(e.fetched) < ttl {
		children = append([]node.Node(nil), e.children...)
		m.listingMu.Unlock()
		log.Printf("mount: list %s entries=%d took=0s cached=true", dir, len(children))
		return children, true, nil
	}
	if call, ok := m.listingInflight[dir]; ok {
		m.listingMu.Unlock()
		<-call.done
		if call.err != nil {
			return nil, false, call.err
		}
		return append([]node.Node(nil), call.children...), true, nil
	}
	call := &listingCall{done: make(chan struct{})}
	if m.listingInflight == nil {
		m.listingInflight = map[string]*listingCall{}
	}
	m.listingInflight[dir] = call
	m.listingMu.Unlock()

	started := time.Now()
	fetched, err := m.call().List(dir)
	took := time.Since(started)
	if err == nil {
		// The wire's paths are index paths ("win/Documents"), while everything in
		// this package keys on FUSE paths ("/win/Documents"). The adapter could do
		// this, but doing it here makes every Caller obey the same convention: the
		// event table, pending table and handed-out table are all keyed by the
		// result, so a relative path in any one of them is a path that never matches.
		byName := make(map[string]node.Node, len(fetched))
		for i := range fetched {
			fetched[i].Path = fusePath(fetched[i].Path)
			byName[path.Base(fetched[i].Path)] = fetched[i]
		}
		fetchedCopy := append([]node.Node(nil), fetched...)
		call.children = fetchedCopy

		m.listingMu.Lock()
		if m.listings == nil {
			m.listings = map[string]dirCacheEntry{}
		}
		if len(m.listings) >= maxDirCacheEntries {
			// Evict the oldest entry. This runs only when the cache is full, so the
			// scan is off the hot path; it keeps the bound without an LRU list.
			var oldest string
			var oldestAt time.Time
			for p, e := range m.listings {
				if oldest == "" || e.fetched.Before(oldestAt) {
					oldest, oldestAt = p, e.fetched
				}
			}
			delete(m.listings, oldest)
		}
		m.listings[dir] = dirCacheEntry{
			children: fetchedCopy,
			byName:   byName,
			fetched:  time.Now(),
		}
		m.listingMu.Unlock()
		log.Printf("mount: list %s entries=%d took=%s cached=false", dir, len(fetched), took)
	} else {
		call.err = err
		log.Printf("mount: list %s took=%s failed: %v", dir, took, err)
	}

	m.listingMu.Lock()
	delete(m.listingInflight, dir)
	close(call.done)
	m.listingMu.Unlock()

	if err != nil {
		return nil, false, err
	}
	return append([]node.Node(nil), fetched...), false, nil
}

// lookupChild resolves one name inside a directory using the cached listing's
// name map.
//
// The first lookup after a miss pays the listing fill and then scans the copy
// it was given; every later lookup is a map hit. This is what keeps a directory
// of thousands of entries from being scanned once per entry.
func (m *Mount) lookupChild(dir, name string) (node.Node, bool, error) {
	m.listingMu.Lock()
	if e, ok := m.listings[dir]; ok && time.Since(e.fetched) < m.listingTTL() {
		n, found := e.byName[name]
		m.listingMu.Unlock()
		return n, found, nil
	}
	m.listingMu.Unlock()

	children, _, err := m.listDir(dir)
	if err != nil {
		return node.Node{}, false, err
	}
	for _, c := range children {
		if path.Base(c.Path) == name {
			return c, true, nil
		}
	}
	return node.Node{}, false, nil
}

// invalidateListings drops the listing for path and for its parent.
//
// A change to path alters both: the directory it is in (membership, size) and,
// if it is a directory, its own children. Called from watch events and from
// every local mutation, so a listing that answers a lookup is never older than
// the mount's own knowledge of a change.
func (m *Mount) invalidateListings(path string) {
	if path == "" {
		return
	}
	m.listingMu.Lock()
	delete(m.listings, path)
	if parent, _, err := SplitParent(path); err == nil {
		delete(m.listings, parent)
	}
	m.listingMu.Unlock()
}

// applyListingEvents folds one event batch into the directory cache.
//
// It patches the cached parent listing instead of discarding it. A build that
// touches one file in a twenty-thousand-entry directory used to throw that
// directory away and refetch and decode it on the next read; now the entry is
// replaced in place, which is O(1) per changed name and leaves every untouched
// entry serving lookups.
//
// Deletions are different: a tombstone for a directory makes its whole cached
// subtree meaningless, so that subtree is dropped. The kernel is still told to
// revalidate the paths separately; this is only the mount's own listing cache.
func (m *Mount) applyListingEvents(changed []node.Node, subtrees []string) {
	if len(changed) == 0 && len(subtrees) == 0 {
		return
	}
	ttl := m.listingTTL()

	m.listingMu.Lock()
	defer m.listingMu.Unlock()

	deleted := make(map[string]struct{}, len(subtrees))
	for _, p := range subtrees {
		deleted[p] = struct{}{}
	}
	if _, all := deleted["/"]; all {
		m.listings = map[string]dirCacheEntry{}
		return
	}

	for _, p := range subtrees {
		m.dropListingSubtreeLocked(p)
		m.removeListingChildLocked(p)
	}
	for _, n := range changed {
		if n.Path == "" || n.Path == "/" {
			continue
		}
		parent, base, err := SplitParent(n.Path)
		if err != nil {
			continue
		}
		e, ok := m.listings[parent]
		if !ok || time.Since(e.fetched) >= ttl {
			// Nothing fresh to patch. An expired entry keeps its age, so the next
			// lookup still refetches the whole directory rather than trusting
			// everything else in a stale list.
			continue
		}
		m.upsertListingChildLocked(&e, base, n)
		e.fetched = time.Now()
		m.listings[parent] = e
	}
}

// upsertListingChildLocked replaces or appends one entry in a cached listing.
func (m *Mount) upsertListingChildLocked(e *dirCacheEntry, base string, n node.Node) {
	e.byName[base] = n
	for i := range e.children {
		if e.children[i].Path == n.Path {
			e.children[i] = n
			return
		}
	}
	e.children = append(e.children, n)
}

// removeListingChildLocked removes one name from its parent's cached listing.
func (m *Mount) removeListingChildLocked(p string) {
	parent, base, err := SplitParent(p)
	if err != nil {
		return
	}
	e, ok := m.listings[parent]
	if !ok {
		return
	}
	delete(e.byName, base)
	kept := e.children[:0]
	for _, c := range e.children {
		if c.Path != p {
			kept = append(kept, c)
		}
	}
	e.children = kept
	e.fetched = time.Now()
	m.listings[parent] = e
}

// dropListingSubtreeLocked forgets the cached listing of path and everything
// beneath it.
func (m *Mount) dropListingSubtreeLocked(path string) {
	for key := range m.listings {
		if key == path || strings.HasPrefix(key, path+"/") || path == "/" {
			delete(m.listings, key)
		}
	}
}

// fusePath puts a path returned by the caller into the FUSE form this package
// uses everywhere else.
//
// The viewer speaks index paths: no leading slash, and a client name as the
// first component. The core speaks FUSE paths: absolute, and with "/" as the
// root. Treating one as the other is silent rather than loud -- an event for
// "win/x" simply never matches a lookup for "/win/x" -- so the conversion is
// done at the one place results enter the core rather than trusted to the
// adapter.
func fusePath(p string) string {
	if p == "" {
		return "/"
	}
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// isRootChild reports whether p names a client at the mount root.
//
// With the leading slash now present, a plain Contains(p, "/") would be true for
// every absolute path. The question is whether there is a second component.
func isRootChild(p string) bool {
	p = strings.TrimPrefix(p, "/")
	return p != "" && !strings.Contains(p, "/")
}

// deletedAncestor reports whether a tombstone on an ancestor shadows p.
//
// A tombstone is kept for the deleted directory, not copied onto every path
// below it. A cached listing under that directory can still answer a lookup, and
// a path whose parent is known to be gone must not be brought back by it.
func (m *Mount) deletedAncestor(p string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletedAncestorLocked(p)
}

// deletedAncestorLocked is deletedAncestor for a caller that already holds
// Mount.mu.
func (m *Mount) deletedAncestorLocked(p string) bool {
	for p != "" && p != "/" {
		parent, _, err := SplitParent(p)
		if err != nil {
			return false
		}
		if ev, ok := m.events[parent]; ok && ev.Kind == node.KindDeleted {
			return true
		}
		p = parent
	}
	return false
}

// clearListings forgets every cached listing. A reconnect replaces the whole
// tree, so an entry from the old connection must not answer for the new one.
func (m *Mount) clearListings() {
	m.listingMu.Lock()
	m.listings = map[string]dirCacheEntry{}
	m.listingMu.Unlock()
}

// Clean normalises a FUSE path and refuses anything that would escape the
// mount root. FUSE hands us absolute slash-separated paths; the caller of this
// function is the last place that can notice a traversal attempt, because
// every layer below treats the path as opaque.
//
// The empty path means the root, because FUSE's own getattr on the mount point
// arrives as the empty string in some versions and as "/" in others, and
// treating one of those as an error would break every mount that used it.
func Clean(p string) (string, error) {
	if p == "" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", errStr("mount: path must be absolute: " + p)
	}
	cleaned := path.Clean(p)
	// path.Clean("/../etc") == "/etc", which already refuses to climb past the
	// root, but it does so silently. Reject the attempt instead of quietly
	// serving a different path: a caller that asked for ".." is either buggy
	// or hostile, and neither should be answered with a plausible-looking file.
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", errStr("mount: path escapes the root: " + p)
		}
	}
	return cleaned, nil
}

// SplitParent returns the parent directory and the base name. The root has no
// parent and yields errRoot.
//
// The two ends are returned together rather than derived twice at each call
// site, because a base name computed from a different string than the parent is
// the classic way a path check gets bypassed.
func SplitParent(p string) (parent, base string, err error) {
	if p == "/" || p == "" {
		return "", "", errRoot
	}
	parent = path.Dir(p)
	base = path.Base(p)
	if parent == "" {
		parent = "/"
	}
	return parent, base, nil
}

// lookup resolves a path to the index entry for it. It returns (node, 0) on
// success and (zero, -errno) on failure, so callers can return the code
// straight to cgofuse.
//
// Two different failures look identical when a name is absent from its parent:
// the name may simply not exist (ENOENT), or the parent may be a file rather
// than a directory (ENOTDIR). Callers branch on which one it is — a shell
// reports them differently, and so does every program that creates a file and
// then asks why it failed — so the two are told apart deliberately.
//
// Telling them apart costs one List of the parent, and the distinction comes
// entirely from that call's error, so it is worth stating what that error
// actually means rather than assuming. index.Children, measured, reports
// ErrNotDir only when the node at the queried path is itself a file, and
// ErrNotFound in every other case. It never returns an empty list for a path
// with no node, and — this is the part that is narrower than POSIX — it does
// not walk the path's ancestors: Children("a/file.txt/x") is ErrNotFound even
// though file.txt is a file, because the question it answers is "is there a
// node at this path, and is that node a directory".
//
// Two consequences follow, and both are pinned by tests. A name missing from a
// listable parent is ENOENT. A path whose immediate parent is a file is
// ENOTDIR. A file further up the path is ENOENT, and reporting ENOTDIR there
// would be inventing behaviour the frozen v1 core cannot produce. ENOENT rather
// than ENOTDIR is the safe direction in any case: "mkdir -p /a/b" proceeds on
// it, while ENOTDIR makes a caller stop.
//
// Keeping this at one List also keeps a lookup at O(1) round trips rather than
// O(depth). Every List is a round trip to the owner, so a component-by-component
// descent would trade this for one round trip per component on a deep path,
// which is the wrong trade for a stat that a shell issues before every command.
//
// There is no whole-tree cache here on purpose. The index is the federation's
// own view, and a second copy of it in this process would need its own
// invalidation, which is the machinery D5 exists to bound. What the kernel
// caches is tracked separately and narrowly: see the inode table in watch.go.
//
// One narrow exception, and its bound is the point: a path that a *push* has
// named is answered from that push rather than from a listing. That is not
// caching the tree, it is recording what this mount was told, and it is what
// keeps a listing from contradicting a change that has already been applied. The
// table holds exactly the paths that were named by an event -- never a directory
// listing -- so a cold path still costs the same one List, and a walk of a
// directory it has never heard about is unaffected.
//
// A disconnected mount answers EIO here rather than refusing each caller
// separately, because almost every operation resolves a path before it does
// anything else: Getattr, Readdir, Open, Opendir, Read, Create, Mkdir, Unlink,
// Rmdir, Rename and the path form of Truncate all arrive through this function.
// The exceptions are the ones that must still work with no connection -- Release
// has to free its handle, and the flush table has to be able to answer -- and
// those are guarded where they actually do damage.
func (m *Mount) lookup(p string) (node.Node, int) {
	cleaned, err := Clean(p)
	if err != nil {
		return node.Node{}, -EINVAL
	}
	if m.offline() {
		// EIO and never ENOENT, and before the root case as well as before the
		// caller. A program that sees ENOENT concludes the file was deleted and
		// may act destructively on that conclusion -- dropping its own copy, or
		// writing a new one over the owner's. EIO makes it retry, which is the
		// correct response to a connection that is coming back.
		//
		// The root is refused too. It costs nothing to synthesise, but a mount
		// that answers *some* paths while the connection is gone forces every
		// program above it to invent a rule for which answers to trust -- and the
		// rule it would invent is "a successful getattr means the file is there",
		// which is false for the root and true for nothing else. Refusing
		// everything is a rule a caller can act on. The root's attributes are
		// refreshed along with every other path once the supervisor reconnects.
		//
		// The check is also before the event table, deliberately: a path the last
		// connection named is exactly the kind of stale answer a caller must not
		// act on while the tree is known to be wrong.
		return node.Node{}, -EIO
	}
	if cleaned == "/" {
		// The root is synthesised, not listed: no client owns it and the
		// server has no entry for it, so listing its parent is impossible.
		// Its mode comes from the options because there is nothing to read.
		return node.Node{Path: "/", Kind: node.KindDir, Mode: m.opts.RootMode}, 0
	}
	if n, ok := m.eventFor(cleaned); ok {
		if n.Kind == node.KindDeleted {
			// The tombstone shadows whatever a listing would say. A listing is
			// answered from the caller's index, which for this path is older
			// than the push that removed it, so the push wins.
			return node.Node{}, -ENOENT
		}
		if n.Kind == node.KindSymlink {
			return node.Node{}, -ELOOP
		}
		return n, 0
	}
	if m.deletedAncestor(cleaned) {
		// The directory that would contain this path is known to be gone. A
		// cached listing under it must not resurrect the name.
		return node.Node{}, -ENOENT
	}
	if n, ok := m.pendingNode(cleaned); ok {
		// This name was created by this mount and the owning client has not
		// reported it back yet. The index does not know it, but Windows is
		// already using it: a fresh Mkdir or Create is revalidated by the
		// kernel before the client's watcher has had a chance to push the new
		// node to the server. Answering ENOENT here makes the first child
		// create in a just-made directory fail with a path that looks missing,
		// even though the caller just made it. The index stays authoritative
		// everywhere else; this covers only the window the mount itself owns.
		//
		// The parent still has to resolve, for the same reason a parent has to
		// resolve everywhere else: the index answers ENOENT for a path whose
		// parent does not exist, and a pending name must not override that.
		if parent, _, err := SplitParent(cleaned); err == nil {
			if _, code := m.lookup(parent); code != 0 {
				if code == -ENOTDIR {
					// A component of the parent path is a file, so this path
					// sits beneath a file. The index answers ENOENT for that,
					// because it inspects only the node at each path; the two
					// answers describe the same situation, and ENOTDIR is the
					// one that tells the caller what is actually wrong.
					code = -ENOENT
				}
				return node.Node{}, code
			}
		}
		return n, 0
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return node.Node{}, -EINVAL
	}
	child, found, err := m.lookupChild(parent, base)
	if err != nil {
		// The parent could not be listed, and that error is the diagnosis, so
		// it is passed through rather than flattened. See the comment above for
		// what the layer below does and does not distinguish.
		return node.Node{}, errnoFor(err)
	}
	if found {
		if child.Kind == node.KindSymlink {
			// A symlink's stat and its read disagree: the index records the
			// link itself, while the owner opens the path and the operating
			// system follows it. The two answers cannot both be right, and a
			// caller that stat'ed a small link and then read a large target
			// would corrupt its own buffers. Refusing is the only answer that
			// cannot corrupt anything (D12).
			//
			// This deliberately does not distinguish a POSIX symlink from a
			// Windows junction or reparse point. The client classifies all of
			// them as KindSymlink, and on Windows a junction is both more
			// common and more dangerous: following one can leave the shared
			// tree entirely.
			return node.Node{}, -ELOOP
		}
		return child, 0
	}
	// The name is not among the parent's children, and the parent listed
	// successfully, so the parent is a real directory and the name is simply
	// absent.
	return node.Node{}, -ENOENT
}

// pendingNode reports whether this path is a name the mount itself created but
// whose owning client has not pushed back yet, and what it is.
//
// It is deliberately separate from pendingCreate/pendingDirectory in ops.go:
// those exist for the adapter's immediate post-create callbacks and are keyed
// on "a handle is open here"; this one is about the *path*, and is consulted by
// lookup, which every resolving operation shares. A name is a directory if it
// was made with Mkdir and a file if it was made with Create; the two are not
// both recorded, so the answer cannot be ambiguous.
func (m *Mount) pendingNode(cleaned string) (node.Node, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kind, ok := m.pendingPaths[cleaned]
	if !ok {
		return node.Node{}, false
	}
	return node.Node{Path: cleaned, Kind: kind}, true
}

// childNamed finds the child whose base name is name.
//
// The comparison is on the base name rather than the whole path because List
// returns full paths, and on a whole name rather than a suffix so that a file
// called "ab" cannot answer for "b". The first match wins; a directory cannot
// hold two entries with the same name, so there is never a second.
func childNamed(children []node.Node, name string) (node.Node, bool) {
	for _, c := range children {
		if path.Base(c.Path) == name {
			return c, true
		}
	}
	return node.Node{}, false
}

// lookupParent returns the parent directory node and the base name, which is
// what every operation that creates, removes or renames something needs: the
// parent has to exist and be a directory, and the base name is the name that
// must be free (or present, for remove and rename).
//
// It refuses the root for the same reason SplitParent does: the root has no
// parent, so there is nothing to create it in or delete it from.
func (m *Mount) lookupParent(p string) (node.Node, string, int) {
	cleaned, err := Clean(p)
	if err != nil {
		return node.Node{}, "", -EINVAL
	}
	parent, base, err := SplitParent(cleaned)
	if err != nil {
		return node.Node{}, "", -EINVAL
	}
	n, code := m.lookup(parent)
	if code != 0 {
		return node.Node{}, "", code
	}
	if n.Kind != node.KindDir {
		// A component of the parent path is a file. Reported as ENOTDIR rather
		// than ENOENT because the caller's mistake is about the shape of the
		// path, not about something being missing.
		return node.Node{}, "", -ENOTDIR
	}
	return n, base, 0
}
