package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peerfold/internal/client"
	"peerfold/internal/config"
	"peerfold/internal/server"
)

// startServerForCLI brings up a real server and returns its address, so the ls
// tests drive the actual viewer path rather than a stub.
func startServerForCLI(t *testing.T) string {
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
	return s.Addr().String()
}

// joinForCLI dials a client that shares a directory holding one nested file.
func joinForCLI(t *testing.T, addr, name string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("nested"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sh, err := client.NewShares([]string{"Documents=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: name, Shares: sh})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}
}

// writeConfig points a config file at addr so `ls` can find the server.
func writeConfig(t *testing.T, addr string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.Default()
	cfg.Server = addr
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}

// TestLsListsTheTopLevel pins the shape of the output: a kind letter, a tab,
// and the name, so a caller can cut on the tab.
func TestLsListsTheTopLevel(t *testing.T) {
	addr := startServerForCLI(t)
	joinForCLI(t, addr, "win")
	joinForCLI(t, addr, "linux")
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "--config", cfg}, &out, &errOut); code != 0 {
		t.Fatalf("runWithEmptyStdin(ls) = %d, want 0; stderr = %q", code, errOut.String())
	}
	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := []string{"d\tlinux", "d\twin"}
	if len(got) != len(want) {
		t.Fatalf("ls output = %q, want %q", out.String(), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ls output = %q, want %q", out.String(), want)
		}
	}
}

// TestLsWalksIntoAClient pins that a bare client name lists its shares, which
// is the whole point of naming one.
func TestLsWalksIntoAClient(t *testing.T) {
	addr := startServerForCLI(t)
	joinForCLI(t, addr, "win")
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "--config", cfg, "win"}, &out, &errOut); code != 0 {
		t.Fatalf("runWithEmptyStdin(ls win) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if got := strings.TrimSpace(out.String()); got != "d\tDocuments" {
		t.Fatalf("ls win = %q, want %q", got, "d\tDocuments")
	}
}

// TestLsMarksFilesAndDirectories pins the kind letter itself: a listing that
// showed everything as a directory would still be "non-empty output".
func TestLsMarksFilesAndDirectories(t *testing.T) {
	addr := startServerForCLI(t)
	joinForCLI(t, addr, "win")
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "--config", cfg, "win/Documents"}, &out, &errOut); code != 0 {
		t.Fatalf("runWithEmptyStdin(ls win/Documents) = %d, want 0; stderr = %q", code, errOut.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	want := map[string]bool{
		"d\tDocuments/sub":   true,
		"f\tDocuments/a.txt": true,
	}
	if len(lines) != len(want) {
		t.Fatalf("ls win/Documents = %q, want %d lines", out.String(), len(want))
	}
	for _, line := range lines {
		if !want[line] {
			t.Fatalf("ls win/Documents = %q, unexpected line %q", out.String(), line)
		}
	}
}

// TestLsMissingPathFails pins that an unknown path is an error, not an empty
// listing: a silent empty output would look like an empty directory.
func TestLsMissingPathFails(t *testing.T) {
	addr := startServerForCLI(t)
	joinForCLI(t, addr, "win")
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	code := runWithEmptyStdin([]string{"ls", "--config", cfg, "win/Documents/nope"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("runWithEmptyStdin(ls of a missing path) = 0, want non-zero; stdout = %q", out.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("stdout = %q, want it empty on failure", out.String())
	}
	if errOut.Len() == 0 {
		t.Error("stderr is empty, want the reason for the failure")
	}
}

func TestLsUnknownClientFails(t *testing.T) {
	addr := startServerForCLI(t)
	joinForCLI(t, addr, "win")
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "--config", cfg, "nobody"}, &out, &errOut); code == 0 {
		t.Fatalf("runWithEmptyStdin(ls nobody) = 0, want non-zero")
	}
}

// TestLsUnreachableServerFails pins that a server the CLI cannot reach is an
// error with a message, rather than an empty listing.
func TestLsUnreachableServerFails(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "--config", cfg}, &out, &errOut); code == 0 {
		t.Fatalf("runWithEmptyStdin(ls with no server) = 0, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Error("stderr is empty, want the reason for the failure")
	}
}

// TestLsWithoutAConfiguredServerFails pins the case where there is no server to
// dial at all: it must be a clear error rather than a hang or a nil deref.
func TestLsWithoutAConfiguredServerFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var out, errOut bytes.Buffer
	code := runWithEmptyStdin([]string{"ls", "--config", path}, &out, &errOut)
	if code == 0 {
		t.Fatalf("runWithEmptyStdin(ls with no server configured) = 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "no server") {
		t.Errorf("stderr = %q, want it to mention the missing server", errOut.String())
	}
}

// TestLsTooManyArgsFails pins the usage error, so `ls a b` does not silently
// list only one of them.
func TestLsTooManyArgsFails(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "a", "b"}, &out, &errOut); code != 2 {
		t.Fatalf("runWithEmptyStdin(ls a b) = %d, want 2", code)
	}
}

// TestLsFollowsAChange pins that ls reads the live index rather than a cached
// listing: a file added by a second upload is visible without restarting.
func TestLsFollowsAChange(t *testing.T) {
	addr := startServerForCLI(t)
	joinForCLI(t, addr, "win")
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"ls", "--config", cfg, "win/Documents"}, &out, &errOut); code != 0 {
		t.Fatalf("first ls = %d, want 0; stderr = %q", code, errOut.String())
	}
	first := out.String()

	// A second client publishing a share must appear at the top level.
	joinForCLI(t, addr, "linux")

	deadline := time.Now().Add(10 * time.Second)
	for {
		out.Reset()
		errOut.Reset()
		if code := runWithEmptyStdin([]string{"ls", "--config", cfg}, &out, &errOut); code != 0 {
			t.Fatalf("second ls = %d, want 0; stderr = %q", code, errOut.String())
		}
		if strings.Contains(out.String(), "linux") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ls never showed the new client; first = %q, now = %q", first, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
