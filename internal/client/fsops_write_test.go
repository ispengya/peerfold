package client_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"peerfold/internal/client"
)

// fsWriteFixture builds a share with a directory, a file, and a subdirectory
// holding a file, and returns the FS, the share name, and the share root on
// disk so a test can assert against the real filesystem rather than against the
// FS's own opinion of what it did.
func fsWriteFixture(t *testing.T) (*client.FS, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("aaaa"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("bb"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sh, err := client.NewShares([]string{"Documents=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	return client.NewFS(sh), "Documents", root
}

// readFile fails the test if the file cannot be read, so an assertion about
// content never silently passes because the file is missing.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return b
}

// TestFSWriteAllCreatesANewFile pins the basic write: the bytes are on the
// owner's disk and the count is what was written.
func TestFSWriteAllCreatesANewFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	n, err := fs.WriteAll(share, "new.bin", strings.NewReader("hello world"))
	if err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	if n != 11 {
		t.Fatalf("WriteAll = %d, want 11", n)
	}
	if got := string(readFile(t, filepath.Join(root, "new.bin"))); got != "hello world" {
		t.Fatalf("file content = %q, want %q", got, "hello world")
	}
}

// TestFSWriteAllReplacesALongerFile pins that the write is a replacement and not
// an overwrite: a longer old file must leave no tail behind.
//
// This is the difference between truncating and merely writing over the start,
// and it is invisible whenever the new content is at least as long as the old.
func TestFSWriteAllReplacesALongerFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	target := filepath.Join(root, "long.txt")
	if err := os.WriteFile(target, []byte(strings.Repeat("x", 500)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := fs.WriteAll(share, "long.txt", strings.NewReader("short")); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	got := string(readFile(t, target))
	if got != "short" {
		t.Fatalf("content = %q (%d bytes), want %q: the old tail must not survive", got, len(got), "short")
	}
}

// TestFSWriteAllLeavesNoTempFile pins that the temporary file the write goes
// through is gone once the write returns. A leftover would be visible in the
// owner's own directory.
func TestFSWriteAllLeavesNoTempFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if _, err := fs.WriteAll(share, "new.bin", strings.NewReader("x")); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "peerfold.tmp") {
			t.Fatalf("a temporary file survived the write: %s", e.Name())
		}
	}
}

// TestFSWriteAllFromALargeReaderSplitsOverFrames is covered by the protocol
// tests; here the concern is that the whole reader is consumed regardless of
// how it delivers its bytes.
func TestFSWriteAllFromAChunkedReader(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	// A reader that never returns more than one byte at a time.
	r := iotest{data: []byte("chunked")}
	n, err := fs.WriteAll(share, "chunked.bin", &r)
	if err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	if n != 7 {
		t.Fatalf("WriteAll = %d, want 7", n)
	}
	if got := string(readFile(t, filepath.Join(root, "chunked.bin"))); got != "chunked" {
		t.Fatalf("content = %q, want %q", got, "chunked")
	}
}

// iotest yields one byte per Read so a writer cannot depend on a full buffer.
type iotest struct {
	data []byte
	pos  int
}

func (r *iotest) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

// TestFSCreateMakesAnEmptyFile pins Create: a new file exists, is empty, and the
// count of bytes written is zero.
func TestFSCreateMakesAnEmptyFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if err := fs.Create(share, "created.txt"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "created.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("Size = %d, want 0", info.Size())
	}
	if info.IsDir() {
		t.Fatal("IsDir = true, want a file")
	}
}

// TestFSCreateRefusesAnExistingFile pins that Create does not quietly truncate
// a file that is already there.
func TestFSCreateRefusesAnExistingFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	err := fs.Create(share, "a.txt")
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("Create of an existing file = %v, want os.ErrExist", err)
	}
	if got := string(readFile(t, filepath.Join(root, "a.txt"))); got != "aaaa" {
		t.Fatalf("content = %q, want it untouched: Create must not truncate", got)
	}
}

// TestFSMkdirCreatesADirectory pins the directory case.
func TestFSMkdirCreatesADirectory(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if err := fs.Mkdir(share, "newdir"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "newdir"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("IsDir = false, want a directory")
	}
}

// TestFSMkdirRefusesAnExistingPath pins that Mkdir does not accept a directory
// that is already there.
func TestFSMkdirRefusesAnExistingPath(t *testing.T) {
	fs, share, _ := fsWriteFixture(t)

	if err := fs.Mkdir(share, "sub"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Mkdir of an existing directory = %v, want os.ErrExist", err)
	}
	if err := fs.Mkdir(share, "a.txt"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("Mkdir over an existing file = %v, want os.ErrExist", err)
	}
}

// TestFSRenameMovesWithinAShare pins the rename: the old name is gone and the
// new one holds the same bytes.
func TestFSRenameMovesWithinAShare(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if err := fs.Rename(share, "a.txt", "renamed.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old path still exists (err = %v)", err)
	}
	if got := string(readFile(t, filepath.Join(root, "renamed.txt"))); got != "aaaa" {
		t.Fatalf("content = %q, want %q", got, "aaaa")
	}
}

// TestFSRenameIntoASubdirectory pins that a rename is not restricted to one
// directory.
func TestFSRenameIntoASubdirectory(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if err := fs.Rename(share, "a.txt", "sub/moved.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := string(readFile(t, filepath.Join(root, "sub", "moved.txt"))); got != "aaaa" {
		t.Fatalf("content = %q, want %q", got, "aaaa")
	}
}

// TestFSRenameMissingPathFails pins that renaming something that is not there is
// an error rather than a silent success.
func TestFSRenameMissingPathFails(t *testing.T) {
	fs, share, _ := fsWriteFixture(t)

	if err := fs.Rename(share, "nope.txt", "x.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Rename of a missing path = %v, want os.ErrNotExist", err)
	}
}

// TestFSRemoveDeletesAnEmptyDirectory pins the empty case.
func TestFSRemoveDeletesAnEmptyDirectory(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := fs.Remove(share, "empty"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "empty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory still exists (err = %v)", err)
	}
}

// TestFSRemoveDeletesAFile pins the file case.
func TestFSRemoveDeletesAFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if err := fs.Remove(share, "a.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file still exists (err = %v)", err)
	}
}

// TestFSRemoveRefusesANonEmptyDirectory is the safety property: Remove must not
// be recursive. A recursive delete would make one wrong path destroy a tree.
func TestFSRemoveRefusesANonEmptyDirectory(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	err := fs.Remove(share, "sub")
	if err == nil {
		t.Fatal("Remove of a non-empty directory = nil, want a failure: it must not recurse")
	}
	// The contents must still be there.
	if got := string(readFile(t, filepath.Join(root, "sub", "b.txt"))); got != "bb" {
		t.Fatalf("the child was deleted: content = %q, want %q", got, "bb")
	}
}

// TestFSRemoveMissingPathFails pins that removing something absent is an error.
func TestFSRemoveMissingPathFails(t *testing.T) {
	fs, share, _ := fsWriteFixture(t)

	if err := fs.Remove(share, "nope"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove of a missing path = %v, want os.ErrNotExist", err)
	}
}

// TestFSWriteOpsRefuseEscape pins that every write resolves through LocalPath
// first. A read that escaped would disclose a file; a write that escaped would
// modify one, so the check matters at least as much here.
func TestFSWriteOpsRefuseEscape(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	escapes := []string{"../outside.txt", "sub/../../outside.txt", ".."}
	for _, rel := range escapes {
		t.Run(rel, func(t *testing.T) {
			if _, err := fs.WriteAll(share, rel, strings.NewReader("x")); err == nil {
				t.Errorf("WriteAll(%q) = nil error, want a refusal", rel)
			}
			if err := fs.Create(share, rel); err == nil {
				t.Errorf("Create(%q) = nil error, want a refusal", rel)
			}
			if err := fs.Mkdir(share, rel); err == nil {
				t.Errorf("Mkdir(%q) = nil error, want a refusal", rel)
			}
			if err := fs.Remove(share, rel); err == nil {
				t.Errorf("Remove(%q) = nil error, want a refusal", rel)
			}
			if err := fs.Rename(share, "a.txt", rel); err == nil {
				t.Errorf("Rename to %q = nil error, want a refusal", rel)
			}
			if err := fs.Rename(share, rel, "x.txt"); err == nil {
				t.Errorf("Rename from %q = nil error, want a refusal", rel)
			}
		})
	}

	// Nothing may have been created outside the share root.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a write escaped the share root (err = %v)", err)
	}
}

// TestFSWriteOpsRejectUnknownShares pins that a share that was never declared is
// refused, so a name cannot be invented to reach an unrelated directory.
func TestFSWriteOpsRejectUnknownShares(t *testing.T) {
	fs, _, _ := fsWriteFixture(t)

	if _, err := fs.WriteAll("nope", "x.txt", strings.NewReader("x")); err == nil {
		t.Error("WriteAll into an unknown share = nil error, want a refusal")
	}
	if err := fs.Mkdir("nope", "d"); err == nil {
		t.Error("Mkdir in an unknown share = nil error, want a refusal")
	}
	if err := fs.Remove("nope", "a.txt"); err == nil {
		t.Error("Remove in an unknown share = nil error, want a refusal")
	}
}

// TestFSWriteAllFailsWhenTheShareIsNotWritable pins that a failed write is
// reported and produces nothing.
//
// This is the only test that reaches the write's own error path. It needs a
// read-only directory, which this host may not be able to enforce -- an elevated
// account on Windows can write to a 0500 directory -- so it verifies that first
// and skips rather than passing for the wrong reason.
func TestFSWriteAllFailsWhenTheShareIsNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Go's os.Chmod on Windows only toggles the read-only attribute, and an
		// elevated account ignores it. Rather than assert on a host that cannot
		// enforce this, the check below would pass for the wrong reason.
		t.Skip("a Windows account may write to a directory it cannot chmod; the read-only case is not enforceable here")
	}

	fs, share, root := fsWriteFixture(t)

	probe := filepath.Join(root, ".write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile probe: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("Remove probe: %v", err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	if _, err := fs.WriteAll(share, "cannot.txt", strings.NewReader("data")); err == nil {
		// Confirm the directory really is unwritable before believing the write
		// should have failed.
		if err := os.WriteFile(filepath.Join(root, "sanity.txt"), []byte("x"), 0o644); err == nil {
			t.Skip("this host can write to a 0500 directory, so the failure cannot be tested")
		}
		t.Fatal("WriteAll into a read-only share = nil error, want a failure")
	}

	if _, err := os.Stat(filepath.Join(root, "cannot.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed write left a file behind (err = %v)", err)
	}
}

// TestFSWriteAllNoResidueWhenTheReaderFails pins that a reader that dies
// part-way leaves the target either absent or whole, never half-written.
//
// The temporary file is allowed to survive: what must not survive is a partial
// file at the real path, because that would look like a complete file to every
// later reader.
func TestFSWriteAllNoResidueWhenTheReaderFails(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	_, err := fs.WriteAll(share, "partial.bin", &failingReader{failAfter: 3})
	if err == nil {
		t.Fatal("WriteAll with a failing reader = nil error, want the reader's failure")
	}
	if _, err := os.Stat(filepath.Join(root, "partial.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a partial file was left at the target (err = %v): a half-written file is worse than none", err)
	}
}

// TestFSWriteAllReplacesAtomicallyOnFailure pins that a failed write does not
// damage the file that was already there. The old content must survive.
func TestFSWriteAllReplacesAtomicallyOnFailure(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	if _, err := fs.WriteAll(share, "a.txt", &failingReader{failAfter: 2}); err == nil {
		t.Fatal("WriteAll = nil error, want the reader's failure")
	}
	if got := string(readFile(t, filepath.Join(root, "a.txt"))); got != "aaaa" {
		t.Fatalf("content = %q, want the original %q: a failed overwrite must not truncate", got, "aaaa")
	}
}

// failingReader yields failAfter bytes and then an error.
type failingReader struct {
	failAfter int
	sent      int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.sent >= r.failAfter {
		return 0, errors.New("reader failed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'z'
	r.sent++
	return 1, nil
}

// TestFSWriteAllEmptyReaderMakesAnEmptyFile pins that writing nothing is a
// successful write of an empty file, which is a real file and not an absence.
func TestFSWriteAllEmptyReaderMakesAnEmptyFile(t *testing.T) {
	fs, share, root := fsWriteFixture(t)

	n, err := fs.WriteAll(share, "empty.bin", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	if n != 0 {
		t.Fatalf("WriteAll = %d, want 0", n)
	}
	info, err := os.Stat(filepath.Join(root, "empty.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("Size = %d, want 0", info.Size())
	}
}

// TestFSWriteAllIntoAMissingDirectoryFails pins that a missing parent is an
// error rather than a panic or a silently created tree.
func TestFSWriteAllIntoAMissingDirectoryFails(t *testing.T) {
	fs, share, _ := fsWriteFixture(t)

	if _, err := fs.WriteAll(share, "nope/deeper/x.txt", strings.NewReader("x")); err == nil {
		t.Fatal("WriteAll into a missing parent = nil error, want a failure")
	}
}
