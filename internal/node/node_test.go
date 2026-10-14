package node_test

import (
	"testing"

	"peerfold/internal/node"
)

func TestSplitFirst(t *testing.T) {
	cases := []struct {
		in       string
		wantHead string
		wantRest string
	}{
		{"Documents/a/b.txt", "Documents", "a/b.txt"},
		{"Documents", "Documents", ""},
		{"a/b", "a", "b"},
		{"a/", "a", ""},
	}
	for _, c := range cases {
		head, rest := node.SplitFirst(c.in)
		if head != c.wantHead || rest != c.wantRest {
			t.Errorf("SplitFirst(%q) = (%q, %q), want (%q, %q)", c.in, head, rest, c.wantHead, c.wantRest)
		}
	}
}

func TestIsUnder(t *testing.T) {
	cases := []struct {
		parent string
		child  string
		want   bool
	}{
		{"/mnt/peerfold", "/mnt/peerfold", true},
		{"/mnt/peerfold", "/mnt/peerfold/win-laptop/x", true},
		// Same prefix but not a subpath: the case a string-prefix comparison
		// gets wrong.
		{"/mnt/peerfold", "/mnt/peerfoldX", false},
		{"/mnt/peerfold", "/mnt/other", false},
		{"/mnt/peerfold/sub", "/mnt/peerfold", false},
		// A real entry may be named "..foo". It is not an escape, and a test
		// of the first two bytes gets this wrong.
		{"/mnt/peerfold", "/mnt/peerfold/..foo", true},
		{"/mnt/peerfold", "/mnt/peerfold/..foo/bar", true},
		// filepath.Rel cleans both arguments first, so a ".." that cancels out
		// is resolved before the comparison and never reaches the ".."
		// component test. Recording it here keeps that from being mistaken for
		// an escape that IsUnder is supposed to catch; the path that does
		// escape has to survive cleaning.
		{"/mnt/peerfold", "/mnt/peerfold/..foo/../x", true},
		{"/mnt/peerfold", "/mnt/peerfold/../../../etc/passwd", false},
		// A trailing separator on the parent is not a different directory.
		{"/mnt/peerfold/", "/mnt/peerfold/x", true},
		{"/mnt/peerfold", "/mnt/peerfold/./x", true},
	}
	for _, c := range cases {
		if got := node.IsUnder(c.parent, c.child); got != c.want {
			t.Errorf("IsUnder(%q, %q) = %v, want %v", c.parent, c.child, got, c.want)
		}
	}
}

func TestKindValues(t *testing.T) {
	// These values travel on the wire inside NodeJSON.Kind, so they are part of
	// the protocol, not an implementation detail.
	if node.KindFile != 0 {
		t.Errorf("KindFile = %d, want 0", node.KindFile)
	}
	if node.KindDir != 1 {
		t.Errorf("KindDir = %d, want 1", node.KindDir)
	}
	// The deletion marker is a Kind value, not a separate concept: Task 4's
	// index applies a node carrying it as a removal, and Task 10 emits it.
	if node.KindDeleted != 255 {
		t.Errorf("KindDeleted = %d, want 255", node.KindDeleted)
	}
}
