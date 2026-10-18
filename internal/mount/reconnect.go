package mount

import (
	"log"
	"time"
)

// Reconnector dials the federation again and returns a fresh caller for it.
//
// It is a function rather than an interface because the mount needs exactly one
// thing from it -- try again -- and the retry schedule, the backoff and the
// give-up decision all belong to the mount, not to whoever does the dialling. The
// host adapter supplies a closure over the client and the viewer.
//
// It must not return a nil Caller with a nil error: the mount would then install
// a nil caller and every later operation would panic. A failed dial returns an
// error.
type Reconnector func() (Caller, error)

// offline reports whether the connection is known to be gone.
//
// It is what makes every operation answer EIO instead of blocking or inventing an
// answer. It is a momentary state, not a terminal one: the supervisor clears it as
// soon as it has a new caller, and the point of clearing it is that the very next
// operation goes to the fresh connection.
func (m *Mount) offline() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isDisconnected
}

// ReconnectOptions is the schedule snapshot supervise works from.
//
// It is copied under the lock once, rather than read from the options on every
// iteration: the options are read by the supervisor's goroutine and would be a
// data race to touch concurrently, and a test that changes the schedule after the
// supervisor has started should not be able to change it mid-flight. A schedule
// that can move under the thing following it is not a schedule.
type ReconnectOptions struct {
	Min    time.Duration
	Max    time.Duration
	GiveUp time.Duration
}

// schedule snapshots the reconnect options.
func (m *Mount) schedule() ReconnectOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ReconnectOptions{
		Min:    m.opts.ReconnectMin,
		Max:    m.opts.ReconnectMax,
		GiveUp: m.opts.GiveUpAfter,
	}
}

// supervise retries the connection until it comes back, and gives up in the end.
//
// The backoff starts at Min and doubles to Max, so a peer that is down for a
// moment is retried promptly and a peer that is gone for an hour is not retried a
// thousand times: by the time the schedule reaches Max, an outage of an hour costs
// about 120 attempts rather than 3600.
//
// On success it installs the new caller and returns. On failure it keeps trying
// until GiveUpAfter has passed, and then unmounts: a mount whose owner is gone
// must stop being a mount, because leaving it up means every program that touches
// the path blocks until its own timeout, repeatedly, with no end.
//
// The wait is capped by the give-up deadline as well as by the backoff, so the
// supervisor never sleeps past the moment it decided to stop.
func (m *Mount) supervise(rc Reconnector, giveUp time.Duration) {
	if giveUp <= 0 {
		giveUp = m.schedule().GiveUp
	}
	deadline := time.Now().Add(giveUp)
	wait := m.schedule().Min
	if wait <= 0 {
		wait = time.Millisecond
	}
	log.Printf("mount: the supervisor will retry the connection for %v", giveUp)

	for {
		c, err := rc()
		if err == nil && c != nil {
			m.adopt(c)
			return
		}
		if err == nil {
			// A nil caller with no error is a broken dialler, not a transient
			// outage. It is reported as a failure rather than installed, because
			// installing it would turn every later operation into a nil-pointer
			// panic somewhere far from the cause.
			err = errStr("mount: the reconnect function returned no client and no error")
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			m.gaveUp(err)
			return
		}
		sleep := wait
		if sleep > remaining {
			sleep = remaining
		}
		if sleep > 0 {
			time.Sleep(sleep)
		}
		wait *= 2
		max := m.schedule().Max
		if max <= 0 {
			max = wait
		}
		if wait > max {
			wait = max
		}
	}
}

// releaseMountPoint asks the host to unmount, through the hook the runner
// installs. It is the single place any give-up becomes an actual unmount, whether
// the reason is "the server never came back" or "the server withdrew the owner's
// tree". A nil hook is tolerated because a Mount built by a test has no host.
func (m *Mount) releaseMountPoint() {
	m.mu.Lock()
	hook := m.onGiveUp
	m.mu.Unlock()
	if hook != nil {
		hook()
		return
	}
	// A Mount built by a test has no host, and a Runner that never wired the
	// hook is the same shape. Without this the give-up is a log line and the
	// mount point stays mounted, which is the failure this whole path exists
	// to prevent.
	log.Printf("mount: nothing is installed to release the mount point; it will " +
		"stay mounted until this process exits")
}

// gaveUp unmounts the mount after the reconnect budget ran out, saying what the
// last failure was.
func (m *Mount) gaveUp(last error) {
	log.Printf("mount: giving up after %v of reconnect attempts; the last failure "+
		"was %v. The mount point is being released", m.schedule().GiveUp, last)
	m.releaseMountPoint()
}

// adopt installs a reconnected caller and makes the mount describe it.
//
// Three things have to happen together, and the order matters:
//
//  1. The old watch loop is stopped *and waited for*, before the caller is
//     replaced. A loop still running would go on merging batches from the old
//     connection's channel into a table that now describes the new one.
//
//  2. The event table is cleared and its sequence counter reset. The table holds
//     paths the old connection named, and lastSeq is the old connection's counter:
//     a new connection numbers its events from wherever it likes, so keeping the
//     old value would read the first batch of the new stream as a gap. That the
//     new counter starts low is not a reason to keep the old one -- it is the
//     reason to drop it.
//
//  3. Dirty handles are discarded, and every path handed to the kernel is
//     invalidated. The discard is the hard part and it is honest rather than
//     clever: the buffer holds content the owner never received, and replaying it
//     over whatever the owner's copy became while the connection was down would
//     destroy data with no way to recover it. Each discarded path is reported so
//     the loss is at least visible.
//
// The refresh is the point of reconnecting at all. Events were missed while
// disconnected, so the tree cannot be assumed unchanged; the kernel has to be told
// to forget what it cached and ask again.
func (m *Mount) adopt(c Caller) {
	// Before the swap: this is the loop reading the caller that is about to be
	// replaced.
	m.stopWatch()

	m.mu.Lock()
	m.caller = c
	m.isDisconnected = false
	// The new connection has thrown away everything the old one told us. The
	// table goes first, so a lookup racing this call cannot see a path from the
	// old connection described as if it came from the new one.
	for k := range m.events {
		delete(m.events, k)
	}
	m.lastSeq = 0
	m.mu.Unlock()

	m.discardDirtyOnDisconnect()
	m.invalidateAll()
	m.startWatch()
}

// discardDirtyOnDisconnect drops every dirty handle and reports each path.
//
// The report is not optional. The write that filled the buffer reported success,
// the mount is still there, and the content is now gone; a discard with nothing
// in the log is indistinguishable from a save that worked. The hook is nil when
// no host adapter is installed, which is the case for a Mount built by a test,
// and the discard still happens.
//
// The buffers are zeroed rather than left to the garbage collector, and each
// entry is deleted rather than the map being replaced: a replacement would be one
// assignment, but a concurrent write to the old map would then be lost with no
// trace at all, while delete is safe under the lock this holds.
func (m *Mount) discardDirtyOnDisconnect() {
	m.mu.Lock()
	paths := make([]string, 0, len(m.files))
	for _, h := range m.files {
		if !h.isClean() {
			paths = append(paths, h.path)
			h.version++
			h.dirtyVersion = h.version
			h.dirty = false
			buf := h.content()
			for i := range buf {
				buf[i] = 0
			}
			delete(m.files, h.fh)
			m.cache.dropOpenLocked(h.fh)
		}
	}
	hook := m.onDiscard
	m.mu.Unlock()

	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if _, once := seen[p]; once {
			continue
		}
		seen[p] = struct{}{}
		if hook != nil {
			hook(p)
		}
	}
	if len(paths) > 0 {
		log.Printf("mount: the connection was lost with %d unflushed file(s); "+
			"their unsaved contents were discarded and the owners' copies are "+
			"unchanged", len(paths))
	}
}

// invalidateAll tells the kernel to forget the attributes of every path it may be
// caching.
//
// FUSE has no whole-filesystem invalidate, so the only way to do this is to walk
// the paths that were handed out and ask per path. The table is bounded by
// InvalidateTable, and what falls out of it is covered by attr_timeout instead --
// a path the kernel was never told about, or one evicted from the table, keeps its
// stale attribute until that timeout expires, which is the bound the table trades
// against unbounded memory.
//
// The hook is nil before a host adapter is installed. The table is still walked
// rather than skipped, so the loop and its bound are exercised by the tests.
func (m *Mount) invalidateAll() {
	paths := m.handedOut()
	hook := m.invalidateHook()
	refused := 0
	for _, p := range paths {
		if hook != nil && !hook(p) {
			refused++
		}
	}
	// A refusal means the kernel may go on serving the cached attributes this
	// call exists to remove. It is logged rather than returned because there is
	// no caller to return it to, and it is logged as a total rather than per path
	// because a reconnect invalidates thousands of paths at once.
	if refused > 0 {
		log.Printf("mount: the kernel refused %d of %d attribute invalidations; "+
			"those paths may stay stale until their cache times out",
			refused, len(paths))
	}
}

// invalidateHook reads the invalidate callback under the lock.
func (m *Mount) invalidateHook() func(string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.invalidatePath
}
