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

// TestRunStubSubcommandSucceeds pins the write commands' stub behaviour. Those
// commands report pending work until Tasks 8/9 implement them. client, server
// and ls are excluded: they are real now, so they fail on their own missing
// arguments instead of printing a stub notice.
func TestRunStubSubcommandSucceeds(t *testing.T) {
	var out, errOut bytes.Buffer

	if code := run([]string{"cat", "/nonexistent"}, &out, &errOut); code != 0 {
		t.Fatalf("run([cat /nonexistent]) = %d, want 0 from the stub; stderr = %q", code, errOut.String())
	}
}

// TestRunDispatchesEveryKnownCommand pins every dispatch arm: a typo in a
// case label would otherwise still pass every other test, because the usage
// text alone is enough to satisfy TestRunNoArgsListsCommands.
//
// The commands that are still stubs answer with the pending-work notice. The
// implemented ones are expected to get past the dispatch and fail on their own
// missing arguments instead, which is a different code path from the default
// arm.
func TestRunDispatchesEveryKnownCommand(t *testing.T) {
	for _, cmd := range []string{"put", "mkdir", "rm", "mv"} {
		var out, errOut bytes.Buffer

		if code := run([]string{cmd}, &out, &errOut); code != 0 {
			t.Fatalf("run([%s]) = %d, want 0 from the stub; stderr = %q", cmd, code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "TODO") {
			t.Fatalf("run([%s]) stderr = %q, want the pending-work notice — did it fall through to the default arm?",
				cmd, errOut.String())
		}
	}

	// These reach their real implementations, which reject the missing input.
	for _, cmd := range []string{"client", "ls"} {
		var out, errOut bytes.Buffer
		if code := run([]string{cmd}, &out, &errOut); code == 0 {
			t.Fatalf("run([%s]) = 0, want non-zero", cmd)
		}
		if strings.Contains(errOut.String(), "TODO") {
			t.Fatalf("run([%s]) stderr = %q, want the real implementation rather than the stub notice", cmd, errOut.String())
		}
	}
}
