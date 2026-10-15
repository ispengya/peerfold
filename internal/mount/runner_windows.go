//go:build windows

package mount

import (
	"os"
	"path/filepath"
)

// DefaultMountPoint is %USERPROFILE%\peerfold.
func DefaultMountPoint() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "peerfold"), nil
}

// prepareMountPoint makes sure the mount point does NOT exist.
//
// WinFsp creates the directory itself when it mounts and removes it when the
// filesystem goes away -- the directory is opened with FILE_FLAG_DELETE_ON_CLOSE
// and the mount is a reparse point. If the directory is already there, WinFsp
// refuses with "mount point in use" and Mount returns false immediately.
//
// This was measured rather than assumed. An afternoon went into an elevation
// theory because the probe pre-created the directory; the cause was the probe.
//
// An empty existing directory is removed so that a previous run which died
// without cleaning up does not wedge every later one. A non-empty one is not
// touched: it is either somebody's data or a stale mount, and guessing between
// those two by deleting is the kind of recovery that makes an incident worse.
//
// cleanup is always nil. WinFsp owns the directory's lifetime, and a cleanup that
// removed it would be racing the thing that is already doing so.
func prepareMountPoint(path string) (cleanup func(), err error) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			// The normal case: WinFsp will create it.
			return nil, nil
		}
		return nil, errStr("mount: " + path + " cannot be used as a mount point: " +
			statErr.Error())
	}

	// The path is there. A file or a symlink is not a mount point and must not be
	// removed, whatever WinFsp would make of it; only a directory can be the empty
	// leftover this function exists to clear.
	if !info.IsDir() {
		return nil, errStr("mount: " + path + " is not a directory, so it cannot " +
			"be a mount point")
	}

	entries, readErr := os.ReadDir(path)
	if readErr != nil {
		return nil, errStr("mount: could not read " + path + ": " + readErr.Error())
	}
	if len(entries) != 0 {
		return nil, errStr("mount: " + path + " exists and is not empty; " +
			"it is either a stale mount point or a real directory, and neither " +
			"should be removed automatically")
	}

	// Empty. WinFsp needs it gone, so remove it and let WinFsp do the creating.
	if removeErr := os.Remove(path); removeErr != nil {
		return nil, errStr("mount: " + path + " is an empty directory that " +
			"could not be removed, and WinFsp needs it to be absent: " +
			removeErr.Error())
	}
	return nil, nil
}
