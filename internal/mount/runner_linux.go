//go:build linux

package mount

import (
	"os"
	"path/filepath"
)

// platformMountArgs is empty on Linux: the kernel applies the mode Getattr
// reports directly, and there is no Windows DACL to synthesise.
func platformMountArgs() []string {
	return nil
}

// DefaultMountPoint is ~/peerfold.
func DefaultMountPoint() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "peerfold"), nil
}

// prepareMountPoint makes sure the mount point EXISTS and is empty.
//
// This is the opposite of Windows, and the inversion is the whole reason this
// lives in a platform file. Linux mounts a filesystem ON an existing directory,
// so the directory has to be there; WinFsp creates the directory itself and
// refuses if it already is. There is no portable rule that could be written once.
//
// A non-empty directory is refused rather than mounted over. Mounting hides the
// existing contents without warning, and from the operator's side "my files are
// gone" is indistinguishable from having lost them.
//
// cleanup is nil on purpose. The directory was created by the operator or on
// their behalf, and after an unmount the kernel leaves it in place; removing it
// would be this program deleting a directory it did not necessarily create, for
// no benefit, since the next mount would just make it again.
func prepareMountPoint(path string) (cleanup func(), err error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, errStr("mount: could not create the mount point " + path +
			": " + err.Error())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, errStr("mount: could not read the mount point " + path +
			": " + err.Error())
	}
	if len(entries) != 0 {
		return nil, errStr("mount: " + path + " exists and is not empty; " +
			"mounting here would hide its contents without warning")
	}
	return nil, nil
}
