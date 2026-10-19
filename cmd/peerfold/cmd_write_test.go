package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/client"
	"github.com/ispengya/peerfold/internal/config"
)

// cliWriteFixture brings up a server and a client sharing a directory holding
// one file, and returns the config path and the share root. Every CLI write test
// asserts against the root, because the point of the command is what happened on
// the other machine.
func cliWriteFixture(t *testing.T) (cfg, root string) {
	t.Helper()
	addr := startServerForCLI(t)
	root = filepath.Join(t.TempDir(), "Documents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("original"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sh, err := client.NewShares([]string{"Documents=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	c, err := client.Dial(client.Options{Server: addr, Name: "win", Shares: sh})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}
	return writeConfig(t, addr), root
}

// diskContent reads a file on the supplying machine.
func diskContent(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("ReadFile %s: %v", rel, err)
	}
	return string(b)
}

// TestPutCLIWritesStdinToTheOwner pins the whole command: what arrives on stdin
// is what appears on the other machine.
func TestPutCLIWritesStdinToTheOwner(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	code := run([]string{"put", "--config", cfg, "win/Documents/from-cli.txt"}, strings.NewReader("piped in\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("run(put) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if got := diskContent(t, root, "from-cli.txt"); got != "piped in\n" {
		t.Fatalf("content = %q, want %q", got, "piped in\n")
	}
}

// TestPutCLIWritesNothingToStdout pins that a transfer does not narrate itself
// onto stdout, so a put can be part of a pipeline.
func TestPutCLIWritesNothingToStdout(t *testing.T) {
	cfg, _ := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"put", "--config", cfg, "win/Documents/quiet.txt"}, strings.NewReader("x"), &out, &errOut); code != 0 {
		t.Fatalf("run(put) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing on success", errOut.String())
	}
}

// TestPutCLIOverwritesByDefault pins that the plain form replaces, which is what
// makes it usable for a second save.
func TestPutCLIOverwritesByDefault(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"put", "--config", cfg, "win/Documents/a.txt"}, strings.NewReader("replaced"), &out, &errOut); code != 0 {
		t.Fatalf("run(put) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if got := diskContent(t, root, "a.txt"); got != "replaced" {
		t.Fatalf("content = %q, want %q", got, "replaced")
	}
}

// TestPutCLIExclusiveRefusesAnExistingFile pins the flag and, just as
// importantly, that the file it refused to write is still intact.
func TestPutCLIExclusiveRefusesAnExistingFile(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	code := run([]string{"put", "--exclusive", "--config", cfg, "win/Documents/a.txt"}, strings.NewReader("nope"), &out, &errOut)
	if code == 0 {
		t.Fatal("run(put --exclusive) = 0, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty: a refusal must say something")
	}
	if got := diskContent(t, root, "a.txt"); got != "original" {
		t.Fatalf("content = %q, want the original %q untouched", got, "original")
	}
}

// TestPutCLIExclusiveCreatesANewFile pins that the flag does not simply refuse
// everything.
func TestPutCLIExclusiveCreatesANewFile(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	code := run([]string{"put", "--exclusive", "--config", cfg, "win/Documents/fresh.txt"}, strings.NewReader("mine"), &out, &errOut)
	if code != 0 {
		t.Fatalf("run(put --exclusive) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if got := diskContent(t, root, "fresh.txt"); got != "mine" {
		t.Fatalf("content = %q, want %q", got, "mine")
	}
}

// TestPutCLIWithoutAPathIsAUsageError pins the argument check.
func TestPutCLIWithoutAPathIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"put"}, &out, &errOut); code != 2 {
		t.Fatalf("run(put) = %d, want 2; stderr = %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "exactly one PATH") {
		t.Fatalf("stderr = %q, want it to name the missing PATH", errOut.String())
	}
}

// TestPutCLIToAnUnknownClientFails pins that the content is not silently
// dropped: a put that cannot be delivered is an error, with a message.
func TestPutCLIToAnUnknownClientFails(t *testing.T) {
	cfg, _ := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	code := run([]string{"put", "--config", cfg, "nobody/Documents/x.txt"}, strings.NewReader("data"), &out, &errOut)
	if code == 0 {
		t.Fatal("run(put to an unknown client) = 0, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty")
	}
}

// TestMkdirCLICreatesTheDirectory pins the command.
func TestMkdirCLICreatesTheDirectory(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"mkdir", "--config", cfg, "win/Documents/newdir"}, &out, &errOut); code != 0 {
		t.Fatalf("run(mkdir) = %d, want 0; stderr = %q", code, errOut.String())
	}
	info, err := os.Stat(filepath.Join(root, "newdir"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("IsDir = false, want a directory")
	}
}

// TestMkdirCLITwiceFails pins that the second attempt is reported rather than
// accepted.
func TestMkdirCLITwiceFails(t *testing.T) {
	cfg, _ := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"mkdir", "--config", cfg, "win/Documents/once"}, &out, &errOut); code != 0 {
		t.Fatalf("run(mkdir) = %d, want 0; stderr = %q", code, errOut.String())
	}
	errOut.Reset()
	if code := runWithEmptyStdin([]string{"mkdir", "--config", cfg, "win/Documents/once"}, &out, &errOut); code == 0 {
		t.Fatal("the second mkdir = 0, want non-zero")
	}
}

// TestRmCLIDeletesTheFile pins the command.
func TestRmCLIDeletesTheFile(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"rm", "--config", cfg, "win/Documents/a.txt"}, &out, &errOut); code != 0 {
		t.Fatalf("run(rm) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file still exists (err = %v)", err)
	}
}

// TestRmCLIRefusesANonEmptyDirectory pins the safety property at the CLI, where
// a user would meet it.
func TestRmCLIRefusesANonEmptyDirectory(t *testing.T) {
	cfg, root := cliWriteFixture(t)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("bb"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"rm", "--config", cfg, "win/Documents/sub"}, &out, &errOut); code == 0 {
		t.Fatal("run(rm of a non-empty directory) = 0, want non-zero")
	}
	if got := diskContent(t, root, "sub/b.txt"); got != "bb" {
		t.Fatalf("the child was deleted: content = %q, want %q", got, "bb")
	}
}

// TestRmCLIOnAMissingPathFails pins that a path that is not there is reported.
func TestRmCLIOnAMissingPathFails(t *testing.T) {
	cfg, _ := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"rm", "--config", cfg, "win/Documents/nope.txt"}, &out, &errOut); code == 0 {
		t.Fatal("run(rm of a missing path) = 0, want non-zero")
	}
	if errOut.Len() == 0 {
		t.Fatal("stderr is empty")
	}
}

// TestMvCLIRenamesTheFile pins that both names move: the old one is gone and the
// new one holds the content.
func TestMvCLIRenamesTheFile(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	code := runWithEmptyStdin([]string{"mv", "--config", cfg, "win/Documents/a.txt", "win/Documents/b.txt"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("run(mv) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the old name still exists (err = %v)", err)
	}
	if got := diskContent(t, root, "b.txt"); got != "original" {
		t.Fatalf("content = %q, want %q", got, "original")
	}
}

// TestMvCLIAcrossClientsFails pins that a cross-client move is refused at the
// CLI rather than silently becoming a copy.
//
// The wording is asserted, not just the exit code. Without the refusal this
// request degrades into a self-rename the owner accepts — the two paths are
// stripped of their client names and arrive identical — so a non-zero exit on
// its own would not say the refusal happened for the right reason.
func TestMvCLIAcrossClientsFails(t *testing.T) {
	cfg, _ := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	code := runWithEmptyStdin([]string{"mv", "--config", cfg, "win/Documents/a.txt", "linux/Documents/a.txt"}, &out, &errOut)
	if code == 0 {
		t.Fatal("run(mv across clients) = 0, want non-zero")
	}
	got := errOut.String()
	if !strings.Contains(got, "win") || !strings.Contains(got, "linux") {
		t.Fatalf("stderr = %q, want the cross-client refusal naming both clients", got)
	}
}

// TestMvCLIWithoutTwoPathsIsAUsageError pins the argument count.
func TestMvCLIWithoutTwoPathsIsAUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"mv", "only-one"}, &out, &errOut); code != 2 {
		t.Fatalf("run(mv) = %d, want 2; stderr = %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "exactly two paths") {
		t.Fatalf("stderr = %q, want it to name the two paths", errOut.String())
	}
}

// TestWriteCLIUnreachableServerFails pins that every write command reports a
// server it cannot reach rather than hanging.
func TestWriteCLIUnreachableServerFails(t *testing.T) {
	cfg := writeConfig(t, "127.0.0.1:1")

	cases := []struct {
		what string
		args []string
	}{
		{"put", []string{"put", "--config", cfg, "win/Documents/x.txt"}},
		{"mkdir", []string{"mkdir", "--config", cfg, "win/Documents/d"}},
		{"rm", []string{"rm", "--config", cfg, "win/Documents/a.txt"}},
		{"mv", []string{"mv", "--config", cfg, "win/Documents/a.txt", "win/Documents/b.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run([]string(tc.args), strings.NewReader("x"), &out, &errOut); code == 0 {
				t.Fatalf("run(%s) = 0 against an unreachable server, want non-zero", tc.what)
			}
			if errOut.Len() == 0 {
				t.Fatalf("stderr is empty for %s", tc.what)
			}
		})
	}
}

// TestWriteCLIWithoutAConfiguredServerFails pins the no-server case for the
// writes, which must be a message rather than a panic.
func TestWriteCLIWithoutAConfiguredServerFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cases := []struct {
		what string
		args []string
	}{
		{"put", []string{"put", "--config", path, "win/Documents/x.txt"}},
		{"mkdir", []string{"mkdir", "--config", path, "win/Documents/d"}},
		{"rm", []string{"rm", "--config", path, "win/Documents/a.txt"}},
		{"mv", []string{"mv", "--config", path, "win/Documents/a.txt", "win/Documents/b.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run([]string(tc.args), strings.NewReader("x"), &out, &errOut); code == 0 {
				t.Fatalf("run(%s) = 0 with no server configured, want non-zero", tc.what)
			}
			if !strings.Contains(errOut.String(), "no server configured") {
				t.Fatalf("stderr = %q, want it to name the missing server", errOut.String())
			}
		})
	}
}

// TestPutCLIPassesBinaryStdinThrough pins that the content is not text-processed
// on the way in: NUL and invalid UTF-8 both survive.
func TestPutCLIPassesBinaryStdinThrough(t *testing.T) {
	cfg, root := cliWriteFixture(t)
	want := string([]byte{0x00, 0x01, 0xfe, 0xff, 0x80, 'a', 0x0a})

	var out, errOut bytes.Buffer
	if code := run([]string{"put", "--config", cfg, "win/Documents/bin.dat"}, strings.NewReader(want), &out, &errOut); code != 0 {
		t.Fatalf("run(put) = %d, want 0; stderr = %q", code, errOut.String())
	}
	if got := diskContent(t, root, "bin.dat"); got != want {
		t.Fatalf("content = %v, want %v", []byte(got), []byte(want))
	}
}

// TestPutCLIEmptyStdinMakesAnEmptyFile pins that a put of nothing is a real
// file, which is what makes `peerfold put x < /dev/null` meaningful.
func TestPutCLIEmptyStdinMakesAnEmptyFile(t *testing.T) {
	cfg, root := cliWriteFixture(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"put", "--config", cfg, "win/Documents/empty.bin"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("run(put) = %d, want 0; stderr = %q", code, errOut.String())
	}
	info, err := os.Stat(filepath.Join(root, "empty.bin"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("Size = %d, want 0", info.Size())
	}
}
