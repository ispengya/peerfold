package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/config"
)

// TestDialAddress pins what the interactive server writes as the local dial
// address for the listen address it was given. A wildcard bind is not a
// dialable address everywhere, and ls/cat/mount read this value.
func TestDialAddress(t *testing.T) {
	cases := map[string]string{
		":7000":          "127.0.0.1:7000",
		"0.0.0.0:7000":   "127.0.0.1:7000",
		"[::]:7000":      "127.0.0.1:7000",
		"127.0.0.1:7000": "127.0.0.1:7000",
		"10.0.0.13:7000": "10.0.0.13:7000",
	}
	for listen, want := range cases {
		if got := dialAddress(listen); got != want {
			t.Errorf("dialAddress(%q) = %q, want %q", listen, got, want)
		}
	}
}

// TestInteractiveMountSavesConfig pins that a no-flag `peerfold mount` asks
// for the server and mount point, writes both into the config, and then
// reports the values it is about to use — so the next run can skip the
// questions. It must not actually mount: the test stops before dialing.
func TestInteractiveMountSavesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix

	mountPoint := filepath.Join(home, "mnt")
	// Port 1 is closed on any sane host, so the dial fails immediately. Using
	// the default port would let this test mount for real on a machine that is
	// already running a server, and then block in the FUSE loop.
	input := strings.NewReader("\n127.0.0.1:1\n" + mountPoint + "\n")
	var out, errOut bytes.Buffer

	// runMount will try to dial and fail; that is fine. What matters is that
	// the config was written before the dial.
	code := interactiveMount(input, &out, &errOut)

	cfgPath, err := config.MountPath()
	if err != nil {
		t.Fatalf("MountPath: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load config after interactive mount (code=%d, out=%q err=%q): %v",
			code, out.String(), errOut.String(), err)
	}
	if cfg.Server != "127.0.0.1:1" {
		t.Errorf("Server = %q, want 127.0.0.1:1", cfg.Server)
	}
	if cfg.MountPoint != mountPoint {
		t.Errorf("MountPoint = %q, want %q", cfg.MountPoint, mountPoint)
	}
	if !strings.Contains(out.String(), "Config saved") {
		t.Errorf("output missing Config saved:\n%s", out.String())
	}
}

// TestInteractiveClientForegroundStartsWithoutDaemonFlag pins that the
// foreground path of the interactive client hands cmdClient arguments its flag
// set accepts. --daemon is an internal marker for the background child and is
// stripped by dispatch, but the foreground path calls cmdClient directly, so
// passing it made `peerfold client` exit with
// "flag provided but not defined: -daemon" instead of starting.
//
// The server address is a closed port on purpose: the test only needs the
// client to get as far as dialing, which proves the flags parsed.
func TestInteractiveClientForegroundStartsWithoutDaemonFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix

	share := filepath.Join(home, "docs")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, "peerfold-client-alice", "config.json")
	if err := config.Save(cfgPath, config.Config{
		Server:     "127.0.0.1:1",
		ClientName: "alice",
		Shares:     map[string]string{"docs": share},
	}); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := interactiveClient(strings.NewReader("1\n"), &out, &errOut)

	if strings.Contains(errOut.String(), "flag provided but not defined") {
		t.Fatalf("foreground client passed --daemon to cmdClient:\n%s", errOut.String())
	}
	if code == 0 {
		t.Fatalf("client with an unreachable server returned 0; out=%q err=%q",
			out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "Starting client 'alice'") {
		t.Errorf("output missing start message:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "127.0.0.1:1") {
		t.Errorf("stderr missing the dial failure for the configured server:\n%s", errOut.String())
	}
}

// TestInteractiveClientCreatesDefaultShareDir pins that a fresh interactive
// setup makes the directory it is about to share. The default lives under
// peerfold-demo and does not exist yet, and the client refuses to start on a
// missing share — so a user who accepted the default used to get a saved
// config and an immediate lstat failure.
func TestInteractiveClientCreatesDefaultShareDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("HOME", home)        // unix

	// Name, server, share name, empty share directory (take the default),
	// foreground mode. The server is a closed port so the dial fails fast.
	input := strings.NewReader("\nalice\n127.0.0.1:1\ndocs\n\n1\n")
	var out, errOut bytes.Buffer
	code := interactiveClient(input, &out, &errOut)

	shareDir := filepath.Join(home, "peerfold-demo", "alice")
	if _, err := os.Stat(shareDir); err != nil {
		t.Fatalf("default share directory was not created (code=%d, out=%q err=%q): %v",
			code, out.String(), errOut.String(), err)
	}
	if strings.Contains(errOut.String(), "no such file or directory") {
		t.Fatalf("client started against a missing share directory:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "Created share directory") {
		t.Errorf("output missing share directory creation:\n%s", out.String())
	}
}

// TestInteractiveMountRejectsRelativePoint pins that a relative mount point
// is refused interactively as well as on the command line: a mount that
// follows the working directory is not a location the user can find later.
func TestInteractiveMountRejectsRelativePoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	input := strings.NewReader("\n127.0.0.1:7000\nrelative/path\n")
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
