package viewer

import (
	"encoding/json"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

// TestEmitDroppingABatchIsVisibleAsAGap pins the mechanism that makes a dropped
// update detectable at all.
//
// The channel is deliberately non-blocking, so a slow watcher loses updates
// rather than stalling the reader. Every layer below treats that as correct
// behaviour, which means a drop leaves no trace: no error, no counter, nothing a
// caller could ask about. What makes it visible is that the sequence number is
// bumped for every batch that arrives, including the one the channel refuses, so
// the numbers a consumer does receive have a hole in them.
//
// The mutation that matters: bump the counter only on a successful enqueue. That
// is the obvious way to write it. It makes the numbers that get through strictly
// consecutive, so a consumer can never tell a lost batch from a quiet period --
// which is precisely the state this whole mechanism exists to rule out.
func TestEmitDroppingABatchIsVisibleAsAGap(t *testing.T) {
	// A reader is draining, but slowly. Batches 1 and 2 land in the buffer; by
	// the time the reader is allowed to move, the buffer is full and batch 3 is
	// refused. The reader then sees 1, 2, ... and the next one it gets carries 4.
	v := &Viewer{events: make(chan Event, 2)}
	v.emit([]node.Node{{Path: "a"}})
	v.emit([]node.Node{{Path: "b"}})
	v.emit([]node.Node{{Path: "c"}}) // refused: the channel is full
	v.emit([]node.Node{{Path: "d"}}) // refused
	// The reader now drains two, which frees the buffer.
	var got []uint64
	for i := 0; i < 2; i++ {
		got = append(got, (<-v.events).Seq)
	}
	// Two more arrive, and the first of them carries the number after every batch
	// that was offered -- refused ones included.
	v.emit([]node.Node{{Path: "e"}})
	v.emit([]node.Node{{Path: "f"}})
	for i := 0; i < 2; i++ {
		got = append(got, (<-v.events).Seq)
	}

	// 1 and 2 were delivered. 3 and 4 were both refused, so the next two through
	// carry 5 and 6. The number 5 reached the consumer, so the hole is visible in
	// the stream itself: the consumer sees 2 followed by 5 and knows three
	// batches were lost.
	if want := []uint64{1, 2, 5, 6}; !reflect.DeepEqual(got, want) {
		t.Errorf("sequence numbers delivered = %v, want %v: a refused batch must "+
			"still consume a number, or the drop is invisible", got, want)
	}
}

// TestEmitDoesNotNumberPastASuccessfulEnqueue pins the other half: numbering must
// not advance by more than one per batch, or every delivery would look like a
// gap and the signal would be worthless.
func TestEmitDoesNotNumberPastASuccessfulEnqueue(t *testing.T) {
	v := &Viewer{events: make(chan Event, 8)}
	for i := 0; i < 3; i++ {
		v.emit([]node.Node{{Path: "p"}})
	}
	close(v.events)
	var got []uint64
	for e := range v.events {
		got = append(got, e.Seq)
	}
	if want := []uint64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Errorf("sequence numbers delivered = %v, want %v", got, want)
	}
}

// TestPublishEventCarriesTheSendersNumber pins that a numbered push is passed
// through rather than renumbered.
//
// The number is the sender's view of its own stream. If the receiver renumbered
// it, the gaps the sender is reporting -- a change it could not send -- would be
// erased at the first hop, and only drops between here and the mount would ever
// be visible.
func TestPublishEventCarriesTheSendersNumber(t *testing.T) {
	v := &Viewer{events: make(chan Event, 4)}
	body, err := json.Marshal(proto.EventBody{Seq: 7, Nodes: []proto.NodeJSON{{Path: "x"}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	v.publishEvent(proto.Envelope{Body: body})

	select {
	case e := <-v.events:
		if e.Seq != 7 {
			t.Errorf("Seq = %d, want 7: the sender's number was not carried through", e.Seq)
		}
	default:
		t.Fatal("nothing was published")
	}
}

// A push that carries no number is numbered here rather than being passed through
// as zero.
//
// This is the deliberate choice. A zero on the channel would mean "not numbered",
// and a mount reading M events and finding every one of them zero can conclude
// nothing about drops -- so an unnumbered sender's stream would silently disable
// the whole detection. Numbering it here gives the mount a contiguous stream it
// can still check, and the local counter is a true count of what this connection
// received. What is lost is only the ability to see a gap between the sender and
// this hop, which an unnumbered sender was never reporting anyway.
//
// The counter is advanced past the sender's number so that a later locally
// numbered batch cannot collide with one the sender numbered.
func TestPublishEventNumbersAnUnnumberedPush(t *testing.T) {
	v := &Viewer{events: make(chan Event, 4)}
	body, err := json.Marshal(proto.EventBody{Nodes: []proto.NodeJSON{{Path: "x"}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	v.publishEvent(proto.Envelope{Body: body})

	select {
	case e := <-v.events:
		if e.Seq == 0 {
			t.Error("an unnumbered push reached the channel as zero, which a " +
				"consumer reads as 'not numbered' and cannot check for gaps")
		}
	default:
		t.Fatal("nothing was published")
	}
}

// A sender's number must not be overtaken by the local counter: after a push
// numbered 7 arrives, a locally numbered push has to be numbered past it, or the
// two streams would interleave and the consumer's idea of "next" would be wrong.
func TestLocalNumberingDoesNotCollideWithASendersNumber(t *testing.T) {
	v := &Viewer{events: make(chan Event, 8)}
	body, err := json.Marshal(proto.EventBody{Seq: 7, Nodes: []proto.NodeJSON{{Path: "a"}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	v.publishEvent(proto.Envelope{Body: body})
	// An unnumbered push, numbered locally, must land after 7.
	v.emit([]node.Node{{Path: "b"}})

	if first := (<-v.events).Seq; first != 7 {
		t.Fatalf("first Seq = %d, want 7", first)
	}
	if second := (<-v.events).Seq; second <= 7 {
		t.Errorf("locally numbered Seq = %d, want > 7: the local counter was not "+
			"advanced past the sender's number", second)
	}
}

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
	// The channel is captured before Close can nil the Viewer's field. Ranging
	// over v.events directly would make the test itself race with that write,
	// which is a report about the test, not about emit.
	events := make(chan Event, 1<<20)
	v := &Viewer{conn: conn, events: events}

	// The channel is oversized on purpose so that every publish reaches the send
	// rather than being absorbed by the default arm; with the send taken every
	// time, a close landing after the check and before the send is what panics.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range events {
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
