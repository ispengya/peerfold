package server_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/proto"
)

// readWatcherEvent reads one pushed event and returns the nodes it carries.
func readWatcherEvent(t *testing.T, c *proto.Conn) []proto.NodeJSON {
	t.Helper()
	if !c.SetReadDeadline(time.Now().Add(5 * time.Second)) {
		t.Fatal("SetReadDeadline is not supported")
	}
	env, err := c.RecvAny()
	if err != nil {
		t.Fatalf("waiting for a watcher event: %v", err)
	}
	if env.Kind != proto.KindEvent {
		t.Fatalf("watcher received kind %q, want %q", env.Kind, proto.KindEvent)
	}
	var body proto.EventBody
	if err := json.Unmarshal(env.Body, &body); err != nil {
		t.Fatalf("decoding watcher event: %v", err)
	}
	return body.Nodes
}

// TestClientJoinAndLeaveReachAWatcher pins the discovery half of the online
// behaviour: a mount that is already attached is told when a supplier appears,
// when its tree is complete, and when it goes away. None of those change a path
// the mount already listed, so without an event there is nothing for it to
// notice.
func TestClientJoinAndLeaveReachAWatcher(t *testing.T) {
	_, addr := start(t)

	viewerConn, _ := hello(t, addr, "mnt", proto.RoleViewer, false)
	defer viewerConn.Close()
	viewer := proto.NewConn(viewerConn)

	clientConn, _ := hello(t, addr, "win", proto.RoleClient, false)
	client := proto.NewConn(clientConn)

	// The registration is broadcast before hello_ok is even sent, so the join is
	// already on its way by the time hello returns.
	join := readWatcherEvent(t, viewer)
	if len(join) != 1 || join[0].Path != "win" || node.Kind(join[0].Kind) != node.KindDir {
		t.Fatalf("join event = %+v, want one directory node named win", join)
	}

	// Finishing the upload repeats the same node: the first event may have been
	// seen while the tree was still arriving.
	if err := client.Send(proto.Envelope{ID: 2, Kind: proto.KindTreeDone, EOF: true}); err != nil {
		t.Fatalf("sending tree_done: %v", err)
	}
	if !client.SetReadDeadline(time.Now().Add(5 * time.Second)) {
		t.Fatal("SetReadDeadline is not supported")
	}
	if _, err := client.RecvAny(); err != nil {
		t.Fatalf("waiting for the tree_done acknowledgement: %v", err)
	}
	ready := readWatcherEvent(t, viewer)
	if len(ready) != 1 || ready[0].Path != "win" || node.Kind(ready[0].Kind) != node.KindDir {
		t.Fatalf("tree-ready event = %+v, want one directory node named win", ready)
	}

	// Closing the supplier is the ordinary offline path: the server removes the
	// tree and tells every watcher with a tombstone at the client name.
	if err := clientConn.Close(); err != nil {
		t.Fatalf("closing the client: %v", err)
	}
	gone := readWatcherEvent(t, viewer)
	if len(gone) != 1 || gone[0].Path != "win" || node.Kind(gone[0].Kind) != node.KindDeleted {
		t.Fatalf("leave event = %+v, want a deleted node named win", gone)
	}
}

// TestByeWithdrawsTheClientAndIsAcknowledged pins the graceful half: the
// withdrawal happens while the client is still connected, and the client waits
// for the acknowledgement. Without the answer a stopping client would have to
// guess whether the server had processed it before the socket closed.
func TestByeWithdrawsTheClientAndIsAcknowledged(t *testing.T) {
	s, addr := start(t)

	viewerConn, _ := hello(t, addr, "mnt", proto.RoleViewer, false)
	defer viewerConn.Close()
	viewer := proto.NewConn(viewerConn)

	clientConn, _ := hello(t, addr, "win", proto.RoleClient, false)
	defer clientConn.Close()
	client := proto.NewConn(clientConn)

	if err := client.Send(proto.Envelope{ID: 7, Kind: proto.KindBye}); err != nil {
		t.Fatalf("sending bye: %v", err)
	}
	if !client.SetReadDeadline(time.Now().Add(5 * time.Second)) {
		t.Fatal("SetReadDeadline is not supported")
	}
	ack, err := client.RecvAny()
	if err != nil {
		t.Fatalf("waiting for the bye acknowledgement: %v", err)
	}
	if ack.Kind != proto.KindByeOK || ack.ID != 7 {
		t.Fatalf("bye acknowledgement = %+v, want kind %q id 7", ack, proto.KindByeOK)
	}

	// The acknowledgement means the withdrawal has already happened, not that it
	// is about to: a client that restarts the moment it sees this must not be
	// refused as a duplicate of the name it just released.
	if live := s.Registry().Live(); len(live) != 0 {
		t.Fatalf("registry still has %v after bye_ok, want empty", live)
	}

	// The join event precedes the tombstone on the watcher's channel.
	for {
		nodes := readWatcherEvent(t, viewer)
		if len(nodes) == 1 && nodes[0].Path == "win" && node.Kind(nodes[0].Kind) == node.KindDeleted {
			return
		}
	}
}
