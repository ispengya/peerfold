// The runner: what an operator actually starts.
//
// This file is the portable half. It knows how to turn a Caller and an Options
// into a live mount and how to shut one down, and it deliberately does not know
// what a mount point is -- that is the one thing the two platforms disagree
// about, and it lives in runner_windows.go and runner_linux.go.
package mount

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"time"
)

// Runner holds everything one mount needs and runs it.
//
// The fields are exported because a runner is the seam between a command-line
// front end and this package: main.go fills it in from flags, and a test fills it
// in directly.
type Runner struct {
	// Mount is the directory to mount on. Its must-exist-ness is the platform
	// question prepareMountPoint answers.
	Mount string

	// Caller is the connection the mount reads and writes through.
	Caller Caller

	// Opts bounds the buffers and the kernel-cache timeouts.
	Opts Options

	// Reconnect dials a new connection after the current one dies. A nil one
	// means the mount does not try: the first disconnect is final, which is the
	// right behaviour for a caller that cannot redial.
	Reconnect Reconnector
}

// mountArgs builds the FUSE option list.
//
// attr_timeout and entry_timeout are positive on purpose. They are the bound on
// how long the kernel may serve an answer this mount never confirmed -- if the
// whole notification path fails silently, a file changed by a peer stays wrong
// for at most that long, and the alternative (zero) is a getattr round trip on
// every path lookup, which over one connection is worse than being briefly
// stale.
//
// negative_timeout is zero and is not an option. A cached negative lookup is the
// one cache entry that can hide a file that demonstrably exists: a peer creates
// it a moment after somebody's stat missed, and the miss is remembered.
func mountArgs(opts Options) []string {
	secs := func(d time.Duration) string {
		return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
	}
	return []string{
		"-o", "attr_timeout=" + secs(opts.AttrTimeout),
		"-o", "entry_timeout=" + secs(opts.EntryTimeout),
		"-o", "negative_timeout=0",
	}
}

// Run mounts and blocks until the mount goes away, ctx is cancelled, or the
// process is interrupted.
//
// Mount() takes over the calling goroutine and does not return until the
// filesystem is gone, so everything that has to happen during the mount is
// arranged before it is called. That includes the signal handler: SIGINT during a
// mount must flush rather than kill, and a handler installed after Mount() would
// never be reached.
func (r *Runner) Run(ctx context.Context) error {
	if r.Caller == nil {
		return errors.New("mount: a runner needs a caller")
	}
	if r.Mount == "" {
		return errors.New("mount: a runner needs a mount point")
	}

	cleanup, err := prepareMountPoint(r.Mount)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	host, m := NewHost(r.Caller, r.Opts)

	// The backstop. shutdownSave is what actually saves on the signal and
	// cancellation paths; this defer covers the third one, a plain unmount or the
	// owner giving up, where nothing else runs before Mount() returns. The flush
	// is idempotent, so a path that hits both of them finds nothing dirty the
	// second time.
	defer m.flushAllHandles()

	if r.Reconnect != nil {
		// The supervisor unmounts by itself once the owner has been gone for
		// Opts.GiveUpAfter, and that is what the hook is for: this layer does not
		// know what unmounting is, and the core does not want to.
		m.onGiveUp = func() { host.Unmount() }

		// supervise is only meaningful once the connection is actually gone, and
		// there is no event for "it just died" that this layer can subscribe to --
		// the watch loop discovers it and sets the flag. Polling for the flag is
		// therefore how the supervisor starts, and the interval is a compromise:
		// long enough not to spin, short enough that an outage does not add
		// meaningfully to the first reconnect attempt's own backoff.
		go func() {
			for {
				time.Sleep(200 * time.Millisecond)
				if m.disconnected() {
					m.supervise(r.Reconnect, r.Opts.GiveUpAfter)
					return
				}
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}()
	}

	// Interrupt and cancellation both mean "save and stop", not "die". The
	// handler flushes and then unmounts, which makes Mount() return and lets the
	// deferred save above run a second time -- it is idempotent, and the second
	// call finds nothing dirty.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
			log.Printf("mount: interrupted; flushing before unmounting")
		case <-ctx.Done():
			log.Printf("mount: the context was cancelled; flushing before unmounting")
		}
		m.shutdownSave()
		host.Unmount()
	}()

	log.Printf("mount: %s is ready", r.Mount)

	// Mount blocks here until the filesystem goes away. File IO has to come from
	// another process while this is running: a read of the mount point from this
	// process deadlocks, because the request would need this goroutine to serve
	// it.
	if !host.Mount(r.Mount, mountArgs(r.Opts)) {
		return errStr("mount: " + r.Mount + " could not be mounted")
	}
	return nil
}

// shutdownSave flushes every dirty handle and says what it lost.
//
// It is a method rather than an inline call because it is the only thing standing
// between an operator pressing Ctrl-C and losing their last edit, and a piece of
// code that load-bearing has to be reachable from a test. It does NOT unmount:
// the unmount is the caller's business, and keeping the two apart is what lets
// Run's own final save cover the path where no unmount ever happens.
func (m *Mount) shutdownSave() {
	m.flushAllHandles()
}
