package mount

import (
	"log"
	"path"
	"strings"

	"github.com/ispengya/peerfold/internal/node"
)

// startWatch begins consuming the caller's event channel, applying each batch to
// this mount's event table.
//
// It is idempotent: a second call while a loop is already running does nothing, so
// a path that starts the watch twice does not get two consumers applying every
// event twice. It is deliberately *not* guarded by a sync.Once, because a
// reconnect has to be able to start a second loop -- on the new caller's channel,
// after stopWatch has retired the first -- and a Once by definition never runs
// again. The nil check is the idempotence.
//
// The loop is the reason the core needs the mount's lock at all for the event
// table: cgofuse calls the filesystem's methods from several threads while this
// goroutine applies pushes, so both touch the table and both take m.mu. Nothing
// here holds that lock across a channel receive.
func (m *Mount) startWatch() {
	m.mu.Lock()
	if m.watchDone != nil {
		m.mu.Unlock()
		return
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	m.watchDone = done
	m.watchExited = exited
	m.mu.Unlock()
	go m.watchLoop(done, exited)
}

// stopWatch stops the loop if one is running, and waits for it to finish. It is
// safe to call before startWatch and safe to call twice.
//
// Waiting is not tidiness. A reconnect replaces the caller, and a loop still
// running when that happened would go on receiving batches from the *old*
// caller's channel and merging them into a table that is now meant to describe
// the new one. Making the old loop finish before the swap removes the race
// outright; the loop is watched from the supervisor's own goroutine, so the wait
// is on a loop that only ever exits.
//
// It does not close the caller's channel: that channel is the caller's, and
// closing it from here would be this layer reaching into the transport's
// lifetime. The loop leaves by its own door.
func (m *Mount) stopWatch() {
	m.mu.Lock()
	done := m.watchDone
	exited := m.watchExited
	m.watchDone = nil
	m.watchExited = nil
	m.mu.Unlock()
	if done == nil {
		return
	}
	close(done)
	if exited != nil {
		<-exited
	}
}

// watchLoop consumes batches until the channel closes or the mount is stopped.
//
// The loop re-checks done after every receive rather than relying on the select
// alone. A select whose done case and receive case are both ready picks one at
// random, and the receive case is ready for as long as the caller's channel holds
// a batch, so a loop that trusted the select would sometimes apply one more batch
// after stopWatch had returned. Once the mount has been stopped its view must not
// move again, so the check is what makes that a guarantee rather than a
// likelihood.
//
// There is deliberately no check here that the caller has not been replaced. Such
// a check looks like the obvious guard against a batch from the dead connection,
// and it used to be one -- but it is unreachable, and the mutation run is what
// proved it. stopWatch waits for this function to return before adopt replaces the
// caller, so by the time the caller can differ the loop is already gone; and any
// batch the loop did apply on the way out is wiped by adopt's clear, which happens
// after the wait. Both are needed for the guarantee, and the wait is the one that
// carries it. Keeping the guard would be keeping a branch neither reachable nor
// tested, and a reader would have to reconstruct this argument to find that out.
//
// exited is closed on every return, including this function's own closes. It is
// how stopWatch knows the loop it stopped has actually stopped, which is what
// makes the argument above hold rather than being merely probable.
func (m *Mount) watchLoop(done chan struct{}, exited chan struct{}) {
	defer close(exited)
	ch := m.call().Watch()
	for {
		select {
		case <-done:
			return
		case b, ok := <-ch:
			if !ok {
				// The channel is closed, which is the transport saying there
				// will be no more updates. From here the tree drifts, and the
				// drift is unbounded in time, so it is reported rather than
				// silently tolerated: Task 11 decides whether to reconnect.
				m.setDisconnected(true)
				log.Printf("mount: the event stream ended; the tree is stale " +
					"until a reconnect")
				return
			}
			// A batch that was already in flight when the mount was stopped is
			// dropped. This is the case the select cannot decide on its own, and
			// with adopt's wait it is the only case left: by the time the caller
			// has been replaced, this loop has returned.
			select {
			case <-done:
				return
			default:
			}
			if m.applyEvent(b) {
				if m.anyOwnerRemains() {
					// One owner's tree is gone, but another owner remains. The
					// server is still reachable and still has something to serve, so
					// the mount stays up and keeps watching. The withdrawn subtree is
					// already shadowed by the tombstone in the event table, and the
					// kernel's entry timeout revalidates the rest on its own.
					log.Printf("mount: an owner's tree was withdrawn; serving the " +
						"remaining owners")
					continue
				}
				// The last owner's tree is gone. A reconnect would succeed against
				// the still-live server and leave the mount up with nothing to serve,
				// so the response is to release the mount point rather than retry. It
				// is deliberately not setDisconnected, which is the signal the
				// reconnect supervisor polls for.
				log.Printf("mount: the last owner's tree was withdrawn; the mount " +
					"point is being released")
				m.releaseMountPoint()
				return
			}
		}
	}
}

// applyEvent merges one batch into the event table and checks the sequence.
//
// Noticing a gap does not discard the batch. The batch is newer information than
// anything else on hand, so applying it is strictly better than dropping it --
// the stale flag records that something was missed, not that what arrived is
// suspect.
func (m *Mount) applyEvent(b Batch) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Seq == 0 means the sender does not number its events, which is not a gap.
	//
	// The first numbered batch is adopted rather than compared: a mount that has
	// just attached has no idea what the previous sequence was, and inventing a
	// gap for the low order bits of a reconnect's counter would mark every fresh
	// mount stale.
	if b.Seq != 0 {
		if m.lastSeq != 0 && b.Seq != m.lastSeq+1 {
			// A gap means the transport dropped events, and nothing else in the
			// system reports it: a drop is not an error at any layer below, it is
			// the designed behaviour of a non-blocking emit.
			log.Printf("mount: the event sequence jumped from %d to %d, so "+
				"events were lost and the tree cannot be trusted", m.lastSeq, b.Seq)
			m.isStale = true
			// A lost batch may have changed any directory. The per-path
			// invalidation below cannot name what was missed, so every cached
			// listing is suspect until it is fetched again.
			m.clearListings()
		}
		m.lastSeq = b.Seq
	}

	withdrawn := false
	for _, n := range b.Nodes {
		// The path and its parent both changed as far as listings are
		// concerned: membership, attributes, or (for a directory) children.
		m.invalidateListings(n.Path)
		delete(m.pendingPaths, n.Path)
		delete(m.pendingContent, n.Path)
		if n.Kind == node.KindDeleted {
			// The tombstone is kept rather than the entry being deleted: see the
			// comment on Mount.events. Deleting it would make the deletion
			// indistinguishable from a path that was never mentioned, and the
			// next listing would bring it back.
			m.events[n.Path] = node.Node{Path: n.Path, Kind: node.KindDeleted}
			if n.Path != "" && !strings.Contains(n.Path, "/") {
				withdrawn = true
			}
			continue
		}
		m.events[n.Path] = n
	}
	return withdrawn
}

// anyOwnerRemains reports whether the federation still has a live owner to
// serve. It lists the root and discards any name this mount has already been told
// is gone, so the answer is right even while the server is still removing the
// withdrawn owner from its own index. It is how the watch loop decides, when one
// owner's tree is withdrawn, whether that owner was the last.
//
// A list that fails is treated as "owners remain": a transient wire error is not
// evidence the tree is empty, and if the server really is gone the watch channel
// closes and the reconnect path takes over.
func (m *Mount) anyOwnerRemains() bool {
	children, err := m.call().List("/")
	if err != nil {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range children {
		name := path.Base(c.Path)
		if ev, ok := m.events[name]; ok && ev.Kind == node.KindDeleted {
			continue
		}
		return true
	}
	return false
}

// eventFor returns this mount's most recent information about a path, if any.
//
// The second return is false when nothing has been heard about the path, which is
// not the same as hearing that it is gone: a tombstone is present and reports
// KindDeleted.
func (m *Mount) eventFor(p string) (node.Node, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.events[p]
	return n, ok
}

// stale reports whether an event was lost, so the event table can no longer be
// trusted. Task 11 owns what happens next.
func (m *Mount) stale() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isStale
}

// setStale is how the reconnect path records that it has refreshed the tree, or
// that a fresher gap has been seen.
func (m *Mount) setStale(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isStale = v
}

// disconnected reports whether the watch channel ended.
func (m *Mount) disconnected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isDisconnected
}

func (m *Mount) setDisconnected(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.isDisconnected = v
}

// noteInode records that a path's attributes were handed to the kernel, so a
// later invalidation has something to walk.
//
// A path that is already recorded is a no-op apart from the eviction check, and
// the table is keyed by path rather than by handle: invalidation walks paths, and
// the same path handed out through five handles is one path to invalidate.
func (m *Mount) noteInode(fh uint64, p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noteInodeLocked(fh, p)
}

// noteInodeLocked is noteInode for a caller that already holds m.mu.
//
// A separate method rather than an unlocked function with a different name,
// because the two are the same decision: the lock is not part of what it records,
// and a caller that took it by accident should get a name that says so rather than
// a deadlock.
func (m *Mount) noteInodeLocked(_ uint64, p string) {
	m.handed[p] = struct{}{}
	if len(m.handed) <= m.opts.InvalidateTable {
		return
	}
	// Drop entries until the table is back inside its cap. Which ones is
	// arbitrary -- Go's map iteration order is unspecified -- and that is
	// acceptable because the table is a best-effort hint: an entry that is
	// dropped is one the kernel may keep a stale attribute for until its
	// attr_timeout expires, which is the bound the table exists to trade against
	// unbounded memory.
	for k := range m.handed {
		delete(m.handed, k)
		if len(m.handed) <= m.opts.InvalidateTable {
			return
		}
	}
}

// forgotInode drops a path from the handed-out table when nothing can still be
// caching it.
//
// It is deliberately not called per handle release: the kernel can hold an
// attribute for a path after every handle on it is closed, so an entry is only
// dropped by eviction or by a refresh that has invalidated it.
func (m *Mount) forgotInode(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.handed, p)
}

// handedOut snapshots the paths the kernel may be caching, for invalidation to
// walk.
func (m *Mount) handedOut() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.handed))
	for p := range m.handed {
		out = append(out, p)
	}
	return out
}
