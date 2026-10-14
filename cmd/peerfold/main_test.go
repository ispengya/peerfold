package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer

	code := run([]string{"bogus"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("run([bogus]) = 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("stderr = %q, want it to contain %q", errOut.String(), "unknown command")
	}
}

func TestRunNoArgsListsCommands(t *testing.T) {
	var out, errOut bytes.Buffer

	if code := run(nil, &out, &errOut); code == 0 {
		t.Fatalf("run(nil) = 0, want non-zero")
	}
	s := out.String()
	for _, want := range []string{"server", "client"} {
		if !strings.Contains(s, want) {
			t.Fatalf("stdout = %q, want it to mention %q", s, want)
		}
	}
}

// TestRunStubSubcommandSucceeds pins this task's stub behaviour. The
// "missing PATH is an error" assertion belongs to Task 7, which owns the
// real ls implementation.
func TestRunStubSubcommandSucceeds(t *testing.T) {
	var out, errOut bytes.Buffer

	if code := run([]string{"ls", "/nonexistent"}, &out, &errOut); code != 0 {
		t.Fatalf("run([ls /nonexistent]) = %d, want 0 from the stub; stderr = %q", code, errOut.String())
	}
}
