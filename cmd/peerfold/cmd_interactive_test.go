package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/config"
)

// TestInteractiveMountSavesConfig pins that a no-flag `peerfold mount` asks
// for the server and mount point, writes both into the config, and then
// reports the values it is about to use — so the next run can skip the
// questions. It must not actually mount: the test stops before dialing.
func TestInteractiveMountSavesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix

	mountPoint := filepath.Join(home, "mnt")
	input := strings.NewReader("127.0.0.1:7000\n" + mountPoint + "\n")
	var out, errOut bytes.Buffer

	// runMount will try to dial and fail; that is fine. What matters is that
	// the config was written before the dial.
	code := interactiveMount(input, &out, &errOut)

	cfgPath := filepath.Join(home, "peerfold-server", "config.json")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load config after interactive mount (code=%d, out=%q err=%q): %v",
			code, out.String(), errOut.String(), err)
	}
	if cfg.Server != "127.0.0.1:7000" {
		t.Errorf("Server = %q, want 127.0.0.1:7000", cfg.Server)
	}
	if cfg.MountPoint != mountPoint {
		t.Errorf("MountPoint = %q, want %q", cfg.MountPoint, mountPoint)
	}
	if !strings.Contains(out.String(), "Config saved") {
		t.Errorf("output missing Config saved:\n%s", out.String())
	}
}

// TestInteractiveMountRejectsRelativePoint pins that a relative mount point
// is refused interactively as well as on the command line: a mount that
// follows the working directory is not a location the user can find later.
func TestInteractiveMountRejectsRelativePoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	input := strings.NewReader("127.0.0.1:7000\nrelative/path\n")
	var out, errOut bytes.Buffer
	if code := interactiveMount(input, &out, &errOut); code == 0 {
		t.Fatal("interactiveMount with a relative mount point returned 0, want non-zero")
	}
	if !strings.Contains(errOut.String(), "absolute") {
		t.Errorf("stderr missing absolute-path complaint:\n%s", errOut.String())
	}
	// Nothing should have been saved: the rejection happens before Save.
	if _, err := os.Stat(filepath.Join(home, "peerfold-server", "config.json")); err == nil {
		t.Error("config was written despite a rejected mount point")
	}
}
