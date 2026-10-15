package mount

import (
	"log"

	"peerfold/internal/node"
)

// startWatch begins consuming the caller's event channel, applying each batch to
// this mount's event table.
//
// It is idempotent, because the reconnect path in Task 11 can reach it more than
// once and a second consumer would apply every event twice.
//
// The loop is the reason the core needs the mount's lock at all for the event
// table: cgofuse calls the filesystem's methods from several threads while this
// goroutine applies pushes, so both touch the table and both take m.mu. Nothing
// here calls the caller, which is what keeps the lock free of wire waits.
func (m *Mount) startWatch() {
	m.watchOnce.Do(func() {
		m.mu.Lock()
		done := make(chan struct{})
		m.watchDone = done
		m.mu.Unlock()
		go m.watchLoop(done)
	})
}

// stopWatch stops the loop, if one is running. It is safe to call before
// startWatch and safe to call twice.
//
// It does not close the caller's channel: that channel is the caller's, and
// closing it from here would be this layer reaching into the transport's
// lifetime. The loop leaves by its own door.
func (m *Mount) stopWatch() {
	m.mu.Lock()
	done := m.watchDone
	m.watchDone = nil
	m.mu.Unlock()
	if done != nil {
		close(done)
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
func (m *Mount) watchLoop(done chan struct{}) {
	ch := m.caller.Watch()
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
			// dropped. This is the case the select cannot decide on its own.
			select {
			case <-done:
				return
			default:
			}
			m.applyEvent(b)
		}
	}
}

// applyEvent merges one batch into the event table and checks the sequence.
//
// Noticing a gap does not discard the batch. The batch is newer information than
// anything else on hand, so applying it is strictly better than dropping it --
// the stale flag records that something was missed, not that what arrived is
// suspect.
func (m *Mount) applyEvent(b Batch) {
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
		}
		m.lastSeq = b.Seq
	}

	for _, n := range b.Nodes {
		if n.Kind == node.KindDeleted {
			// The tombstone is kept rather than the entry being deleted: see the
			// comment on Mount.events. Deleting it would make the deletion
			// indistinguishable from a path that was never mentioned, and the
			// next listing would bring it back.
			m.events[n.Path] = node.Node{Path: n.Path, Kind: node.KindDeleted}
			continue
		}
		m.events[n.Path] = n
	}
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
func (m *Mount) noteInode(_ uint64, p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
