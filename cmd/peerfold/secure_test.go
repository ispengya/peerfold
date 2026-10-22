package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/identity"
	"github.com/ispengya/peerfold/internal/server"
)

// TestInteractiveClientSavesInvite pins the invite-driven first run: one paste
// supplies the address, pinned certificate, principal and token, and the saved
// config carries all four. The dial destination is a closed port so the command
// returns instead of starting a client.
func TestInteractiveClientSavesInvite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	inv := identity.Invite{
		Address:     "127.0.0.1:1",
		Fingerprint: strings.Repeat("a", 64),
		Name:        "alice",
		Role:        identity.RoleClient,
		Token:       "test-token",
	}
	// Invite, default share name, default share directory, foreground mode.
	input := strings.NewReader(inv.String() + "\n\n\n0\n")
	var out, errOut bytes.Buffer
	code := interactiveClient(input, &out, &errOut)
	if code == 0 {
		t.Fatalf("client with an unreachable server returned 0; out=%q", out.String())
	}
	if strings.Contains(errOut.String(), "invite is for role") {
		t.Fatalf("the client setup rejected a client invite: %s", errOut.String())
	}

	cfgPath, err := config.ClientPath("alice")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config was not saved (out=%q err=%q): %v", out.String(), errOut.String(), err)
	}
	if cfg.TLSFingerprint != inv.Fingerprint || cfg.Token != inv.Token || cfg.AuthName != "alice" {
		t.Fatalf("saved invite fields = %+v", cfg)
	}
	if cfg.Server != inv.Address {
		t.Fatalf("Server = %q, want %q", cfg.Server, inv.Address)
	}
	if _, err := os.Stat(filepath.Join(home, "peerfold-demo", "alice")); err != nil {
		t.Fatalf("default share directory was not created: %v", err)
	}
}

// TestInviteCommandProducesAWorkingViewerCredential drives the whole CLI trust
// path: a secure server, `peerfold invite`, a config carrying the invite, and
// the ordinary ls command over TLS with the issued token.
func TestInviteCommandProducesAWorkingViewerCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	serverDir := filepath.Join(home, "peerfold-server")
	id, err := identity.LoadOrCreateServer(serverDir)
	if err != nil {
		t.Fatal(err)
	}
	s := server.NewWithOptions("127.0.0.1:0", server.Options{
		TLSConfig:  id.TLSConfig(),
		TokensPath: filepath.Join(serverDir, "tokens.json"),
	})
	if s.Addr() == nil {
		t.Fatalf("server: %v", s.Err())
	}
	go func() {
		if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { s.Close() })

	serverCfg := filepath.Join(serverDir, "config.json")
	var out, errOut bytes.Buffer
	code := cmdInvite([]string{
		"--name", "laptop",
		"--role", "viewer",
		"--server", s.Addr().String(),
		"--config", serverCfg,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("invite = %d, stderr=%q", code, errOut.String())
	}
	inv, err := identity.ParseInvite(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("the command printed an unparseable invite %q: %v", out.String(), err)
	}
	if inv.Fingerprint != id.Fingerprint() || inv.Name != "laptop" || inv.Role != identity.RoleViewer {
		t.Fatalf("invite = %+v", inv)
	}

	viewerCfgPath := filepath.Join(home, "viewer.json")
	cfg := config.Default()
	cfg.Server = inv.Address
	cfg.TLSFingerprint = inv.Fingerprint
	cfg.AuthName = inv.Name
	cfg.Token = inv.Token
	if err := config.Save(viewerCfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if code := cmdLs([]string{"--config", viewerCfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("ls over the invited credentials = %d, stderr=%q", code, errOut.String())
	}

	// The same config with a damaged token must fail the handshake, not fall
	// back to plaintext.
	cfg.Token = "wrong"
	if err := config.Save(viewerCfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := cmdLs([]string{"--config", viewerCfgPath}, &bytes.Buffer{}, &errOut); code == 0 {
		t.Fatalf("ls with a wrong token returned 0; stderr=%q", errOut.String())
	}
}

// A stale config with a half-written invite must not connect unprotected; the
// interactive setup asks for the invite again.
func TestInteractiveMountRepairsAnIncompleteInvite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	cfgPath, err := config.MountPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfgPath, config.Config{
		Server:         "127.0.0.1:1",
		TLSFingerprint: strings.Repeat("b", 64),
		// Token deliberately missing.
		MountPoint: filepath.Join(home, "mnt"),
	}); err != nil {
		t.Fatal(err)
	}

	inv := identity.Invite{
		Address:     "127.0.0.1:1",
		Fingerprint: strings.Repeat("c", 64),
		Name:        "laptop-mount",
		Role:        identity.RoleViewer,
		Token:       "viewer-token",
	}
	// Invite, then accept the saved mount point, then foreground.
	input := strings.NewReader(inv.String() + "\n\n")
	var out, errOut bytes.Buffer
	code := interactiveMount(input, &out, &errOut)
	if code == 0 {
		t.Fatalf("mount with an unreachable server returned 0; out=%q", out.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != inv.Token || cfg.TLSFingerprint != inv.Fingerprint || cfg.AuthName != "laptop-mount" {
		t.Fatalf("repaired config = %+v", cfg)
	}
}

// An existing plaintext config is offered an invite on its next interactive
// start; pasting one upgrades it in place. This is the migration path for
// machines that were set up before TLS existed.
func TestInteractiveClientUpgradesAnExistingConfigWithAnInvite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	cfgPath, err := config.ClientPath("alice")
	if err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(home, "docs")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(cfgPath, config.Config{
		Server:     "127.0.0.1:1",
		ClientName: "alice",
		Shares:     map[string]string{"docs": share},
	}); err != nil {
		t.Fatal(err)
	}

	inv := identity.Invite{
		Address:     "127.0.0.1:1",
		Fingerprint: strings.Repeat("d", 64),
		Name:        "alice",
		Role:        identity.RoleClient,
		Token:       "upgrade-token",
	}
	var out, errOut bytes.Buffer
	code := interactiveClient(strings.NewReader(inv.String()+"\n0\n"), &out, &errOut)
	if code == 0 {
		t.Fatalf("client with an unreachable server returned 0; out=%q", out.String())
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != inv.Token || cfg.TLSFingerprint != inv.Fingerprint || cfg.AuthName != "alice" {
		t.Fatalf("upgraded config = %+v", cfg)
	}
}
