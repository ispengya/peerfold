package server_test

import (
	"errors"
	"testing"

	"github.com/ispengya/peerfold/internal/node"
	"github.com/ispengya/peerfold/internal/server"
)

// The Query is the read side of the server the CLI drives: it answers "what is
// under this path" without any of the write paths being involved.

func TestQueryListClientRoots(t *testing.T) {
	s, _ := start(t)

	// Two clients with a share each, plus one nested directory to prove the
	// listing is by component rather than by raw string prefix.
	s.Index().Apply("win", []node.Node{
		{Path: "Documents", Kind: node.KindDir},
		{Path: "Documents/a.txt", Kind: node.KindFile, Size: 3},
		{Path: "Documents/sub", Kind: node.KindDir},
	})
	s.Index().Apply("linux", []node.Node{
		{Path: "home", Kind: node.KindDir},
	})
	registerForQuery(t, s, "win")
	registerForQuery(t, s, "linux")

	got, err := s.Query().List("win", "")
	if err != nil {
		t.Fatalf("List(win, \"\"): %v", err)
	}
	if len(got) != 1 || got[0].Path != "Documents" {
		t.Fatalf("List(win, \"\") = %v, want [Documents]", pathsOf(got))
	}
	if got[0].Kind != node.KindDir {
		t.Errorf("Kind = %d, want KindDir", got[0].Kind)
	}
}

func TestQueryListTopLevelIsEveryLiveClient(t *testing.T) {
	s, _ := start(t)
	registerForQuery(t, s, "win")
	registerForQuery(t, s, "linux")

	got, err := s.Query().List("", "")
	if err != nil {
		t.Fatalf("List(\"\", \"\"): %v", err)
	}
	// Ascending, and each entry is a directory node named after its client.
	want := []string{"linux", "win"}
	if len(got) != len(want) {
		t.Fatalf("List(\"\", \"\") = %v, want %v", pathsOf(got), want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Fatalf("List(\"\", \"\") = %v, want %v", pathsOf(got), want)
		}
		if got[i].Kind != node.KindDir {
			t.Errorf("%s: Kind = %d, want KindDir — a client is shown as a directory", got[i].Path, got[i].Kind)
		}
	}
}

// A client can be live while owning no nodes yet, and must still be listed:
// the top level is the set of connected clients, not the set of indexed ones.
func TestQueryListTopLevelIncludesAnEmptyClient(t *testing.T) {
	s, _ := start(t)
	registerForQuery(t, s, "win")

	got, err := s.Query().List("", "")
	if err != nil {
		t.Fatalf("List(\"\", \"\"): %v", err)
	}
	if len(got) != 1 || got[0].Path != "win" {
		t.Fatalf("List(\"\", \"\") = %v, want [win] even though it has indexed nothing", pathsOf(got))
	}
}

func TestQueryListUnknownClient(t *testing.T) {
	s, _ := start(t)
	s.Index().Apply("win", []node.Node{{Path: "Documents", Kind: node.KindDir}})

	_, err := s.Query().List("nobody", "")
	if !errors.Is(err, server.ErrNoSuchClient) {
		t.Fatalf("List(nobody, \"\") error = %v, want ErrNoSuchClient", err)
	}
}

func TestQueryListChildren(t *testing.T) {
	s, _ := start(t)
	s.Index().Apply("win", []node.Node{
		{Path: "Documents", Kind: node.KindDir},
		{Path: "Documents/a.txt", Kind: node.KindFile, Size: 3},
		{Path: "Documents/sub", Kind: node.KindDir},
		{Path: "Documents/sub/b.txt", Kind: node.KindFile, Size: 4},
		{Path: "Documents/subway", Kind: node.KindDir},
		{Path: "Documents/subway/c.txt", Kind: node.KindFile, Size: 5},
	})
	registerForQuery(t, s, "win")

	got, err := s.Query().List("win", "Documents")
	if err != nil {
		t.Fatalf("List(win, Documents): %v", err)
	}
	// Subway is a sibling of sub, not a child of it: it shares the byte prefix
	// "Documents/sub" but is a direct child of Documents in its own right, so it
	// belongs here while its own contents must not appear.
	want := []string{"Documents/a.txt", "Documents/sub", "Documents/subway"}
	if len(got) != len(want) {
		t.Fatalf("List(win, Documents) = %v, want %v — a grandchild leaked in, or a sibling was dropped", pathsOf(got), want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Fatalf("List(win, Documents) = %v, want %v", pathsOf(got), want)
		}
	}
	for _, n := range got {
		if n.Path == "Documents/sub/b.txt" || n.Path == "Documents/subway/c.txt" {
			t.Fatalf("List(win, Documents) returned the grandchild %q", n.Path)
		}
	}
}

func TestQueryListMissingPath(t *testing.T) {
	s, _ := start(t)
	s.Index().Apply("win", []node.Node{{Path: "Documents", Kind: node.KindDir}})
	registerForQuery(t, s, "win")

	_, err := s.Query().List("win", "Documents/nope")
	if err == nil {
		t.Fatal("List of a missing path = nil error, want a failure")
	}
}

// A file has no children: asking for them is a caller error, not an empty
// directory, and must be distinguishable from one.
func TestQueryListOnAFileIsNotAnEmptyDirectory(t *testing.T) {
	s, _ := start(t)
	s.Index().Apply("win", []node.Node{
		{Path: "Documents", Kind: node.KindDir},
		{Path: "Documents/a.txt", Kind: node.KindFile, Size: 3},
	})
	registerForQuery(t, s, "win")

	if _, err := s.Query().List("win", "Documents/a.txt"); err == nil {
		t.Fatal("List of a file's children = nil error, want a failure")
	}
}

func TestQueryClients(t *testing.T) {
	s, _ := start(t)
	registerForQuery(t, s, "win")
	registerForQuery(t, s, "alpha")

	got := s.Query().Clients()
	want := []string{"alpha", "win"}
	if len(got) != len(want) {
		t.Fatalf("Clients() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Clients() = %v, want %v", got, want)
		}
	}
}

// registerForQuery puts a name into the registry the way a connection would,
// by reusing the real Register path. The index is applied separately because a
// test wants to control the nodes without a live socket.
func registerForQuery(t *testing.T, s *server.Server, name string) {
	t.Helper()
	if _, err := s.Registry().Register(name, false); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}
	t.Cleanup(func() { s.Registry().Unregister(name) })
}

func pathsOf(nodes []node.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Path)
	}
	return out
}
