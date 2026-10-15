package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"peerfold/internal/errtoken"
)

// The classifier is the producing half of the token convention, so its output
// is what the mount layer reads. It is tested here, in the package that owns
// it, rather than through a mount: the mount reaches these categories only
// after a round trip through a wire.
func TestTokenForOSError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"missing file", &os.PathError{Op: "open", Path: "a", Err: syscall.ENOENT}, errtoken.NotFound},
		{"permission denied", &os.PathError{Op: "open", Path: "a", Err: syscall.EACCES}, errtoken.Permission},
		{"already exists", &os.PathError{Op: "mkdir", Path: "a", Err: syscall.EEXIST}, errtoken.NotEmpty},
		// A non-empty directory is ENOTEMPTY on Linux and ERROR_DIR_NOT_EMPTY
		// on Windows. Go maps both onto os.ErrExist, so it classifies the same
		// way on both platforms; syscall.ENOTEMPTY itself would not, because on
		// Windows it holds a unix-style value that no real errno equals.
		{"directory not empty", &os.PathError{Op: "remove", Path: "a", Err: syscall.ENOTEMPTY}, errtoken.NotEmpty},
		// Something with no category: it must pass through untouched, because
		// errnoFor turns an unclassified error into EIO and that is the honest
		// answer.
		{"unclassified", errors.New("the device is not ready"), ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := tokenForOSError(c.err)
			if c.want == "" {
				if got != c.err {
					t.Errorf("tokenForOSError returned %v, want the error unchanged", got)
				}
				return
			}
			if !errors.Is(got, c.err) {
				t.Errorf("the token broke the chain: errors.Is(%v, original) is false", got)
			}
			if !hasToken(got, c.want) {
				t.Errorf("tokenForOSError(%v) = %q, want the %q token", c.err, got, c.want)
			}
			// The original error's own classification must survive too, since
			// the layers above still use os.ErrNotExist.
			if errors.Is(c.err, fs.ErrNotExist) && !errors.Is(got, fs.ErrNotExist) {
				t.Errorf("%v no longer satisfies os.ErrNotExist", got)
			}
		})
	}
}

func hasToken(err error, token string) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	if len(text) < len(token)+2 {
		return false
	}
	for i := 0; i+len(token)+2 <= len(text); i++ {
		if text[i:i+len(token)] == token && text[i+len(token):i+len(token)+2] == ": " {
			return true
		}
	}
	return false
}

// The filesystem operations must actually use the classifier. Each of these is
// a category the mount layer has to tell apart, and a missing call is invisible:
// the error still arrives, it just arrives as EIO.
func TestFSOperationsTagTheirErrors(t *testing.T) {
	root := t.TempDir()
	sh, err := NewShares([]string{"S=" + root}, nil)
	if err != nil {
		t.Fatalf("NewShares: %v", err)
	}
	fs := NewFS(sh)

	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("read a missing file is notfound", func(t *testing.T) {
		_, _, err := fs.ReadAt("S", "nope.txt", 0, 10)
		if !hasToken(err, errtoken.NotFound) {
			t.Errorf("ReadAt(missing) = %v, want the %s token", err, errtoken.NotFound)
		}
	})

	t.Run("create over an existing name is notempty", func(t *testing.T) {
		err := fs.Create("S", "file.txt")
		if !hasToken(err, errtoken.NotEmpty) {
			t.Errorf("Create(existing) = %v, want the %s token", err, errtoken.NotEmpty)
		}
	})

	t.Run("mkdir over an existing name is notempty", func(t *testing.T) {
		err := fs.Mkdir("S", "dir")
		if !hasToken(err, errtoken.NotEmpty) {
			t.Errorf("Mkdir(existing) = %v, want the %s token", err, errtoken.NotEmpty)
		}
	})

	t.Run("rename a missing path is notfound", func(t *testing.T) {
		err := fs.Rename("S", "nope.txt", "other.txt")
		if !hasToken(err, errtoken.NotFound) {
			t.Errorf("Rename(missing) = %v, want the %s token", err, errtoken.NotFound)
		}
	})

	t.Run("remove a non-empty directory is notempty", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(root, "dir", "inside.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := fs.Remove("S", "dir")
		if !hasToken(err, errtoken.NotEmpty) {
			t.Errorf("Remove(non-empty) = %v, want the %s token", err, errtoken.NotEmpty)
		}
		// The contents must be untouched: the refusal is what protects them.
		if _, statErr := os.Stat(filepath.Join(root, "dir", "inside.txt")); statErr != nil {
			t.Errorf("the refusal damaged the directory: %v", statErr)
		}
	})

	t.Run("remove a missing path is notfound", func(t *testing.T) {
		err := fs.Remove("S", "nope.txt")
		if !hasToken(err, errtoken.NotFound) {
			t.Errorf("Remove(missing) = %v, want the %s token", err, errtoken.NotFound)
		}
	})
}
