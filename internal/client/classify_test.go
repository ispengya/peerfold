package client

import (
	"os"
	"testing"

	"peerfold/internal/node"
)

// classify is tested here, in the internal test package, rather than from
// client_test.
//
// The reason is that the states it decides cannot all be produced by this
// host: creating a symlink needs Administrator or Developer Mode, and there is
// no way to create a junction that Go reports as non-directory from a test. The
// os.Symlink test in symlink_test.go has to skip on such a machine, so if the
// decision were only reachable through the filesystem it would ship
// unexercised. ModeSymlink and ModeIrregular are just bits, so the decision can
// be pinned without a filesystem.
//
// The repository already takes this approach elsewhere: internal/viewer/emit_test.go
// is in package viewer, and internal/e2e's tests are in package e2e.
func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		mode  os.FileMode
		isDir bool
		want  node.Kind
	}{
		{"regular file", 0o644, false, node.KindFile},
		{"regular file, no permission bits", 0, false, node.KindFile},
		{"directory", os.ModeDir | 0o755, true, node.KindDir},
		{"directory, no permission bits", os.ModeDir, true, node.KindDir},
		{"symlink", os.ModeSymlink | 0o777, false, node.KindSymlink},

		// A Windows junction. Go reports ModeIrregular and neither the dir bit
		// nor the symlink bit, and DirEntry.IsDir() is false because a junction
		// is not a directory to Lstat. This is the case that matters most in
		// practice: junctions are far more common than symlinks on Windows
		// (OneDrive, node_modules, "C:\Users\All Users"), and indexing one as
		// a regular file promises a size and a content that do not exist.
		{"junction (ModeIrregular)", os.ModeIrregular | 0o666, false, node.KindSymlink},
		{"junction with no permission bits", os.ModeIrregular, false, node.KindSymlink},

		// Other non-regular files. None of them can be read as a byte stream,
		// so none of them is a KindFile.
		{"named pipe", os.ModeNamedPipe | 0o644, false, node.KindSymlink},
		{"socket", os.ModeSocket | 0o644, false, node.KindSymlink},
		{"device", os.ModeDevice | 0o644, false, node.KindSymlink},
		{"char device", os.ModeCharDevice | os.ModeDevice | 0o644, false, node.KindSymlink},

		// The directory bit wins when both are set, because IsDir() is the
		// authoritative answer for directories and the check order says so.
		{"directory with irregular bit", os.ModeDir | os.ModeIrregular | 0o755, true, node.KindDir},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.mode, c.isDir); got != c.want {
				t.Errorf("classify(%v, isDir=%v) = %d, want %d", c.mode, c.isDir, got, c.want)
			}
		})
	}
}

// The property that makes the bug impossible rather than merely fixed: no mode
// carrying a type bit may be classified as a regular file. Scan and the watcher
// both go through classify, so one assertion covers both paths.
func TestNoTypeBitIsEverAFile(t *testing.T) {
	for _, mode := range []os.FileMode{
		os.ModeSymlink | 0o777,
		os.ModeIrregular | 0o666,
		os.ModeNamedPipe | 0o644,
		os.ModeSocket | 0o644,
		os.ModeDevice | 0o644,
		os.ModeCharDevice | os.ModeDevice | 0o644,
	} {
		for _, isDir := range []bool{false, true} {
			if got := classify(mode, isDir); got == node.KindFile {
				t.Errorf("classify(%v, isDir=%v) = KindFile; an entry with a type bit must never be indexed as a regular file", mode, isDir)
			}
		}
	}
}

// And the opposite direction: a plain file must never be classified as
// something the mount layer will refuse, or every ordinary file becomes
// unreadable.
func TestPlainFileIsStillAFile(t *testing.T) {
	for _, mode := range []os.FileMode{0, 0o644, 0o755, 0o600, 0o777} {
		if got := classify(mode, false); got != node.KindFile {
			t.Errorf("classify(%v, false) = %d, want KindFile", mode, got)
		}
	}
}
