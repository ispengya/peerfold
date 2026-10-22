package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A share can legitimately contain a symlink pointing at a directory that is
// not part of the share. The mount refuses such entries, but the wire is a
// second consumer: it addresses the owner's filesystem directly, so the owner
// has to enforce the same boundary. This drives every operation through a real
// server and client rather than calling the filesystem layer itself.
func TestE2EAnEscapingSymlinkIsRefusedOnEveryOperation(t *testing.T) {
	_, addr := startServer(t)
	root := share(t, "Documents", map[string]string{"safe.txt": "safe"})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		filepath.Join(outside, "secret.txt"),
		filepath.Join(root, "escape.txt"),
	); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape-dir")); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	if err := os.Symlink(
		filepath.Join(root, "safe.txt"),
		filepath.Join(root, "inside-link.txt"),
	); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}

	join(t, addr, "win", "Documents="+root)
	v := dialViewer(t, addr)

	// A link that stays inside the share is still readable: the guard rejects
	// escapes, not links.
	if got, err := v.Cat("win/Documents/inside-link.txt"); err != nil || string(got) != "safe" {
		t.Fatalf("Cat(inside-link.txt) = %q, %v; an inside link must keep working", got, err)
	}

	if got, err := v.Cat("win/Documents/escape.txt"); err == nil {
		t.Fatalf("Cat(escape.txt) = %q, nil; a link out of the share must be refused", got)
	}
	if _, err := v.Read("win/Documents/escape-dir/secret.txt", 0, 100); err == nil {
		t.Error("Read followed a directory link out of the share")
	}
	if err := v.Put("win/Documents/escape-dir/new.txt", strings.NewReader("payload")); err == nil {
		t.Error("Put wrote through a directory link out of the share")
	}
	if err := v.Mkdir("win/Documents/escape-dir/newdir"); err == nil {
		t.Error("Mkdir created a directory outside the share")
	}
	if err := v.Remove("win/Documents/escape-dir/secret.txt"); err == nil {
		t.Error("Remove deleted a file outside the share")
	}

	if b, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(b) != "secret" {
		t.Fatalf("outside/secret.txt = %q, %v; an escape attempt changed it", b, err)
	}
	for _, name := range []string{"new.txt", "newdir"} {
		if _, err := os.Lstat(filepath.Join(outside, name)); !os.IsNotExist(err) {
			t.Errorf("outside/%s exists after a refused operation (err=%v)", name, err)
		}
	}
}
