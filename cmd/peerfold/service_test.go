package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/server"
	"github.com/ispengya/peerfold/internal/viewer"
)

// TestDefaultClientNamePrefersComputerName pins the interactive default: the
// Windows environment variable wins when it is set, because that is what a
// Windows user already sees as their machine identity.
func TestDefaultClientNamePrefersComputerName(t *testing.T) {
	t.Setenv("COMPUTERNAME", "WINBOX")
	got := defaultClientName()
	if got != "WINBOX" {
		t.Fatalf("defaultClientName() = %q, want WINBOX", got)
	}
}

// TestDefaultClientNameFallsBackToHostname pins the non-Windows path: with no
// COMPUTERNAME the host name is the identity, which is what cmd_client.go
// already uses when --name is omitted.
func TestDefaultClientNameFallsBackToHostname(t *testing.T) {
	t.Setenv("COMPUTERNAME", "")
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skipf("no hostname: %v", err)
	}
	got := defaultClientName()
	if got != host {
		t.Fatalf("defaultClientName() = %q, want hostname %q", got, host)
	}
}

// TestDefaultShareDirIsAPerUserPath pins that the interactive suggestion is a
// real per-user directory and not a hard-coded foreign path.
func TestDefaultShareDirIsAPerUserPath(t *testing.T) {
	got := defaultShareDir("alice")
	if !strings.Contains(got, "alice") {
		t.Fatalf("defaultShareDir(alice) = %q, want it to contain the client name", got)
	}
	if runtime.GOOS == "windows" {
		if !strings.Contains(strings.ToLower(got), "peerfold-demo") {
			t.Fatalf("defaultShareDir(alice) = %q, want peerfold-demo", got)
		}
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home: %v", err)
	}
	want := filepath.Join(home, "peerfold-demo", "alice")
	if got != want {
		t.Fatalf("defaultShareDir(alice) = %q, want %q", got, want)
	}
}

// TestStatusReportsTheMount pins that a mount is visible from another
// terminal: the pid file makes it "running", and its state file supplies the
// values it was actually started with. Flags need not match the config, so
// reading the config alone would report the wrong server or mount point.
func TestStatusReportsTheMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	defer func() {
		clearPID("mount")
		clearMountState()
	}()

	if err := writePID("mount", os.Getpid()); err != nil {
		t.Fatalf("writePID(mount): %v", err)
	}
	if err := writeMountState("10.0.0.9:7000", filepath.Join(home, "mnt")); err != nil {
		t.Fatalf("writeMountState: %v", err)
	}

	var out, errOut bytes.Buffer
	if code := cmdStatus(nil, &out, &errOut); code != 0 {
		t.Fatalf("cmdStatus() = %d, stderr = %q", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"Mount:", "running (PID " + itoa(os.Getpid()) + ")", "10.0.0.9:7000", filepath.Join(home, "mnt")} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
}

// itoa is strconv.Itoa under a shorter name, so the test above reads as the
// sentence it is making.
func itoa(n int) string { return strconv.Itoa(n) }

// TestStatusListsMountsOnAnotherMachine pins the reason the viewer registry
// exists: the mount and the status command that sees it are different
// processes, and the status side has no local pid file to consult. Only the
// server knows about the mount, and status has to ask it.
func TestStatusListsMountsOnAnotherMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows

	s := server.New("127.0.0.1:0")
	if err := s.Err(); err != nil {
		t.Fatalf("server.New: %v", err)
	}
	go s.Serve()
	defer s.Close()
	addr := s.Addr().String()

	cfg := config.Default()
	cfg.Server = addr
	cfgPath, err := config.ServerPath()
	if err != nil {
		t.Fatalf("ServerPath: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	m, err := viewer.DialMount(addr, "winbox", `C:\Users\me\peerfold`)
	if err != nil {
		t.Fatalf("DialMount: %v", err)
	}
	defer m.Close()

	var out, errOut bytes.Buffer
	if code := cmdStatus(nil, &out, &errOut); code != 0 {
		t.Fatalf("cmdStatus() = %d, stderr = %q", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"Remote mounts via " + addr, "winbox", `C:\Users\me\peerfold`} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}
}

// TestPidFileRoundTrip pins the portable pid-file helpers against the real
// home directory layout.
func TestPidFileRoundTrip(t *testing.T) {
	const component = "client-servicetest"
	defer clearPID(component)

	if err := writePID(component, os.Getpid()); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	pid, alive, err := readPID(component)
	if err != nil {
		t.Fatalf("readPID: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("readPID pid = %d, want %d", pid, os.Getpid())
	}
	if !alive {
		t.Fatal("readPID alive = false for our own pid")
	}

	path, err := pidFilePath(component)
	if err != nil {
		t.Fatalf("pidFilePath: %v", err)
	}
	if !strings.Contains(path, component) {
		t.Fatalf("pidFilePath = %q, want it to contain %q", path, component)
	}
}
