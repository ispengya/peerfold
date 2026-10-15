package mount

import (
	"testing"

	"peerfold/internal/node"
)

func TestCleanRejectsEscape(t *testing.T) {
	for _, in := range []string{"/../etc/passwd", "/a/../../b", "..", "/.."} {
		if got, err := Clean(in); err == nil {
			t.Errorf("Clean(%q) = %q, want an error (it escapes the root)", in, got)
		}
	}
}

func TestCleanAccepts(t *testing.T) {
	cases := map[string]string{
		"/":          "/",
		"":           "/",
		"/a":         "/a",
		"/a/b.txt":   "/a/b.txt",
		"/a//b":      "/a/b",
		"/a/./b":     "/a/b",
		"/a/b/":      "/a/b",
		"/中文/文件.txt": "/中文/文件.txt",
	}
	for in, want := range cases {
		got, err := Clean(in)
		if err != nil {
			t.Errorf("Clean(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitParent(t *testing.T) {
	parent, base, err := SplitParent("/a/b.txt")
	if err != nil || parent != "/a" || base != "b.txt" {
		t.Errorf("SplitParent(/a/b.txt) = %q, %q, %v", parent, base, err)
	}
	if _, _, err := SplitParent("/"); err == nil {
		t.Error("SplitParent(/) must fail: the root has no parent")
	}
}

func TestLookupRejectsSymlink(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addDir("/a")
	f.addSymlink("/a/link")
	f.addFile("/a/real.txt", 3)

	if _, code := m.lookup("/a/link"); code != -ELOOP {
		t.Errorf("lookup(symlink) = %d, want -ELOOP (%d)", code, -ELOOP)
	}
	if _, code := m.lookup("/a/real.txt"); code != 0 {
		t.Errorf("lookup(regular) = %d, want 0", code)
	}
}

func TestLookupMissingAndNotDir(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a/real.txt", 3)

	if _, code := m.lookup("/a/nope"); code != -ENOENT {
		t.Errorf("lookup(missing) = %d, want -ENOENT", code)
	}
	if _, code := m.lookup("/a/real.txt/x"); code != -ENOTDIR {
		t.Errorf("lookup(under file) = %d, want -ENOTDIR", code)
	}
}

func TestLookupRoot(t *testing.T) {
	m := newTestMount(t)
	n, code := m.lookup("/")
	if code != 0 || n.Kind != node.KindDir {
		t.Errorf("lookup(/) = %+v, %d; want a directory and 0", n, code)
	}
	// The root's mode comes from the options, since it is synthesised and there
	// is no index entry to read one from.
	if n.Mode != m.opts.RootMode {
		t.Errorf("lookup(/) mode = %o, want the configured %o", n.Mode, m.opts.RootMode)
	}
}

// The walk descends one component at a time, so a deep path exercises every
// step of it. A deep path also exposes an asymmetry in the layer below that is
// worth pinning rather than assuming, because it is narrower than POSIX:
// ENOTDIR is reported when the immediate parent is a file, and ENOENT when a
// file sits further up the path. See TestFakeListMirrorsTheIndex for the
// measurement that establishes it and internal/index/index.go for why.
func TestLookupWalksEveryComponent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a/b/c/d.txt", 4)

	for _, p := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d.txt"} {
		n, code := m.lookup(p)
		if code != 0 {
			t.Errorf("lookup(%q) = %d, want 0", p, code)
			continue
		}
		if n.Path != p {
			t.Errorf("lookup(%q) resolved to %q", p, n.Path)
		}
	}

	// Every prefix of an existing path must exist, and a name under a
	// directory that does not exist is ENOENT no matter how deep it is.
	for _, p := range []string{"/zzz", "/zzz/yyy", "/zzz/yyy/xxx"} {
		if _, code := m.lookup(p); code != -ENOENT {
			t.Errorf("lookup(%q) = %d, want -ENOENT", p, code)
		}
	}

	// One level under a file: the parent IS the file, so the parent cannot be
	// listed and ENOTDIR is the answer.
	if _, code := m.lookup("/a/b/c/d.txt/x"); code != -ENOTDIR {
		t.Errorf("lookup(file parent) = %d, want -ENOTDIR", code)
	}
	// Two levels under a file. The parent "/a/b/c/d.txt/x" does not exist as a
	// node, so the index answers "not found" and that is what comes back. It
	// is not ENOTDIR, and a test demanding ENOTDIR here would be demanding
	// behaviour the real stack cannot produce.
	if _, code := m.lookup("/a/b/c/d.txt/x/y"); code != -ENOENT {
		t.Errorf("lookup(two levels under a file) = %d, want -ENOENT: "+
			"the index does not walk ancestors, so only the immediate parent is diagnosed", code)
	}
}

// A name must match exactly. A child whose name ends with the segment's name
// must not answer for it, which is what a suffix comparison would have done.
func TestLookupMatchesWholeNames(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/ab", 1)

	if _, code := m.lookup("/b"); code != -ENOENT {
		t.Errorf("lookup(/b) = %d, want -ENOENT: /ab must not answer for /b", code)
	}
	if _, code := m.lookup("/ab"); code != 0 {
		t.Errorf("lookup(/ab) = %d, want 0", code)
	}
}

// A missing path under a directory that is itself missing is ENOENT, not
// ENOTDIR, even though both are failures of the same walk. Getting this wrong
// makes "mkdir -p /a/b" impossible, because the shell would report the wrong
// reason and stop.
func TestMissingParentIsENOENTNotENOTDIR(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/exists/file.txt", 1)

	if _, code := m.lookup("/missing/file.txt"); code != -ENOENT {
		t.Errorf("lookup(missing parent) = %d, want -ENOENT", code)
	}
}

// lookupParent is what every mutating operation uses, so it has to refuse the
// root, refuse a missing parent, and refuse a parent that is a file.
func TestLookupParent(t *testing.T) {
	m := newTestMount(t)
	f := fakeOf(t, m)
	f.addFile("/a/real.txt", 3)

	t.Run("happy path", func(t *testing.T) {
		n, base, code := m.lookupParent("/a/new.txt")
		if code != 0 || base != "new.txt" || n.Path != "/a" {
			t.Errorf("lookupParent = %+v, %q, %d; want /a, new.txt, 0", n, base, code)
		}
	})

	t.Run("root has no parent", func(t *testing.T) {
		if _, _, code := m.lookupParent("/"); code != -EINVAL {
			t.Errorf("lookupParent(/) = %d, want -EINVAL", code)
		}
	})

	t.Run("missing parent", func(t *testing.T) {
		if _, _, code := m.lookupParent("/nope/new.txt"); code != -ENOENT {
			t.Errorf("lookupParent(missing parent) = %d, want -ENOENT", code)
		}
	})

	t.Run("parent is a file", func(t *testing.T) {
		if _, _, code := m.lookupParent("/a/real.txt/new.txt"); code != -ENOTDIR {
			t.Errorf("lookupParent(file parent) = %d, want -ENOTDIR", code)
		}
	})

	t.Run("escaping path", func(t *testing.T) {
		if _, _, code := m.lookupParent("/a/../../etc/passwd"); code != -EINVAL {
			t.Errorf("lookupParent(escaping) = %d, want -EINVAL", code)
		}
	})
}

func TestCleanRejectsRelative(t *testing.T) {
	if _, err := Clean("a/b"); err == nil {
		t.Error("Clean accepted a relative path; FUSE paths are absolute")
	}
}

func TestSplitParentNonRoot(t *testing.T) {
	cases := []struct{ in, parent, base string }{
		{"/a", "/", "a"},
		{"/a/b", "/a", "b"},
		{"/a/b/c", "/a/b", "c"},
		{"/中文/文件", "/中文", "文件"},
	}
	for _, c := range cases {
		parent, base, err := SplitParent(c.in)
		if err != nil || parent != c.parent || base != c.base {
			t.Errorf("SplitParent(%q) = %q, %q, %v; want %q, %q", c.in, parent, base, err, c.parent, c.base)
		}
	}
}
