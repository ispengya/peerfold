package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/client"
	"github.com/ispengya/peerfold/internal/config"
)

// joinWithFiles dials a client sharing a directory built from the given files,
// so a cat test can choose exactly what is on the owner's disk.
func joinWithFiles(t *testing.T, addr, name string, files map[string]string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", full, err)
		}
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

// TestCatWritesTheOriginalBytes pins the whole point of cat: stdout is the file
// and nothing else, so `peerfold cat win/Documents/a.txt > copy` produces a copy.
// Anything the command wanted to say would corrupt that.
func TestCatWritesTheOriginalBytes(t *testing.T) {
	addr := startServerForCLI(t)
	joinWithFiles(t, addr, "win", map[string]string{"a.txt": "hello world\n"})
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", cfg, "win/Documents/a.txt"}, &out, &errOut); code != 0 {
		t.Fatalf("runWithEmptyStdin(cat) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if out.String() != "hello world\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello world\n")
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing on a successful read", errOut.String())
	}
}

// TestCatPassesBinaryBytesThrough pins that the bytes are not text-processed on
// the way out: a NUL byte and an invalid UTF-8 sequence both survive.
func TestCatPassesBinaryBytesThrough(t *testing.T) {
	addr := startServerForCLI(t)
	want := string([]byte{0x00, 0x01, 0xfe, 0xff, 0x80, 'a', 0x0a})
	joinWithFiles(t, addr, "win", map[string]string{"bin.dat": want})
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", cfg, "win/Documents/bin.dat"}, &out, &errOut); code != 0 {
		t.Fatalf("runWithEmptyStdin(cat) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if out.String() != want {
		t.Fatalf("stdout = %v, want %v", []byte(out.String()), []byte(want))
	}
}

// TestCatOfAnEmptyFileWritesNothing pins that an empty file is a success with
// empty output. Reporting it as a failure would make it indistinguishable from a
// file the owner does not have.
func TestCatOfAnEmptyFileWritesNothing(t *testing.T) {
	addr := startServerForCLI(t)
	joinWithFiles(t, addr, "win", map[string]string{"empty.txt": ""})
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", cfg, "win/Documents/empty.txt"}, &out, &errOut); code != 0 {
		t.Fatalf("runWithEmptyStdin(cat) = %d, want 0 for an empty file; stderr = %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing", errOut.String())
	}
}

// TestCatMissingPathFails pins that a path the owner does not have is an error
// with a message on stderr, not silent empty output that looks like an empty
// file.
func TestCatMissingPathFails(t *testing.T) {
	addr := startServerForCLI(t)
	joinWithFiles(t, addr, "win", map[string]string{"a.txt": "hello"})
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", cfg, "win/Documents/nope.txt"}, &out, &errOut); code == 0 {
		t.Fatalf("runWithEmptyStdin(cat of a missing file) = 0, want non-zero; stdout = %q", out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing on a failure", out.String())
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty: a failure must say something")
	}
}

// TestCatUnknownClientFails pins the offline case: nobody can answer, so it is an
// error rather than a hang or an empty answer.
func TestCatUnknownClientFails(t *testing.T) {
	addr := startServerForCLI(t)
	joinWithFiles(t, addr, "win", map[string]string{"a.txt": "hello"})
	cfg := writeConfig(t, addr)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", cfg, "nobody/x"}, &out, &errOut); code == 0 {
		t.Fatalf("runWithEmptyStdin(cat of an unknown client) = 0, want non-zero; stdout = %q", out.String())
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty: an unknown client must be reported")
	}
}

// TestCatWithoutAPathIsAUsageError pins the argument check, so `cat` alone does
// not dial and then report something misleading.
func TestCatWithoutAPathIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat"}, &out, &errOut); code != 2 {
		t.Fatalf("runWithEmptyStdin(cat) = %d, want 2; stderr = %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "exactly one PATH") {
		t.Fatalf("stderr = %q, want it to name the missing PATH", errOut.String())
	}
}

// TestCatTooManyPathsIsAUsageError pins that `cat a b` is refused rather than
// silently printing the first one.
func TestCatTooManyPathsIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "a", "b"}, &out, &errOut); code != 2 {
		t.Fatalf("runWithEmptyStdin(cat a b) = %d, want 2; stderr = %q", code, errOut.String())
	}
}

// TestCatUnreachableServerFails pins that a server the CLI cannot reach is an
// error, not a hang and not empty output.
func TestCatUnreachableServerFails(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", cfg, "win/Documents/a.txt"}, &out, &errOut); code == 0 {
		t.Fatalf("runWithEmptyStdin(cat) = 0 against an unreachable server, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty: an unreachable server must be reported")
	}
}

// TestCatWithoutAConfiguredServerFails pins the no-server case, which must be a
// clear error rather than a nil dereference.
func TestCatWithoutAConfiguredServerFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"cat", "--config", path, "win/Documents/a.txt"}, &out, &errOut); code == 0 {
		t.Fatal("runWithEmptyStdin(cat) = 0 with no server configured, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty: a missing server must be reported")
	}
}
