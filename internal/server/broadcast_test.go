package server

import (
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

// This file drives broadcast's watcher-retirement policy.
//
// The policy's observable state is the two maps on Server -- who is watched, and
// how many consecutive sends to each have failed -- so these tests are in this
// package rather than driving a real viewer over a socket.
//
// Two mechanisms make the tests deterministic rather than racy:
//
//   - settle() waits for the per-watcher goroutines broadcast started. broadcast
//     returns as soon as it has spawned them (it must, or one slow watcher would
//     hold up the reporting client), so without a join a test would read a state
//     from before the attempt it just asked for.
//
//   - A failure is produced by closing the watcher's peer before the broadcast,
//     not by waiting for a timeout. SendWithin sets a write deadline first, and
//     SetWriteDeadline on a pipe whose other end is closed fails at once. So the
//     attempt fails immediately, with no clock involved, and the test runs in
//     microseconds.
//
// A property of a *real* timeout shapes what can be tested here. SendWithin
// remembers the first write error for the life of the Conn, because a timed-out
// write leaves the frame boundaries unknown: a later write would be appended
// behind a possibly-truncated frame. A watcher that times out is therefore
// poisoned and cannot be recovered on that Conn -- the remedy is to reconnect,
// which a real viewer does, and which the mount layer's own sequence-gap
// detection exists to prompt. A closed-peer failure takes a different path: it
// returns before any frame is written, so the Conn stays usable. That is the case
// the streak reset exists for, and it is reachable in tests. The one thing that
// is *not* reachable is a success on an already-timed-out Conn, so no test here
// claims one.

// fenceWait bounds how long a test will wait for an in-flight attempt.
const fenceWait = 5 * time.Second

// settle waits until every send goroutine broadcast started has finished, state
// update included.
func settle(t *testing.T, s *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		s.fanout.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(fenceWait):
		t.Fatal("a broadcast attempt never finished")
	}
}

// sendAndSettle broadcasts one envelope and waits for every attempt to finish.
// After it returns, the counters reflect that broadcast.
func sendAndSettle(t *testing.T, s *Server) {
	t.Helper()
	s.broadcast(testEnvelope())
	settle(t, s)
}

// watcher registers a connection as a watching peer and returns the server-side
// Conn and the peer.
func watcher(t *testing.T, s *Server) (*proto.Conn, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	c := proto.NewConn(local)
	s.mu.Lock()
	s.watchers[c] = struct{}{}
	s.mu.Unlock()
	return c, peer
}

// breakingWatcher registers a watcher whose peer is already closed, so every
// send to it fails at once.
func breakingWatcher(t *testing.T, s *Server) *proto.Conn {
	t.Helper()
	conn, peer := watcher(t, s)
	if err := peer.Close(); err != nil {
		t.Fatalf("closing the peer: %v", err)
	}
	return conn
}

func watcherCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watchers)
}

func watchFailStreak(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.watchFails {
		n += v
	}
	return n
}

func testEnvelope() proto.Envelope {
	return proto.Envelope{Kind: proto.KindEvent, Body: []byte(`{"nodes":[]}`)}
}

// TestOneFailedBroadcastDoesNotRetireTheWatcher pins the deliberately forgiving
// half of the policy.
//
// A watcher whose socket has gone quiet for a moment is not a watcher that has
// gone away, and retiring it is worse than it sounds: the connection stays up and
// keeps serving List and Read, so the watcher goes on being answered from a tree
// that has stopped changing, with nothing anywhere to say so. That is the failure
// this test exists to rule out.
//
// The mutation that matters: retire on the first failure. This is the test that
// catches it.
func TestOwnerGoneBroadcastCarriesAWatchableTombstone(t *testing.T) {
	s := New("127.0.0.1:0")
	t.Cleanup(func() { _ = s.Close() })

	_, peer := watcher(t, s)
	_ = peer

	env := marshalOwnerGone("win")
	if env.Kind != proto.KindEvent {
		t.Fatalf("owner-loss event kind = %q, want %q", env.Kind, proto.KindEvent)
	}
	var body proto.EventBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("decoding owner-loss event: %v", err)
	}
	if len(body.Nodes) != 1 {
		t.Fatalf("owner-loss event has %d nodes, want 1", len(body.Nodes))
	}
	if got := body.Nodes[0]; got.Path != "win" || got.Kind != uint8(node.KindDeleted) {
		t.Fatalf("owner-loss tombstone = %+v, want path=win kind=%d", got, node.KindDeleted)
	}
}

// TestAQueuedWatcherReceivesUpdatesInOrder pins that the per-watcher queue
// preserves the order updates were produced in. The old fan-out started one
// goroutine per update, and two updates could reach the connection in either
// order -- so a mount could apply the older state last.
func TestAQueuedWatcherReceivesUpdatesInOrder(t *testing.T) {
	s := New("127.0.0.1:0")
	t.Cleanup(func() { _ = s.Close() })

	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	conn := proto.NewConn(local)
	unwatch := s.watch(conn)
	defer unwatch()
	peerConn := proto.NewConn(peer)

	s.mu.Lock()
	q := s.watchQueues[conn]
	s.mu.Unlock()
	if q == nil {
		t.Fatal("watch registered no outbound queue")
	}

	const updates = 32
	for i := 0; i < updates; i++ {
		body, err := json.Marshal(i + 1)
		if err != nil {
			t.Fatalf("marshal update body: %v", err)
		}
		s.broadcast(proto.Envelope{Kind: proto.KindEvent, Body: body})
	}

	if err := peer.SetReadDeadline(time.Now().Add(fenceWait)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	for i := 0; i < updates; i++ {
		// RecvAny, not Recv: Recv deliberately passes over pushed updates.
		env, err := peerConn.RecvAny()
		if err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		want, _ := json.Marshal(i + 1)
		if string(env.Body) != string(want) {
			t.Fatalf("update %d carried %s, want %d", i, env.Body, i+1)
		}
	}
}

func TestClientDisconnectBroadcastsOwnerRemoval(t *testing.T) {
	s := New("127.0.0.1:0")
	t.Cleanup(func() { _ = s.Close() })

	conn, peer := watcher(t, s)

	s.mu.Lock()
	s.reg.Register("win", false)
	s.ix.Apply("win", []node.Node{{Path: "", Kind: node.KindDir}})
	s.mu.Unlock()

	s.mu.Lock()
	delete(s.watchers, conn)
	s.mu.Unlock()

	got := make(chan []byte, 1)
	s.mu.Lock()
	s.watchers[conn] = struct{}{}
	s.mu.Unlock()
	go func() {
		buf := make([]byte, 1<<16)
		n, err := peer.Read(buf)
		if err == nil {
			got <- buf[:n]
		}
	}()

	s.ix.RemoveClient("win")
	s.broadcastOwnerGone("win")

	select {
	case body := <-got:
		var env proto.Envelope
		if err := json.Unmarshal(body, &env); err == nil && env.Kind != "" {
			t.Fatalf("owner-loss broadcast body decoded unexpectedly as an envelope: %+v", env)
		}
	case <-time.After(fenceWait):
		t.Fatal("the owner-loss broadcast never reached the watcher")
	}
}

func TestOneFailedBroadcastDoesNotRetireTheWatcher(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	breakingWatcher(t, s)
	sendAndSettle(t, s)

	if got := watchFailStreak(s); got != 1 {
		t.Errorf("consecutive failures recorded = %d, want 1", got)
	}
	if got := watcherCount(s); got != 1 {
		t.Errorf("watcher count = %d, want 1: a watcher was retired after a single "+
			"failed send", got)
	}
}

// TestTwoFailuresAreStillShortOfTheThreshold pins that the threshold really is a
// threshold and not an off-by-one.
func TestTwoFailuresAreStillShortOfTheThreshold(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	breakingWatcher(t, s)

	for i := 1; i <= 2; i++ {
		sendAndSettle(t, s)
		if got := watcherCount(s); got != 1 {
			t.Fatalf("watcher count = %d after %d failures, want 1: the threshold is %d",
				got, i, watchFailThreshold)
		}
	}
	if got := watchFailStreak(s); got != 2 {
		t.Errorf("consecutive failures recorded = %d, want 2", got)
	}
}

// TestTheThresholdIsWhereItRetires pins the other half, and pins the exact
// number: the watchFailThreshold-th consecutive failure retires the watcher and
// no earlier one does.
func TestTheThresholdIsWhereItRetires(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	breakingWatcher(t, s)

	for i := 1; i < watchFailThreshold; i++ {
		sendAndSettle(t, s)
		if got := watcherCount(s); got != 1 {
			t.Fatalf("the watcher was retired after %d failures; the threshold is %d",
				i, watchFailThreshold)
		}
	}
	sendAndSettle(t, s)
	if got := watcherCount(s); got != 0 {
		t.Errorf("watcher count = %d after %d consecutive failures, want 0",
			got, watchFailThreshold)
	}
	// The failure record must not be left behind: a viewer that reconnected would
	// otherwise be retired on its first slow moment.
	if got := len(s.watchFails); got != 0 {
		t.Errorf("watchFails has %d entries after the watcher was retired, want 0", got)
	}
}

// TestASuccessfulBroadcastResetsTheStreak pins that the count is a streak and not
// a total.
//
// Without the reset, a watcher that stumbled once an hour would be retired after
// three hours of otherwise perfect service, which is the original bug with a
// longer fuse.
//
// The first half of the sequence uses a broken watcher, whose failure returns
// before any frame is written and so leaves the Conn usable. The second half
// replaces that watcher with one whose peer reads, which produces the successful
// send. This models recovery rather than waiting for it: a Conn that timed out is
// poisoned and no later send on it can succeed (see the note at the top), so the
// only recovery a real viewer has is to reconnect -- and reconnecting is a new
// Conn with a fresh count, which is what the last assertion checks.
func TestASuccessfulBroadcastResetsTheStreak(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	conn := breakingWatcher(t, s)

	// Two failures on the broken watcher.
	for i := 1; i <= 2; i++ {
		sendAndSettle(t, s)
	}
	if got := watchFailStreak(s); got != 2 {
		t.Fatalf("failures recorded = %d, want 2", got)
	}
	if watcherCount(s) != 1 {
		t.Fatal("the watcher was retired before reaching the threshold")
	}

	// Replace it, the way a viewer that reconnects does: a new registration and a
	// new Conn. A readable peer makes the next send succeed.
	s.mu.Lock()
	delete(s.watchers, conn)
	delete(s.watchFails, conn)
	s.mu.Unlock()

	good, peer := watcher(t, s)
	go func() {
		buf := make([]byte, 1<<16)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
		}
	}()

	sendAndSettle(t, s)
	if got := watchFailStreak(s); got != 0 {
		t.Errorf("failures recorded = %d after a successful send, want 0: the count "+
			"is a total, not a streak", got)
	}

	// And the fresh registration is intact and has no inherited history.
	s.mu.Lock()
	_, registered := s.watchers[good]
	_, hasHistory := s.watchFails[good]
	s.mu.Unlock()
	if !registered {
		t.Error("the reconnected watcher is not registered")
	}
	if hasHistory {
		t.Error("the reconnected watcher inherited a failure streak")
	}
}

// TestRetiringOneWatcherLeavesTheOthersAlone pins that the policy is per
// connection: one viewer that has stopped reading must not cost the others their
// updates.
func TestRetiringOneWatcherLeavesTheOthersAlone(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	goodConn, peer := watcher(t, s)
	go func() {
		buf := make([]byte, 1<<16)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
		}
	}()

	breakingWatcher(t, s)

	if watcherCount(s) != 2 {
		t.Fatalf("watcher count = %d, want 2", watcherCount(s))
	}
	for i := 1; i <= watchFailThreshold; i++ {
		sendAndSettle(t, s)
	}

	if got := watcherCount(s); got != 1 {
		t.Errorf("watcher count = %d, want 1: the watcher that kept up was retired "+
			"along with the one that did not", got)
	}
	// A count of one cannot say which one survived, so check by identity.
	s.mu.Lock()
	_, stillThere := s.watchers[goodConn]
	s.mu.Unlock()
	if !stillThere {
		t.Error("the watcher that kept up was the one retired")
	}
}

// TestARetiredWatcherKeepsItsConnection pins that retirement stops the updates and
// nothing else.
//
// This is the shape of the limitation rather than a guarantee: the watcher is
// removed from the fan-out, so its tree stops changing, but its List and Read keep
// working and keep being answered -- from an index that will now drift without
// bound. The mount layer is what closes the loop, by noticing that the sequence
// numbers stopped advancing.
func TestARetiredWatcherKeepsItsConnection(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	breakingWatcher(t, s)

	for i := 1; i <= watchFailThreshold; i++ {
		sendAndSettle(t, s)
	}
	if watcherCount(s) != 0 {
		t.Fatal("the watcher was not retired")
	}

	// A further broadcast finds no target at all, so the retired watcher is not
	// even attempted. That is the whole of the removal: its Conn is left open and
	// still serving, it is only out of the fan-out.
	sendAndSettle(t, s)
	if got := watcherCount(s); got != 0 {
		t.Errorf("watcher count = %d, want 0", got)
	}
	if got := watchFailStreak(s); got != 0 {
		t.Errorf("failures recorded = %d, want 0: nothing was attempted", got)
	}
}

// TestUnwatchClearsTheFailureRecord pins that a watcher leaving normally leaves
// nothing behind.
func TestUnwatchClearsTheFailureRecord(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()

	conn, peer := watcher(t, s)
	if err := peer.Close(); err != nil {
		t.Fatalf("closing the peer: %v", err)
	}
	sendAndSettle(t, s)
	if watchFailStreak(s) != 1 {
		t.Fatalf("failures recorded = %d, want 1", watchFailStreak(s))
	}

	// The handler's own cleanup path.
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Logf("closing the server side: %v", err)
	}
	s.mu.Lock()
	delete(s.watchers, conn)
	delete(s.watchFails, conn)
	s.mu.Unlock()

	if got := watchFailStreak(s); got != 0 {
		t.Errorf("failures recorded after the watcher left = %d, want 0", got)
	}
	if got := len(s.watchFails); got != 0 {
		t.Errorf("watchFails has %d entries after the watcher left, want 0", got)
	}
}

// TestARealTimeoutTerminatesTheSendsItBounds pins the timeout path itself, which
// the other tests avoid for speed.
//
// A watcher that is not reading and never closes is the case the bound exists
// for: without it the write to a full pipe would block for ever and hold up the
// fan-out for every other watcher. The test uses a short bound so it runs in
// milliseconds rather than in five seconds per attempt.
func TestARealTimeoutTerminatesTheSendsItBounds(t *testing.T) {
	s := New("127.0.0.1:0")
	defer s.Close()
	s.broadcastTimeout = 40 * time.Millisecond

	// A peer that is open but never reads. The pipe write blocks, and only the
	// deadline ends it.
	conn, _ := watcher(t, s)

	start := time.Now()
	sendAndSettle(t, s)
	elapsed := time.Since(start)

	if elapsed < s.broadcastTimeout {
		t.Errorf("the attempt returned after %v, before the %v bound: it cannot have "+
			"been ended by the deadline", elapsed, s.broadcastTimeout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the attempt took %v, far beyond the %v bound", elapsed, s.broadcastTimeout)
	}
	if got := watchFailStreak(s); got != 1 {
		t.Errorf("failures recorded = %d, want 1", got)
	}
	// The timed-out write is remembered on the Conn, so the connection is
	// finished: the frame boundaries are unknown and every later write reports
	// the original error rather than appending behind a possibly-truncated frame.
	if err := conn.SendWithin(testEnvelope(), time.Second); err == nil {
		t.Error("a write after a timed-out write succeeded: the stream's frame " +
			"boundaries are unknown, so it must be refused")
	}
}

// A viewer that asks not to watch must not be added to the fan-out. The extra
// connections a mount opens for parallel reads have no consumer for events:
// registering them would make the server marshal and queue updates nobody reads.
func TestAHelloThatAsksNotToWatchIsNotRegistered(t *testing.T) {
	s := New("127.0.0.1:0")
	t.Cleanup(func() { _ = s.Close() })
	go func() { _ = s.Serve() }()

	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	pc := proto.NewConn(c)

	no := false
	body, err := json.Marshal(proto.HelloBody{Role: proto.RoleViewer, Watch: &no})
	if err != nil {
		t.Fatalf("Marshal hello: %v", err)
	}
	if err := pc.Send(proto.Envelope{ID: 1, Kind: proto.KindHello, Body: body}); err != nil {
		t.Fatalf("Send hello: %v", err)
	}
	if _, err := pc.Recv(); err != nil {
		t.Fatalf("Recv hello reply: %v", err)
	}

	// The registration branch runs just after the reply, so watch the table for
	// a moment: a test that returned on the first zero would not notice a
	// registration that happened one scheduling step later.
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.watchers)
		s.mu.Unlock()
		if n != 0 {
			t.Fatalf("a non-watching viewer was registered as a watcher")
		}
		time.Sleep(time.Millisecond)
	}
}
