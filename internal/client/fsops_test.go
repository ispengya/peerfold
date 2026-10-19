package client_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/client"
)

// fsFixture builds a share holding one file of known content plus a zero-byte
// file, and returns the FS with the share name.
//
//	<tmp>/Documents/known.bin   300 bytes: 0,1,2,...  (byte i == i%256)
//	<tmp>/Documents/zero.bin    0 bytes
func fsFixture(t *testing.T) (*client.FS, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	known := make([]byte, 300)
	for i := range known {
		known[i] = byte(i)
	}
	if err := os.WriteFile(filepath.Join(root, "known.bin"), known, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "zero.bin"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sh, err := client.NewShares([]string{"Documents=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	return client.NewFS(sh), "Documents"
}

// wantRange is the fixture's expected bytes: byte i of known.bin is i%256.
func wantRange(off, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(off + i)
	}
	return out
}

func TestFSReadFirstBytes(t *testing.T) {
	fs, share := fsFixture(t)

	got, info, err := fs.ReadAt(share, "known.bin", 0, 100)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if len(got) != 100 {
		t.Fatalf("len = %d, want 100", len(got))
	}
	if want := wantRange(0, 100); string(got) != string(want) {
		t.Fatalf("ReadAt(0,100) returned the wrong bytes")
	}
	if info == nil {
		t.Fatal("FileInfo is nil, want the file's metadata alongside the bytes")
	}
	if info.Size() != 300 {
		t.Errorf("Size = %d, want 300", info.Size())
	}
	if info.IsDir() {
		t.Error("IsDir = true, want false for a file")
	}
}

// A read that runs past the end returns the bytes that exist and no error. A
// caller that asked for more than there is should get a short read, not a
// failure: that is what makes "the last 50 bytes of a file" expressible.
func TestFSReadClampsAtEOF(t *testing.T) {
	fs, share := fsFixture(t)

	got, _, err := fs.ReadAt(share, "known.bin", 250, 100)
	if err != nil {
		t.Fatalf("ReadAt(250,100): %v", err)
	}
	if len(got) != 50 {
		t.Fatalf("len = %d, want 50: the read must clamp to the end of the file", len(got))
	}
	if want := wantRange(250, 50); string(got) != string(want) {
		t.Fatal("ReadAt(250,100) returned the wrong bytes")
	}
}

// Starting at or past the end yields nothing and no error. A caller that asked
// for a range past the end gets an empty answer, not a diagnosis: the alternative
// makes an empty read a failure, which would make a zero-byte file a missing one
// at every layer above this.
func TestFSReadPastTheEndIsEmpty(t *testing.T) {
	fs, share := fsFixture(t)

	got, info, err := fs.ReadAt(share, "known.bin", 400, 10)
	if err != nil {
		t.Fatalf("ReadAt(400,10): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
	if info == nil {
		t.Fatal("FileInfo is nil: a read past the end still names the file")
	}
}

// A zero-byte file is a real file with real metadata. It must report its size and
// read as an empty success, not as a missing file and not as a read error.
func TestFSReadZeroByteFile(t *testing.T) {
	fs, share := fsFixture(t)

	got, info, err := fs.ReadAt(share, "zero.bin", 0, 10)
	if err != nil {
		t.Fatalf("ReadAt of a zero-byte file: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
	if info == nil {
		t.Fatal("FileInfo is nil: a zero-byte file still has metadata")
	}
	if info.Size() != 0 {
		t.Errorf("Size = %d, want 0", info.Size())
	}
}

func TestFSReadMissingFile(t *testing.T) {
	fs, share := fsFixture(t)

	_, _, err := fs.ReadAt(share, "nope.bin", 0, 10)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want it to satisfy os.ErrNotExist", err)
	}
}

func TestFSReadUnknownShare(t *testing.T) {
	fs, _ := fsFixture(t)

	if _, _, err := fs.ReadAt("nosuchshare", "known.bin", 0, 10); err == nil {
		t.Fatal("ReadAt with an unknown share name = nil error, want a failure")
	}
}

// LocalPath is the escape gate. It must refuse and return no path at all: a
// caller that got a path back would be free to use it.
func TestFSLocalPathRejectsEscape(t *testing.T) {
	fs, share := fsFixture(t)

	for _, rel := range []string{
		"../../etc/passwd",
		"../outside.txt",
		"sub/../../outside.txt",
		"..",
	} {
		got, err := fs.LocalPath(share, rel)
		if err == nil {
			t.Errorf("LocalPath(%q) = %q, nil; want an error and no path", rel, got)
		}
		if got != "" {
			t.Errorf("LocalPath(%q) returned the path %q alongside the error; it must return nothing", rel, got)
		}
	}
}

func TestFSLocalPathAllowsInside(t *testing.T) {
	fs, share := fsFixture(t)

	got, err := fs.LocalPath(share, "known.bin")
	if err != nil {
		t.Fatalf("LocalPath: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("LocalPath = %q, want an absolute path", got)
	}
	if base := filepath.Base(got); base != "known.bin" {
		t.Errorf("LocalPath = %q, want it to end in known.bin", got)
	}
	// The share root itself is not a legal target: the empty relative path
	// names the root, and naming the root is not naming something inside it.
	// It used to be allowed on the grounds that listing a share needs it, but
	// listing is served by the scanner, which starts at the root it already
	// has — no read or write ever asks LocalPath for the root. Allowing it was
	// how Remove("") came to unlink the share root itself, and how a whole-file
	// write to "" came to put its temporary file in the share's parent
	// directory.
	if got, err := fs.LocalPath(share, ""); err == nil {
		t.Errorf("LocalPath(\"\") = %q, want a refusal: the empty path is the share root", got)
	}
}

// TestFSReadRejectsEscape pins that ReadAt goes through the same gate: reading
// is the operation a path traversal would actually be used for.
func TestFSReadRejectsEscape(t *testing.T) {
	fs, share := fsFixture(t)

	if _, _, err := fs.ReadAt(share, "../outside.txt", 0, 10); err == nil {
		t.Fatal("ReadAt of an escaping path = nil error, want a failure")
	}
}

// deepShare creates a share holding n files, nested deep enough that the
// scanned paths are near this host's limit. It returns the root, the share
// spec, and the share-relative directory holding the files.
//
// Path length is the point: a batch travels as one control frame, and a frame
// is limited in bytes, so how many nodes fit in a batch depends on how long
// their paths are. A fixture of short names cannot reach the boundary that a
// real machine's deep trees reach.
func deepShare(t *testing.T, n int) (root, spec, leafDir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "Documents")
	dir := root
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Climb until the path is long enough to be near the limit, then stop:
	// the climbing is what makes each node's path, and so each batch, big.
	component := strings.Repeat("d", 30)
	for len(dir)+len(component)+1 < 220 {
		next := filepath.Join(dir, component)
		if err := os.Mkdir(next, 0o755); err != nil {
			break
		}
		dir = next
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	leafDir = filepath.ToSlash(rel)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%05d.txt", strings.Repeat("f", 20), i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Skipf("this host cannot create deep paths for a batch fixture: %v", err)
		}
	}
	return root, "Documents=" + root, leafDir
}

// TestUploadTreeSplitsBatchesOnPayloadSize pins the frame-size ceiling.
//
// A batch is one control frame, and a frame is limited in bytes, so the node
// count alone cannot bound a batch: 4006 nodes whose paths are about 195 bytes
// marshal to 1,056,944 bytes, past MaxPayload itself. A client that only
// counted nodes would hand the frame layer one oversized payload — and here
// that is accepted without an error, so the split must be asserted directly.
func TestUploadTreeSplitsBatchesOnPayloadSize(t *testing.T) {
	s, addr := startServer(t)
	_, spec, leafDir := deepShare(t, 4000)

	sh, err := client.NewShares([]string{spec}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}

	// The node count is deliberately enormous, so the byte budget is the only
	// thing that can split this upload.
	//
	// The split has to be asserted directly rather than inferred from "the
	// upload succeeded": on this fixture an unsplit batch is over MaxPayload
	// yet is still accepted end-to-end, so success alone proves nothing.
	var batches, nodes int
	c, err := client.Dial(client.Options{
		Server: addr, Name: "big", Shares: sh, Batch: 100000,
		OnBatch: func(sent int) {
			batches++
			nodes = sent
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree with long paths: %v", err)
	}
	if batches < 2 {
		t.Fatalf("the upload went out in %d batch(es); paths this long do not fit in one frame, so the byte budget must split them", batches)
	}
	wantNodes := 4000 + 1 + len(strings.Split(strings.Trim(leafDir, "/"), "/"))
	if nodes != wantNodes {
		t.Fatalf("OnBatch totalled %d nodes, want %d: every scanned node must be sent exactly once", nodes, wantNodes)
	}

	// The upload barrier means every node is indexed once TreeDone is acked.
	waitForChildCount(t, s.Index(), "big", "Documents/"+leafDir, 4000)
}
