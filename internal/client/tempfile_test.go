package client_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ispengya/peerfold/internal/client"
)

// A temporary file's name used to be derivable from the target's, and it was
// opened without O_EXCL — so a local process able to write the directory could
// pre-create a symlink by that name and have a remote put write through it.
func TestWriteDoesNotFollowAPredictableTempSymlink(t *testing.T) {
	root := t.TempDir()
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim.txt")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The exact name the old implementation would have used for a.txt.
	trap := filepath.Join(root, ".a.txt.peerfold.tmp")
	if err := os.Symlink(victim, trap); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}

	sh, err := client.NewShares([]string{"S=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	fs := client.NewFS(sh)
	if _, err := fs.WriteAll("S", "a.txt", strings.NewReader("payload")); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}

	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep" {
		t.Fatalf("victim = %q, %v; the write followed a symlink planted at the old temporary name", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(b) != "payload" {
		t.Fatalf("a.txt = %q, %v; the write did not land on its target", b, err)
	}
	// The trap itself must still be a link: the write went through a name of
	// its own rather than replacing this one.
	if info, err := os.Lstat(trap); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the planted link was replaced or removed: info=%v err=%v", info, err)
	}
}

// Two writers to one name used to share a temporary file, so their bytes could
// interleave and the rename would publish the mixture. Each write now has its
// own name, and whichever rename lands last installs a whole payload.
func TestConcurrentWritesToTheSameNameDoNotMix(t *testing.T) {
	root := t.TempDir()
	sh, err := client.NewShares([]string{"S=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	fs := client.NewFS(sh)

	payloads := [][]byte{
		bytes.Repeat([]byte("A"), 64<<10),
		bytes.Repeat([]byte("B"), 64<<10),
	}
	var wg sync.WaitGroup
	for _, payload := range payloads {
		wg.Add(1)
		go func(payload []byte) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := fs.WriteAll("S", "same.txt", bytes.NewReader(payload)); err != nil {
					t.Errorf("WriteAll: %v", err)
					return
				}
			}
		}(payload)
	}
	wg.Wait()

	got, err := os.ReadFile(filepath.Join(root, "same.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, payloads[0]) && !bytes.Equal(got, payloads[1]) {
		t.Fatalf("same.txt is %d bytes and is not either whole payload: concurrent writes mixed", len(got))
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".peerfold.tmp") {
			t.Errorf("temporary file %q was left behind", e.Name())
		}
	}
}
