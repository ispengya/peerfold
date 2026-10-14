// Package e2e drives the real components against each other: a server, real
// clients over TCP, and a viewer. The unit tests prove each part in isolation;
// this is where "the pieces work together" is actually demonstrated.
package e2e

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"peerfold/internal/client"
	"peerfold/internal/node"
	"peerfold/internal/server"
	"peerfold/internal/viewer"
)

// startServer brings up a server on a loopback port and returns it with its
// address. New binds the listener, so the address is known before Serve runs.
func startServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	s := server.New("127.0.0.1:0")
	if s.Addr() == nil {
		t.Fatal("Addr() is nil before Serve: New must bind the listener")
	}
	go func() {
		if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { s.Close() })
	return s, s.Addr().String()
}

// share creates a share with the given files directly inside it.
func share(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", root, err)
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", full, err)
		}
	}
	return root
}

// join dials a client, uploads its tree, and returns the live client.
func join(t *testing.T, addr, name string, shares ...string) *client.Client {
	t.Helper()
	sh, err := client.NewShares(shares, nil)
	if err != nil {
		t.Fatalf("NewShares(%v): %v", shares, err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: name, Shares: sh})
	if err != nil {
		t.Fatalf("Dial(%s): %v", name, err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree(%s): %v", name, err)
	}
	return c
}

func pathsOf(nodes []node.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Path)
	}
	return out
}

// waitFor polls until cond holds, so a test does not depend on how quickly an
// index update happens to land. The index is built asynchronously by the
// server's handler, and the acknowledgement removes most of that race, but a
// poll here keeps a failure from being a timing accident.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestE2EList walks the aggregated tree the way a user does: the top level is
// the set of connected clients, then a client's shares, then its directories,
// and finally a file.
func TestE2EList(t *testing.T) {
	_, addr := startServer(t)

	winDocs := share(t, "Documents", map[string]string{
		"a.txt":     "hello",
		"sub/b.txt": "nested",
	})
	linuxHome := share(t, "home", map[string]string{
		"notes.md": "notes",
	})
	join(t, addr, "win", "Documents="+winDocs)
	join(t, addr, "linux", "home="+linuxHome)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	// Top level: every connected client, ascending.
	top, err := v.List("")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	want := []string{"linux", "win"}
	if got := pathsOf(top); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List(\"\") = %v, want %v (ascending)", got, want)
	}
	for _, n := range top {
		if n.Kind != node.KindDir {
			t.Errorf("%s: Kind = %d, want KindDir — a client is presented as a directory", n.Path, n.Kind)
		}
	}

	// One client's shares.
	shares, err := v.List("win")
	if err != nil {
		t.Fatalf("List(win): %v", err)
	}
	if got := pathsOf(shares); len(got) != 1 || got[0] != "Documents" {
		t.Fatalf("List(win) = %v, want [Documents]", got)
	}

	// A directory inside that client, then the file itself.
	docs, err := v.List("win/Documents")
	if err != nil {
		t.Fatalf("List(win/Documents): %v", err)
	}
	if got := pathsOf(docs); len(got) != 2 || got[0] != "Documents/a.txt" || got[1] != "Documents/sub" {
		t.Fatalf("List(win/Documents) = %v, want [Documents/a.txt Documents/sub]", got)
	}

	nested, err := v.List("win/Documents/sub")
	if err != nil {
		t.Fatalf("List(win/Documents/sub): %v", err)
	}
	if len(nested) != 1 || nested[0].Path != "Documents/sub/b.txt" {
		t.Fatalf("List(win/Documents/sub) = %v, want [Documents/sub/b.txt]", pathsOf(nested))
	}
	if nested[0].Kind != node.KindFile {
		t.Errorf("b.txt: Kind = %d, want KindFile", nested[0].Kind)
	}
	if nested[0].Size != int64(len("nested")) {
		t.Errorf("b.txt: Size = %d, want %d", nested[0].Size, len("nested"))
	}

	// The other client is reachable the same way.
	home, err := v.List("linux/home")
	if err != nil {
		t.Fatalf("List(linux/home): %v", err)
	}
	if got := pathsOf(home); len(got) != 1 || got[0] != "home/notes.md" {
		t.Fatalf("List(linux/home) = %v, want [home/notes.md]", got)
	}
}

func TestE2EListReportsUnknownPaths(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	if _, err := v.List("nobody"); err == nil {
		t.Fatal("List(nobody) = nil error, want a failure")
	}
	if _, err := v.List("win/Documents/nope"); err == nil {
		t.Fatal("List of a missing path = nil error, want a failure")
	}
}

// A viewer is not registered and owns nothing, so it must not appear at the top
// level and must not disturb the suppliers.
func TestE2EViewerIsNotInTheTree(t *testing.T) {
	s, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	top, err := v.List("")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if got := pathsOf(top); len(got) != 1 || got[0] != "win" {
		t.Fatalf("List(\"\") = %v, want [win]: a viewer must not appear as a client", got)
	}
	if live := s.Registry().Live(); len(live) != 1 || live[0] != "win" {
		t.Fatalf("Live() = %v, want [win]", live)
	}
}

// A client that disconnects disappears from the top level, and its nodes go
// with it: the tree describes what is connected right now.
func TestE2EListFollowsDisconnects(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	c := join(t, addr, "win", "Documents="+docs)
	join(t, addr, "linux", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	waitFor(t, "both clients to be listed", func() bool {
		top, err := v.List("")
		return err == nil && len(top) == 2
	})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	waitFor(t, "win to disappear from the top level", func() bool {
		top, err := v.List("")
		if err != nil || len(top) != 1 {
			return false
		}
		return top[0].Path == "linux"
	})
	if _, err := v.List("win"); err == nil {
		t.Fatal("List(win) after disconnect = nil error, want a failure")
	}
}

// A client with two shares lists both, and listing one does not leak the other.
func TestE2EListMultipleShares(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	media := share(t, "Media", map[string]string{"song.mp3": "audio"})

	sh, err := client.NewShares([]string{"Documents=" + docs, "Media=" + media}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: "win", Shares: sh})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	waitFor(t, "both shares to be listed", func() bool {
		got, err := v.List("win")
		return err == nil && len(got) == 2
	})
	got, err := v.List("win")
	if err != nil {
		t.Fatalf("List(win): %v", err)
	}
	paths := pathsOf(got)
	if len(paths) != 2 || paths[0] != "Documents" || paths[1] != "Media" {
		t.Fatalf("List(win) = %v, want [Documents Media]", paths)
	}

	mediaKids, err := v.List("win/Media")
	if err != nil {
		t.Fatalf("List(win/Media): %v", err)
	}
	if paths := pathsOf(mediaKids); len(paths) != 1 || paths[0] != "Media/song.mp3" {
		t.Fatalf("List(win/Media) = %v, want [Media/song.mp3]", paths)
	}
}

// TestE2ECatReadsTheOriginalBytes is the whole point of cat: the bytes a viewer
// gets back are the bytes on the owner's disk.
func TestE2ECatReadsTheOriginalBytes(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello world"})
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	got, err := v.Cat("win/Documents/a.txt")
	if err != nil {
		t.Fatalf("Cat: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("Cat = %q, want %q", got, "hello world")
	}
}

// TestE2EReadReturnsARange pins that a range read returns that range and not
// the whole file: a server that ignored the offset would still return bytes.
func TestE2EReadReturnsARange(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello world"})
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	got, err := v.Read("win/Documents/a.txt", 6, 5)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "world" {
		t.Fatalf("Read(off=6,n=5) = %q, want %q", got, "world")
	}
}

// TestE2ECatAnEmptyFile pins the zero-byte case end-to-end: it is a real file,
// and the reply must be an empty read rather than an error or a hang.
func TestE2ECatAnEmptyFile(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	if err := os.WriteFile(filepath.Join(docs, "empty.txt"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	got, err := v.Cat("win/Documents/empty.txt")
	if err != nil {
		t.Fatalf("Cat of an empty file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Cat of an empty file = %q, want nothing", got)
	}
}

func TestE2ECatUnknownPathsFail(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	// A path the owner does not have, and a client that is not connected.
	// The latter must not panic or hang: there is nobody to answer.
	for _, path := range []string{"win/Documents/nope.txt", "nobody/x"} {
		done := make(chan error, 1)
		go func(p string) {
			_, err := v.Cat(p)
			done <- err
		}(path)
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("Cat(%q) = nil error, want a failure", path)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Cat(%q) did not return within 5s: an offline client must fail, not hang", path)
		}
	}
}

// TestE2ECatReflectsADeletedFile pins that the index is a cache of names, not of
// content: once the owner's file is gone, reading it must fail rather than
// return the bytes the index remembers.
func TestE2ECatReflectsADeletedFile(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	if _, err := v.Cat("win/Documents/a.txt"); err != nil {
		t.Fatalf("Cat before delete: %v", err)
	}
	if err := os.Remove(filepath.Join(docs, "a.txt")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := v.Cat("win/Documents/a.txt"); err == nil {
		t.Fatal("Cat after the file was deleted on the owner = nil error, want a failure; the index is not a content cache")
	}
}

// TestE2ECatFailsFastWhenTheOwnerLeaves pins the offline case at the router: no
// one can answer, so the caller must be told quickly rather than waiting out a
// timeout meant for a slow but live owner.
func TestE2ECatFailsFastWhenTheOwnerLeaves(t *testing.T) {
	_, addr := startServer(t)
	docs := share(t, "Documents", map[string]string{"a.txt": "hello"})
	owner := join(t, addr, "win", "Documents="+docs)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	if err := owner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "the owner to disappear from the top level", func() bool {
		top, err := v.List("")
		return err == nil && len(top) == 0
	})

	start := time.Now()
	if _, err := v.Cat("win/Documents/a.txt"); err == nil {
		t.Fatal("Cat with no connected owner = nil error, want a failure")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Cat with no connected owner took %s, want it to fail fast rather than wait out a live-owner timeout", elapsed)
	}
}

// TestE2EListAWidthPastOneFrame pins that a listing is not limited by one
// frame's payload.
//
// A frame carries at most MaxPayload, and a listing of 20000 files is several
// times that, so a listing implemented as one frame cannot answer at all — and
// the user sees a transport error rather than "that directory is too big". The
// plan requires listings to be batchable; this is where that is checked.
func TestE2EListAWidthPastOneFrame(t *testing.T) {
	_, addr := startServer(t)
	root := t.TempDir()
	const n = 20000
	// 20-byte names, so the reply is comfortably past 1 MiB and the paths stay
	// short enough to create quickly.
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file-%014d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	join(t, addr, "wide", "Documents="+root)

	v, err := viewer.Dial(addr)
	if err != nil {
		t.Fatalf("viewer.Dial: %v", err)
	}
	defer v.Close()

	nodes, err := v.List("wide/Documents")
	if err != nil {
		t.Fatalf("listing an %d-entry directory: %v", n, err)
	}
	if len(nodes) != n {
		t.Fatalf("listed %d entries, want %d", len(nodes), n)
	}
	// The whole reply must be one consistent listing, not a truncated prefix:
	// the last entry has to be there too.
	last := fmt.Sprintf("Documents/file-%014d.txt", n-1)
	found := false
	for _, nd := range nodes {
		if nd.Path == last {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the listing is missing %q: it was truncated", last)
	}

	// And the connection is still usable afterwards: a listing that overran a
	// frame must not have torn the connection down on its way out.
	if _, err := v.List(""); err != nil {
		t.Fatalf("List after a wide listing: %v", err)
	}
}
