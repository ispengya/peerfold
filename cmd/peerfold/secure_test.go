package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/identity"
)

// TestInteractiveClientSavesLoginCode pins the first run: one paste supplies
// the certificate pin and enrollment secret; the client supplies its own name
// and address, and the saved config carries the code. The dial destination is a
// closed port so the command returns instead of starting a client.
func TestInteractiveClientSavesLoginCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	code := identity.LoginCode{
		Fingerprint: strings.Repeat("a", 64),
		Secret:      "test-secret",
	}
	// Code, client name, server address, default share name, default share
	// directory, foreground mode.
	input := strings.NewReader(code.String() + "\nalice\n127.0.0.1:1\n\n\n0\n")
	var out, errOut bytes.Buffer
	exit := interactiveClient(input, &out, &errOut)
	if exit == 0 {
		t.Fatalf("client with an unreachable server returned 0; out=%q", out.String())
	}
	if strings.Contains(errOut.String(), "login code must look like") {
		t.Fatalf("the client setup rejected a valid login code: %s", errOut.String())
	}

	cfgPath, err := config.ClientPath("alice")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config was not saved (out=%q err=%q): %v", out.String(), errOut.String(), err)
	}
	if cfg.TLSFingerprint != code.Fingerprint || cfg.Token != code.Secret || cfg.AuthName != "alice" {
		t.Fatalf("saved login-code fields = %+v", cfg)
	}
	if cfg.Server != "127.0.0.1:1" {
		t.Fatalf("Server = %q, want 127.0.0.1:1", cfg.Server)
	}
	if _, err := os.Stat(filepath.Join(home, "peerfold-demo", "alice")); err != nil {
		t.Fatalf("default share directory was not created: %v", err)
	}
}

// A stale config with a half-written login code must not connect unprotected;
// the interactive setup asks for the code again.
func TestInteractiveMountRepairsAnIncompleteLoginCode(t *testing.T) {
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

	code := identity.LoginCode{
		Fingerprint: strings.Repeat("c", 64),
		Secret:      "dmlld2VyLXNlY3JldA",
	}
	// Code, then accept the saved server address and mount point.
	input := strings.NewReader(code.String() + "\n\n\n")
	var out, errOut bytes.Buffer
	exit := interactiveMount(input, &out, &errOut)
	if exit == 0 {
		t.Fatalf("mount with an unreachable server returned 0; out=%q", out.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != code.Secret || cfg.TLSFingerprint != code.Fingerprint {
		t.Fatalf("repaired config = %+v", cfg)
	}
}

// An existing plaintext config is offered a login code on its next interactive
// start; pasting one upgrades it in place. This is the migration path for
// machines that were set up before TLS existed.
func TestInteractiveClientUpgradesAnExistingConfigWithALoginCode(t *testing.T) {
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

	login := identity.LoginCode{
		Fingerprint: strings.Repeat("d", 64),
		Secret:      "upgrade-secret",
	}
	var out, errOut bytes.Buffer
	code := interactiveClient(strings.NewReader(login.String()+"\n0\n"), &out, &errOut)
	if code == 0 {
		t.Fatalf("client with an unreachable server returned 0; out=%q", out.String())
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != login.Secret || cfg.TLSFingerprint != login.Fingerprint || cfg.AuthName != "alice" {
		t.Fatalf("upgraded config = %+v", cfg)
	}
}

// A secure server command prints exactly the one code a new machine needs, and
// the code is a valid certificate pin plus enrollment secret.
func TestCmdServerPrintsALoginCode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)

	cfgPath := filepath.Join(home, "peerfold-server", "config.json")
	var out syncBuffer
	var errOut bytes.Buffer
	interrupt := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		done <- cmdServerWith([]string{"--listen", "127.0.0.1:0", "--config", cfgPath}, &out, &errOut, interrupt)
	}()

	var code string
	for i := 0; i < 2000; i++ {
		for _, line := range strings.Split(out.String(), "\n") {
			if c, ok := strings.CutPrefix(line, "login code: "); ok {
				code = strings.TrimSpace(c)
			}
		}
		if code != "" && strings.Contains(out.String(), "listening on ") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if code == "" {
		close(interrupt)
		<-done
		t.Fatalf("the server printed no login code; stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if _, err := identity.ParseLoginCode(code); err != nil {
		t.Fatalf("the printed login code does not parse: %v", err)
	}

	close(interrupt)
	select {
	case exit := <-done:
		if exit != 0 {
			t.Fatalf("server exited %d; stderr=%q", exit, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop")
	}
}
