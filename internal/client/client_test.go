package client_test

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"peerfold/internal/client"
	"peerfold/internal/index"
	"peerfold/internal/node"
	"peerfold/internal/proto"
	"peerfold/internal/server"
)

// startServer brings up a real server on a loopback port.
func startServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	s := server.New("127.0.0.1:0")
	if s.Addr() == nil {
		t.Fatal("Addr() is nil before Serve")
	}
	go func() {
		if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { s.Close() })
	return s, s.Addr().String()
}

// docs returns the Documents share inside the shared fixture.
func docsOf(t *testing.T) string {
	t.Helper()
	return filepath.Join(buildTree(t), "Documents")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// children lists the paths under dir on the server's index, ascending.
func children(t *testing.T, ix *index.Index, name, dir string) []string {
	t.Helper()
	nodes, err := ix.Children(name, dir)
	if err != nil {
		t.Fatalf("Children(%q, %q): %v", name, dir, err)
	}
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Path
	}
	sort.Strings(out)
	return out
}

// waitForChild polls until dir on client has exactly want as its children.
//
// The index belongs to the connection: a client's nodes are dropped when its
// handler notices the disconnect. A test that closes the client and then reads
// the index is therefore racing that teardown, so every post-Close assertion
// polls rather than sampling once.
func waitForChild(t *testing.T, ix *index.Index, name, dir string, want []string) {
	t.Helper()
	var last []string
	deadline := time.Now().Add(10 * time.Second)
	for {
		nodes, err := ix.Children(name, dir)
		if err == nil {
			last = last[:0]
			for _, n := range nodes {
				last = append(last, n.Path)
			}
			sort.Strings(last)
			if len(last) == len(want) {
				ok := true
				for i := range want {
					if last[i] != want[i] {
						ok = false
						break
					}
				}
				if ok {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Children(%q, %q) = %v, want %v", name, dir, last, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestClientDialReportsGrantedRole(t *testing.T) {
	_, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	c, err := client.Dial(client.Options{Server: addr, Name: "laptop", Shares: sh, Aggregate: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if c.Name() != "laptop" {
		t.Fatalf("Name() = %q, want laptop", c.Name())
	}
	if !c.Aggregate() {
		t.Fatal("Aggregate() = false for the first client to ask, want true")
	}
}

func TestClientUploadTreeIndexesShares(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	c, err := client.Dial(client.Options{Server: addr, Name: "laptop", Shares: sh, Aggregate: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}

	// The share name is the top-level entry for this client.
	waitForChild(t, s.Index(), "laptop", "", []string{"Documents"})

	want := []string{
		"Documents/a.txt",
		"Documents/empty",
		"Documents/sub",
		"Documents/zero.bin",
	}
	got := children(t, s.Index(), "laptop", "Documents")
	if len(got) != len(want) {
		t.Fatalf("Children(laptop, Documents) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Children(laptop, Documents) = %v, want %v", got, want)
		}
	}

	// Size and Mtime must come from the filesystem, not be left at zero.
	info, err := os.Stat(filepath.Join(docs, "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	n, ok := s.Index().Lookup("laptop", "Documents/a.txt")
	if !ok {
		t.Fatal("Documents/a.txt is not in the index")
	}
	if n.Size != info.Size() {
		t.Errorf("Size = %d, want %d", n.Size, info.Size())
	}
	if n.Mtime != info.ModTime().UnixNano() {
		t.Errorf("Mtime = %d, want %d", n.Mtime, info.ModTime().UnixNano())
	}
	if n.Kind != node.KindFile {
		t.Errorf("Kind = %d, want KindFile", n.Kind)
	}

	// A zero-byte file and an empty directory are real entries.
	if _, ok := s.Index().Lookup("laptop", "Documents/zero.bin"); !ok {
		t.Error("a zero-byte file was not indexed")
	}
	if kids, err := s.Index().Children("laptop", "Documents/empty"); err != nil || len(kids) != 0 {
		t.Errorf("Children(empty) = (%v, %v), want (empty, nil)", kids, err)
	}

}

// The aggregating client supplies the aggregated tree as well as observing it:
// asking for --aggregate must not stop a client sharing its own directories.
func TestClientAggregateAlsoSharesItsOwnTree(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: "agg", Shares: sh, Aggregate: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}
	if _, ok := s.Index().Lookup("agg", "Documents/a.txt"); !ok {
		t.Fatal("the aggregating client's own tree is missing from the index")
	}
}

func TestClientSecondAggregateIsDowngraded(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	first, err := client.Dial(client.Options{Server: addr, Name: "one", Shares: sh, Aggregate: true})
	if err != nil {
		t.Fatalf("Dial first: %v", err)
	}
	defer first.Close()

	second, err := client.Dial(client.Options{Server: addr, Name: "two", Shares: sh, Aggregate: true})
	if err != nil {
		t.Fatalf("Dial second: %v", err)
	}
	defer second.Close()

	// The second client is connected and usable, but not the aggregator.
	if second.Aggregate() {
		t.Fatal("the second client was granted the aggregate role; v1 allows exactly one")
	}
	if owner, ok := s.Registry().AggregateOwner(); !ok || owner != "one" {
		t.Fatalf("AggregateOwner() = (%q, %v), want (one, true)", owner, ok)
	}
	// It is still a full supplier: the downgrade is about the role, not about
	// whether its tree is welcome.
	if err := second.UploadTree(); err != nil {
		t.Fatalf("UploadTree on a downgraded client: %v", err)
	}
	if _, ok := s.Index().Lookup("two", "Documents/a.txt"); !ok {
		t.Fatal("a downgraded client's tree is missing from the index")
	}
}

// Batches of one are the boundary: every node is its own batch, and the upload
// still completes.
func TestClientUploadTreeWithBatchOfOne(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: "batchy", Shares: sh, Batch: 1})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}
	if _, ok := s.Index().Lookup("batchy", "Documents/a.txt"); !ok {
		t.Fatal("a batch-of-one upload lost a node")
	}
	if _, ok := s.Index().Lookup("batchy", "Documents/sub/b.txt"); !ok {
		t.Fatal("a batch-of-one upload lost a nested node")
	}
}

// The tree must grow as the batches are sent, not appear all at once at the
// end: a caller showing progress is told the truth, and the server is not
// buffering the whole upload.
func TestClientUploadTreeGrowsIndexProgressively(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	var calls int
	var last int
	c, err := client.Dial(client.Options{
		Server: addr, Name: "prog", Shares: sh, Batch: 2,
		OnBatch: func(sent int) {
			calls++
			if sent <= last {
				t.Errorf("OnBatch sent total %d after %d: the running total must increase", sent, last)
			}
			last = sent
			// The server indexes each batch as it arrives, so the index must
			// have grown by the time the client is told about the batch.
			// Poll, because the server applies it on its own goroutine.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if s.Index().Len() >= sent {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Errorf("OnBatch reported %d nodes sent but the index has only %d: the tree is not growing with the upload",
				sent, s.Index().Len())
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}

	// Five nodes (share root, a.txt, empty, sub, sub/b.txt, zero.bin = six) in
	// batches of two means three batches, so several calls.
	if calls < 2 {
		t.Fatalf("OnBatch was called %d times; a batched upload must report progress more than once", calls)
	}
	if last < 6 {
		t.Fatalf("OnBatch reported %d nodes sent, want at least 6", last)
	}
}

// Closing the client is what removes it from the server: there is no goodbye
// message, so the server acts on the disconnection.
func TestClientCloseRemovesClientFromServer(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: "leaver", Shares: sh})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}
	if _, ok := s.Index().Lookup("leaver", "Documents/a.txt"); !ok {
		t.Fatal("precondition: the upload did not reach the index")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "the registry to drop the client", func() bool {
		return len(s.Registry().Live()) == 0
	})
	waitFor(t, "the index to drop the client's nodes", func() bool {
		_, ok := s.Index().Lookup("leaver", "Documents/a.txt")
		return !ok
	})
	if got := s.Index().Len(); got != 0 {
		t.Fatalf("Len() = %d after the only client left, want 0", got)
	}
}

// A duplicate name must be refused by the server, and the refusal must reach
// the caller rather than looking like a successful connection.
func TestClientDialReportsDuplicateName(t *testing.T) {
	_, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	first, err := client.Dial(client.Options{Server: addr, Name: "same", Shares: sh})
	if err != nil {
		t.Fatalf("Dial first: %v", err)
	}
	defer first.Close()

	if _, err := client.Dial(client.Options{Server: addr, Name: "same", Shares: sh}); err == nil {
		t.Fatal("Dial with a duplicate name succeeded, want a refusal")
	}
}

// An unreachable server must fail the dial rather than block forever.
func TestClientDialFailsFastOnRefusedConnection(t *testing.T) {
	// Port 1 on loopback has nothing listening.
	start := time.Now()
	_, err := client.Dial(client.Options{Server: "127.0.0.1:1", Name: "nope"})
	if err == nil {
		t.Fatal("Dial to a closed port succeeded")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Dial took %v to fail, want a prompt connection error", elapsed)
	}
}

// A viewer connection is not a supplier: uploading a tree must be refused.
func TestClientViewerCannotUploadTree(t *testing.T) {
	s, addr := startServer(t)
	docs := docsOf(t)
	sh, err := client.NewShares([]string{"Documents=" + docs}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: "v", Role: proto.RoleViewer, Shares: sh})
	if err != nil {
		t.Fatalf("Dial viewer: %v", err)
	}
	defer c.Close()
	if err := c.UploadTree(); err == nil {
		t.Fatal("a viewer uploaded a tree; only a registered client may own index entries")
	}
	if got := s.Index().Len(); got != 0 {
		t.Fatalf("Len() = %d after a viewer upload, want 0", got)
	}
}
