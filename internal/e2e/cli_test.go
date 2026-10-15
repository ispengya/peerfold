// This file drives the real `peerfold` binary, not the libraries under it.
//
// Everything else in this package builds a server, a client and a viewer out of
// the same packages the commands use, which proves the pieces work together but
// not that any *command* wires them together. That gap is not hypothetical: the
// `client` subcommand shipped for several tasks uploading its tree and then
// blocking on a signal without ever starting a watcher, so the tree it published
// never changed again — and every test in this package passed, because each of
// them calls `c.Watch()` itself. A test that supplies the missing piece cannot
// notice that the program does not.
//
// So this one runs `peerfold client` as a process and asserts the property a
// user actually cares about: change a file on the supplying machine, and the
// aggregated tree follows.
package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildPeerfold compiles the CLI once per test binary and returns its path.
//
// The e2e package's own directory is internal/e2e, so the module root — where
// the `go` command must run — is two levels up.
func buildPeerfold(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	name := "peerfold"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/peerfold")
	cmd.Dir = "../.."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/peerfold: %v\n%s", err, out)
	}
	return bin
}

// lockedBuffer is an io.Writer safe to read while another goroutine writes.
//
// os/exec writes to a non-*os.File Stdout from its own goroutine whenever the
// command's output outlives a single write, so a plain strings.Builder would be
// read and written concurrently — which is a data race, and one this suite
// cannot detect without -race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestCLIClientKeepsTheTreeLiveAfterItJoins pins the one thing the library-level
// tests structurally cannot: that the shipped `client` command watches.
//
// Without it the scan is a snapshot taken at connect time, so a file created
// afterwards never reaches the index — and the index is the aggregate's only
// source of truth, so `ls` would report a tree that does not match the disk for
// as long as the client stays connected. That is the failure the design calls
// out by name.
func TestCLIClientKeepsTheTreeLiveAfterItJoins(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI binary")
	}
	bin := buildPeerfold(t)

	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"a.txt": "aaaa"})
	cfg := filepath.Join(t.TempDir(), "config.json")

	cmd := exec.Command(bin,
		"client",
		"--server", addr,
		"--name", "win",
		"--share", "Documents="+root,
		"--config", cfg,
	)
	stdout := &lockedBuffer{}
	cmd.Stdout = stdout
	var stderr lockedBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s client: %v", bin, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})

	v := dialViewer(t, addr)

	// Wait for the join announcement before asserting anything about the tree:
	// the file below has to be created *after* the client is up and watching, or
	// a snapshot taken at connect time would already contain it and the test
	// would pass without a watcher.
	waitFor(t, "the client to report that it joined", func() bool {
		if strings.Contains(stdout.String(), "joined ") {
			return true
		}
		// A client that died instead of joining should say so rather than
		// looking like a slow start.
		if strings.TrimSpace(stderr.String()) != "" {
			t.Fatalf("the client failed to join: stderr = %q", stderr.String())
		}
		return false
	})

	// The snapshot arrived with the connection; this is the liveness half.
	if !hasPath(t, v, "win/Documents", "Documents/a.txt") {
		t.Fatalf("the initial tree is missing Documents/a.txt")
	}

	if err := os.WriteFile(filepath.Join(root, "fresh.txt"), []byte("appeared"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	waitForWithin(t, 5*time.Second, "a file created after the client joined to reach the tree",
		func() bool { return hasPath(t, v, "win/Documents", "Documents/fresh.txt") })

	// And the size has to be right, not merely present: a node reported with a
	// transient size is the "the tree lies" failure in its other form.
	nodes, err := v.List("win/Documents")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, n := range nodes {
		if n.Path == "Documents/fresh.txt" && n.Size != 8 {
			t.Fatalf("fresh.txt size = %d, want 8", n.Size)
		}
	}
}
