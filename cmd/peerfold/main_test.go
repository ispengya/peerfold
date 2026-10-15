package main

import (
	"bytes"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peerfold/internal/client"
)

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer

	code := runWithEmptyStdin([]string{"bogus"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("runWithEmptyStdin([bogus]) = 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("stderr = %q, want it to contain %q", errOut.String(), "unknown command")
	}
}

func TestRunNoArgsListsCommands(t *testing.T) {
	var out, errOut bytes.Buffer

	if code := runWithEmptyStdin(nil, &out, &errOut); code == 0 {
		t.Fatalf("runWithEmptyStdin(nil) = 0, want non-zero")
	}
	s := out.String()
	for _, want := range []string{"server", "client"} {
		if !strings.Contains(s, want) {
			t.Fatalf("stdout = %q, want it to mention %q", s, want)
		}
	}
}

// TestRunServerServesAndStops pins that `peerfold server` really listens and
// stops on an interrupt, rather than reporting pending work.
//
// It is driven through cmdServerWith because cmdServer waits on the process's
// own interrupt signal, which a test cannot send to itself.
func TestRunServerServesAndStops(t *testing.T) {
	var out, errOut bytes.Buffer
	interrupt := make(chan struct{})

	cfg := filepath.Join(t.TempDir(), "config.json")
	done := make(chan int, 1)
	go func() {
		done <- cmdServerWith([]string{"--listen", "127.0.0.1:0", "--config", cfg}, &out, &errOut, interrupt)
	}()

	// The address is printed once the listener is bound, so this waits for the
	// command to be serving rather than guessing how long binding takes.
	addr := waitForAddress(t, &out)

	c, err := client.Dial(client.Options{Server: addr, Name: "probe"})
	if err != nil {
		t.Fatalf("Dial(%s): %v: the command did not start a working server", addr, err)
	}
	defer c.Close()
	if err := c.UploadTree(); err != nil {
		t.Fatalf("UploadTree: %v", err)
	}

	close(interrupt)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdServerWith = %d, want 0; stderr = %q", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not stop after being interrupted")
	}
	if !strings.Contains(out.String(), "stopped") {
		t.Errorf("stdout = %q, want it to report that it stopped", out.String())
	}
}

// TestRunServerReportsWhyThePortIsUnavailable pins that a failed bind says what
// the operating system said.
//
// The command used to print "address is unavailable" for every listen failure,
// because it only had Addr() to look at and that is nil when the bind fails. A
// user whose port is taken needs to be told that, not that something was
// unavailable: the two most common failures — the port is in use, and the
// address is malformed — have completely different fixes.
func TestRunServerReportsWhyThePortIsUnavailable(t *testing.T) {
	var out, errOut bytes.Buffer
	interrupt := make(chan struct{})

	// Occupy a port so the bind has a specific, knowable reason to fail.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	cfg := filepath.Join(t.TempDir(), "config.json")
	code := cmdServerWith([]string{"--listen", ln.Addr().String(), "--config", cfg}, &out, &errOut, interrupt)

	if code != 1 {
		t.Fatalf("cmdServerWith on a taken port = %d, want 1; stderr = %q", code, errOut.String())
	}
	// The operating system's words, not a paraphrase. The wording differs by
	// platform — "address already in use" on Unix, "Only one usage of each
	// socket address ..." on Windows — so this accepts either rather than
	// pinning one, while still requiring that the reason reached the user.
	got := errOut.String()
	if !strings.Contains(got, "in use") && !strings.Contains(got, "Only one usage") {
		t.Fatalf("stderr = %q, want the operating system's reason for the failed bind", got)
	}
	if !strings.Contains(got, ln.Addr().String()) {
		t.Fatalf("stderr = %q, want it to name the address %s", got, ln.Addr())
	}
	if strings.Contains(out.String(), "listening on") {
		t.Fatalf("stdout = %q, want no claim of listening after a failed bind", out.String())
	}
}

// waitForAddress reads the "listening on <addr>" line the server prints.
func waitForAddress(t *testing.T, out *bytes.Buffer) string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(out.String(), "\n") {
			if addr, ok := strings.CutPrefix(line, "listening on "); ok {
				return strings.TrimSpace(addr)
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no address was printed: stdout = %q", out.String())
	return ""
}

// TestRunDispatchesEveryKnownCommand pins every dispatch arm: a typo in a case
// label would otherwise still pass every other test, because the usage text
// alone is enough to satisfy TestRunNoArgsListsCommands.
//
// The commands that are still stubs answer with the pending-work notice. The
// implemented ones are expected to get past the dispatch and fail on their own
// missing arguments instead, which is a different code path from the default
// arm. Every command in usageText must appear in exactly one of the two lists,
// so a command that becomes real has to be moved rather than silently dropped.
//
// There are no stubs left, so the first list is empty and is kept only as the
// place a future stub goes.
func TestRunDispatchesEveryKnownCommand(t *testing.T) {
	_ = []string{} // stubs, if any return

	// These reach their real implementations, which reject the missing input.
	//
	// `mount` is driven through its own missing-server rejection rather than the
	// plain-args loop here, because with no arguments it would fall back to the
	// real config path and a real default mount point; the loop below stays for
	// the commands whose bare form is safely inert.
	for _, cmd := range []string{"client", "ls", "cat", "put", "mkdir", "rm", "mv"} {
		var out, errOut bytes.Buffer
		if code := runWithEmptyStdin([]string{cmd}, &out, &errOut); code == 0 {
			t.Fatalf("runWithEmptyStdin([%s]) = 0, want non-zero", cmd)
		}
		if strings.Contains(errOut.String(), "TODO") {
			t.Fatalf("runWithEmptyStdin([%s]) stderr = %q, want the real implementation rather than the stub notice", cmd, errOut.String())
		}
	}

	// `server` cannot be driven this way: it is a long-running command, so
	// reaching its implementation means it would listen rather than return, and
	// a test that called it would hang instead of failing. Its dispatch arm is
	// pinned here through a bad flag, which is refused before anything binds,
	// and its real behaviour by TestRunServerServesAndStops.
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"server", "--not-a-flag"}, &out, &errOut); code != 2 {
		t.Fatalf("runWithEmptyStdin([server --not-a-flag]) = %d, want 2 from the flag parser; stderr = %q", code, errOut.String())
	}
	if strings.Contains(errOut.String(), "TODO") {
		t.Fatalf("runWithEmptyStdin([server]) stderr = %q, want the real implementation rather than the stub notice", errOut.String())
	}
	if !strings.Contains(errOut.String(), "usage: peerfold server") {
		t.Errorf("stderr = %q, want the server usage text: the dispatch arm did not reach cmd_server.go", errOut.String())
	}

	// `mount` reaches for the network, so it is driven with a server address that
	// cannot answer rather than with no arguments -- and the point is the code
	// path, not the failure: a missing dispatch arm would be refused as an unknown
	// command before it ever dialled.
	var mOut, mErr bytes.Buffer
	if code := runWithEmptyStdin([]string{"mount", "--server", "127.0.0.1:1", "--mount", filepath.Join(t.TempDir(), "mp")}, &mOut, &mErr); code == 2 && strings.Contains(mErr.String(), "unknown command") {
		t.Errorf("runWithEmptyStdin([mount]) = %q, want the command to be dispatched", mErr.String())
	}
	if strings.Contains(mErr.String(), "TODO") {
		t.Errorf("runWithEmptyStdin([mount]) stderr = %q, want the real implementation", mErr.String())
	}
}
