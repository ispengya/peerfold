package mount

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/client"
	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/server"
	"github.com/ispengya/peerfold/internal/viewer"
)

// startFederation brings up a real server and mounts a viewer connection over
// it, so the join/leave path is exercised through the viewer's actual path
// translation rather than through the fake Caller.
func startFederation(t *testing.T) (*server.Server, string, *Mount) {
	t.Helper()
	s := server.New("127.0.0.1:0")
	if s.Addr() == nil {
		t.Fatalf("server did not bind: %v", s.Err())
	}
	go func() {
		if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { _ = s.Close() })

	v, err := viewer.DialMount(s.Addr().String(), "mnt", "/tmp/peerfold-test")
	if err != nil {
		t.Fatalf("DialMount: %v", err)
	}
	m := New(NewViewerCaller(v), DefaultOptions())
	m.startWatch()
	t.Cleanup(func() {
		m.stopWatch()
		_ = v.Close()
	})
	return s, s.Addr().String(), m
}

// joinClient connects a supplier with one empty share and waits for the join
// event to reach the mount.
func joinClient(t *testing.T, addr, name string, m *Mount) *client.Client {
	t.Helper()
	sh, err := client.NewShares([]string{"Documents=" + t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: name, Shares: sh})
	if err != nil {
		t.Fatalf("Dial(%s): %v", name, err)
	}
	if err := c.UploadTree(); err != nil {
		_ = c.Close()
		t.Fatalf("UploadTree(%s): %v", name, err)
	}
	waitFor(t, 5*time.Second, func() bool {
		n, ok := m.eventFor("/" + name)
		return ok && n.Kind == node.KindDir
	}, "the mount did not learn about client "+name)
	return c
}

// TestAClientComingOnlineAfterTheMountIsVisible walks the exact sequence the
// feature is for: the mount is already up, a client connects, the mount sees it,
// the client leaves, and a different client connects afterwards. The mount must
// survive the empty gap and show the second client.
func TestAClientComingOnlineAfterTheMountIsVisible(t *testing.T) {
	s, addr, m := startFederation(t)

	released := make(chan struct{})
	m.mu.Lock()
	m.onGiveUp = func() { close(released) }
	m.mu.Unlock()

	first := joinClient(t, addr, "alice", m)
	if n, code := m.lookup("/alice"); code != 0 || n.Kind != node.KindDir {
		t.Fatalf("lookup(/alice) = (%+v, %d), want a directory", n, code)
	}

	if err := first.Shutdown(2 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		n, ok := m.eventFor("/alice")
		return ok && n.Kind == node.KindDeleted
	}, "the mount did not learn that alice left")
	if _, code := m.lookup("/alice"); code != -ENOENT {
		t.Fatalf("lookup(/alice) = %d after the client left, want ENOENT", code)
	}
	if len(s.Registry().Live()) != 0 {
		t.Fatalf("registry still has clients: %v", s.Registry().Live())
	}

	// The last owner has left. The mount must still be there, or the second
	// client could never appear.
	select {
	case <-released:
		t.Fatal("the mount released itself when the last client left")
	case <-time.After(200 * time.Millisecond):
	}

	second := joinClient(t, addr, "bob", m)
	defer second.Close()
	if n, code := m.lookup("/bob"); code != 0 || n.Kind != node.KindDir {
		t.Fatalf("lookup(/bob) = (%+v, %d), want a directory", n, code)
	}
}
