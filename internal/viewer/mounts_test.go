package viewer_test

import (
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/server"
	"github.com/ispengya/peerfold/internal/viewer"
)

// startServer runs a real server for the duration of one test.
func startServer(t *testing.T) (s *server.Server, addr string) {
	t.Helper()
	s = server.New("127.0.0.1:0")
	if err := s.Err(); err != nil {
		t.Fatalf("server.New: %v", err)
	}
	go s.Serve()
	t.Cleanup(func() { s.Close() })
	return s, s.Addr().String()
}

// TestDialMountIsVisibleToOtherViewers pins the cross-machine contract: a
// mount declares its host and mount point in the handshake, and a viewer on
// another machine can ask the server for the list. The querying viewer itself
// must not appear -- it is not a mount and would otherwise pollute every answer.
func TestDialMountIsVisibleToOtherViewers(t *testing.T) {
	_, addr := startServer(t)

	m, err := viewer.DialMount(addr, "winbox", `C:\Users\me\peerfold`)
	if err != nil {
		t.Fatalf("DialMount: %v", err)
	}
	defer m.Close()

	q, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer q.Close()

	mounts, err := q.ListMounts()
	if err != nil {
		t.Fatalf("ListMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("ListMounts returned %d mounts, want 1: %+v", len(mounts), mounts)
	}
	got := mounts[0]
	if got.Name != "winbox" {
		t.Errorf("Name = %q, want winbox", got.Name)
	}
	if got.MountPoint != `C:\Users\me\peerfold` {
		t.Errorf("MountPoint = %q, want C:\\Users\\me\\peerfold", got.MountPoint)
	}
	if got.RemoteAddr == "" {
		t.Error("RemoteAddr is empty; the server observed the connection and should say where it came from")
	}
	if got.Since.IsZero() {
		t.Error("Since is zero; the server should stamp when it accepted the mount")
	}
}

// TestClosedMountDisappears pins the other half: a mount that goes away is no
// longer reported. The removal is done by the connection handler on its way
// out, so the assertion polls rather than assuming it is synchronous.
func TestClosedMountDisappears(t *testing.T) {
	_, addr := startServer(t)

	m, err := viewer.DialMount(addr, "winbox", "/mnt/federation")
	if err != nil {
		t.Fatalf("DialMount: %v", err)
	}
	defer m.Close()

	q, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer q.Close()

	if mounts, err := q.ListMounts(); err != nil || len(mounts) != 1 {
		t.Fatalf("before close: mounts = %+v, err = %v; want one mount", mounts, err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("mount Close: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mounts, err := q.ListMounts()
		if err != nil {
			t.Fatalf("ListMounts after close: %v", err)
		}
		if len(mounts) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("closed mount still listed after 5s: %+v", mounts)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
