//go:build linux

package mount

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrepareMountPointCreatesThePath is the Linux half of the divergence.
//
// Linux mounts a filesystem on top of an existing directory, so the directory has
// to be there. A mount point that does not exist cannot be mounted on at all.
func TestPrepareMountPointCreatesThePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "made", "for", "this")
	cleanup, err := prepareMountPoint(p)
	if err != nil {
		t.Fatalf("prepareMountPoint: %v", err)
	}
	if cleanup != nil {
		t.Errorf("prepareMountPoint returned a cleanup for a Linux mount point; the "+
			"directory is meant to survive an unmount, got %v", cleanup)
	}
	info, statErr := os.Stat(p)
	if statErr != nil {
		t.Fatalf("prepareMountPoint did not create the mount point: %v", statErr)
	}
	if !info.IsDir() {
		t.Error("the mount point is not a directory")
	}
}

// TestPrepareMountPointAcceptsAnExistingEmptyDirectory is the normal case on this
// platform: an operator made ~/peerfold and it is empty.
func TestPrepareMountPointAcceptsAnExistingEmptyDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mp")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareMountPoint(p); err != nil {
		t.Fatalf("prepareMountPoint refused an existing empty directory: %v", err)
	}
	if _, statErr := os.Stat(p); statErr != nil {
		t.Errorf("prepareMountPoint removed an existing empty directory: %v", statErr)
	}
}
