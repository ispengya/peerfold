//go:build windows

package mount

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareMountPointRequiresAnEmptyDirectoryToBeCleared is Windows's
// housekeeping rule: an empty directory left behind by a run that died is
// removed, because WinFsp creates the directory itself and refuses if it is
// already there.
func TestPrepareMountPointRequiresAnEmptyDirectoryToBeCleared(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mp")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanup, err := prepareMountPoint(p)
	if err != nil {
		t.Fatalf("prepareMountPoint refused an empty directory: %v", err)
	}
	if cleanup != nil {
		t.Error("prepareMountPoint returned a cleanup for a Windows mount point")
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Errorf("an empty mount point survived prepareMountPoint (stat err %v); "+
			"WinFsp refuses with \"mount point in use\" when the directory "+
			"already exists, so a leftover from a crashed run wedges every later "+
			"mount on that path", statErr)
	}
}

// TestPrepareMountPointRequiresThePathToBeAbsent is the same invariant reached
// from the other side: a path that was never there must stay absent.
//
// WinFsp creates the mount directory itself and refuses with "mount point in use"
// if it is already there. So the invariant this platform must hold is the opposite
// of Linux's: after prepareMountPoint succeeds, the path must NOT exist.
func TestPrepareMountPointRequiresThePathToBeAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mp")

	cleanup, err := prepareMountPoint(p)
	if err != nil {
		t.Fatalf("prepareMountPoint refused an absent path: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Errorf("the mount point exists after prepareMountPoint (stat err %v); "+
			"WinFsp creates the directory itself", statErr)
	}
}

// TestPrepareMountPointAcceptsAnAbsentPath is the normal case: the directory is
// not there and WinFsp will make it.
func TestPrepareMountPointAcceptsAnAbsentPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-yet")
	cleanup, err := prepareMountPoint(p)
	if err != nil {
		t.Fatalf("prepareMountPoint refused an absent path: %v", err)
	}
	if cleanup != nil {
		t.Error("prepareMountPoint returned a cleanup for a Windows mount point; " +
			"WinFsp owns that directory's lifetime and a cleanup would race it")
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Errorf("prepareMountPoint created the mount point; WinFsp must create it "+
			"itself (stat err %v)", statErr)
	}
}

// TestPrepareMountPointRefusesAFileAsTheMountPoint covers the case that is not a
// directory at all. Reading it fails, and the refusal must not remove it: it is a
// file somebody put there.
func TestPrepareMountPointRefusesAFileAsTheMountPoint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareMountPoint(p); err == nil {
		t.Error("prepareMountPoint accepted a regular file as a mount point")
	}
	if _, statErr := os.Stat(p); statErr != nil {
		t.Errorf("prepareMountPoint removed a file it refused: %v", statErr)
	}
}
