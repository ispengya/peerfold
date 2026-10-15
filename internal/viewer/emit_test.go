package viewer

import (
	"net"
	"sync"
	"testing"
	"time"

	"peerfold/internal/node"
)

// TestEmitAndCloseCannotMeetOnAClosedChannel pins the one thing that has to be
// atomic about publishing a batch.
//
// emit has to decide two things — is there a channel, and is it still open — and
// it has to decide them where Close decides them, because the send is what
// panics when they disagree. The decision used to be split: emit read the
// channel under the lock and then sent to it after releasing the lock, so a
// Close landing in that window closed the channel underneath the send. That is a
// panic in a background reader, which takes the process down rather than failing
// a call.
//
// This drives the two against each other in a tight loop, which is what makes
// the window reachable at all: a push arriving on its own schedule almost never
// lands in it. Run with -race it also reports the unsynchronised access directly.
func TestEmitAndCloseCannotMeetOnAClosedChannel(t *testing.T) {
	// Close ends the socket as well as the channel, so there has to be a socket.
	// The other end is never read; closing this end is all the test needs.
	conn, peer := net.Pipe()
	defer peer.Close()
	v := &Viewer{conn: conn, events: make(chan []node.Node, 1<<20)}

	// The channel is oversized on purpose so that every publish reaches the send
	// rather than being absorbed by the default arm; with the send taken every
	// time, a close landing after the check and before the send is what panics.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range v.events {
		}
	}()

	var wg sync.WaitGroup

	// One goroutine publishes continuously. The other closes in the middle of it.
	// There is deliberately no barrier between them: the two must be in flight at
	// the same time for the window to exist at all, so the closer does not wait
	// for the publisher to finish — it waits only for the publisher to be running.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200000; i++ {
			v.emit([]node.Node{{Path: "win/Documents/x.txt"}})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Let the publisher get going, then close underneath it. A fixed wait is
		// enough because the publisher does not stop until well past it.
		time.Sleep(time.Millisecond)
		if err := v.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	wg.Wait()

	// A second close must not close the channel again.
	if err := v.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	<-done

	// Publishing after Close is a no-op, not a send on a closed channel.
	v.emit([]node.Node{{Path: "win/Documents/z.txt"}})
}
