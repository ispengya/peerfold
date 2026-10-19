package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
