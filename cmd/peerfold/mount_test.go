package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/mount"
)

// TestMountIsDispatched pins the arm that turns the whole mount layer into
// something a person can run. Without it every other test in this file is
// exercising a function nothing reaches.
func TestMountIsDispatched(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runWithEmptyStdin([]string{"mount", "--server", "127.0.0.1:1"}, &out, &errOut); code == 2 && strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("the dispatcher does not know %q: stderr = %q", "mount", errOut.String())
	}
	if !strings.Contains(usageText, "mount") {
		t.Error("usageText does not list mount, so nobody can find it")
	}
}

// TestDispatcherKnowsEveryListedCommand is the brief's dispatcherKnows check,
// made into a property rather than a list: every word in the command list is
// known, and every word the usage text promises is in the command list. The two
// lists are separate strings, so without this a command could be documented and
// not implemented -- or implemented and not documented.
func TestDispatcherKnowsEveryListedCommand(t *testing.T) {
	for _, cmd := range commands {
		if !dispatcherKnows(cmd) {
			t.Errorf("%q is in the command list but not in the dispatcher", cmd)
		}
	}
	if !dispatcherKnows("mount") {
		t.Error(`"mount" is not in the dispatcher`)
	}
	for _, line := range strings.Split(usageText, "\n") {
		word, ok := strings.CutPrefix(strings.TrimSpace(line), "  ")
		if !ok {
			continue
		}
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		name, _, _ := strings.Cut(word, " ")
		if !dispatcherKnows(name) {
			t.Errorf("usageText lists %q, which the dispatcher does not know", name)
		}
	}
}
func TestMountRequiresAServer(t *testing.T) {
	// Isolate HOME: a real server config on this machine would be migrated
	// into the mount's view and provide the address the test says is missing.
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows

	var out, errOut bytes.Buffer
	code := runMount([]string{"--mount", filepath.Join(t.TempDir(), "mp")}, &out, &errOut)
	if code == 0 {
		t.Errorf("mount without --server = %d, want non-zero; stderr = %q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "server") {
		t.Errorf("stderr = %q, want it to name the missing --server", errOut.String())
	}
}

func TestMountRejectsUnknownOption(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runMount([]string{"--nonsense"}, &out, &errOut); code == 0 {
		t.Error("mount with an unknown option should fail")
	}
}

// TestMountRejectsARelativeMountPoint pins the absolute-path rule. A relative
// mount point resolves against the process's working directory, so the same
// command would mount somewhere different depending on where it was run.
func TestMountRejectsARelativeMountPoint(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runMount([]string{"--server", "127.0.0.1:1", "--mount", "relative/mp"}, &out, &errOut)
	if code == 0 {
		t.Error("a relative --mount should be refused before anything is dialled")
	}
	if !strings.Contains(errOut.String(), "absolute") {
		t.Errorf("stderr = %q, want it to say the path must be absolute", errOut.String())
	}
}

// TestMountSaysWhereItWillMount pins the disclosure. The mount point has a
// default, and a default the user cannot see is a directory they will go looking
// for later; the line is printed before any connection is attempted, so it is
// readable even when the server is down.
func TestMountSaysWhereItWillMount(t *testing.T) {
	want, err := mount.DefaultMountPoint()
	if err != nil {
		t.Fatalf("DefaultMountPoint: %v", err)
	}
	var out, errOut bytes.Buffer
	runMount([]string{"--server", "127.0.0.1:1"}, &out, &errOut)

	if !strings.Contains(out.String(), want) {
		t.Errorf("stdout = %q, want it to say the mount point is %s", out.String(), want)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int{
		"1024":   1024,
		"64MiB":  64 << 20,
		"256MiB": 256 << 20,
		"2GiB":   2 << 30,
		"512KiB": 512 << 10,
		"64M":    64 << 20,
		"1G":     1 << 30,
		"0":      0,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Errorf("parseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "abc", "-1", "64XiB", "MiB", "64MiBx", "1.5MiB"} {
		if got, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", bad, got)
		}
	}
}

// TestMountBufferFlagsReachTheOptions is the reason parseSize exists: a flag that
// is parsed and then dropped is worse than no flag, because the user believes the
// limit is in force.
func TestMountBufferFlagsReachTheOptions(t *testing.T) {
	opts, _, _, _, err := mountOptionsFrom([]string{"--server", "127.0.0.1:1", "--buffer", "4MiB", "--buffer-total", "8MiB"})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	if opts.BufferLimit != 4<<20 {
		t.Errorf("BufferLimit = %d, want %d", opts.BufferLimit, 4<<20)
	}
	if opts.BufferTotal != 8<<20 {
		t.Errorf("BufferTotal = %d, want %d", opts.BufferTotal, 8<<20)
	}
}

// TestMountBufferFlagsDefaultToTheMountDefaults pins that the CLI does not
// invent its own numbers. If it did, `peerfold mount` and a program using the
// package directly would have different limits, and only one of them is
// documented.
func TestMountBufferFlagsDefaultToTheMountDefaults(t *testing.T) {
	opts, _, _, _, err := mountOptionsFrom([]string{"--server", "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	def := mount.DefaultOptions()
	if opts.BufferLimit != def.BufferLimit {
		t.Errorf("BufferLimit = %d, want the package default %d", opts.BufferLimit, def.BufferLimit)
	}
	if opts.BufferTotal != def.BufferTotal {
		t.Errorf("BufferTotal = %d, want the package default %d", opts.BufferTotal, def.BufferTotal)
	}
	if opts.AttrTimeout != def.AttrTimeout || opts.EntryTimeout != def.EntryTimeout {
		t.Errorf("the timeouts are %v/%v, want the package defaults %v/%v",
			opts.AttrTimeout, opts.EntryTimeout, def.AttrTimeout, def.EntryTimeout)
	}
}

// TestMountRejectsABadBufferSize keeps a mistyped limit from being ignored.
func TestMountRejectsABadBufferSize(t *testing.T) {
	if _, _, _, _, err := mountOptionsFrom([]string{"--server", "127.0.0.1:1", "--buffer", "64XiB"}); err == nil {
		t.Error("a bad --buffer should be refused, not ignored")
	}
}

// TestMountUsesTheConfiguredServer pins the fallback the client already has: a
// --server on the command line wins, and a config value is used when there is no
// flag. Without it, every mount would have to repeat an address that is already
// written down.
func TestMountUsesTheConfiguredServer(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	point := filepath.Join(dir, "mp")

	cfg := config.Default()
	cfg.Server = "127.0.0.1:9"
	cfg.MountPoint = point
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	// With no --server and an existing config, the config is the source.
	_, server, gotPoint, _, err := mountOptionsFrom([]string{"--config", cfgPath})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	if server != cfg.Server {
		t.Errorf("server = %q, want the configured %q", server, cfg.Server)
	}
	if gotPoint != point {
		t.Errorf("mount point = %q, want the configured %q", gotPoint, point)
	}
}
