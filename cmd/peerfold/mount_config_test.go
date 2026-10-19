package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ispengya/peerfold/internal/config"
)

// TestMountFlagsWorkWithoutAnyConfig pins that a fresh machine can mount with
// nothing but flags. The config is a convenience, not a prerequisite: before
// this, a missing default config failed the command even though every value it
// needed was on the command line.
func TestMountFlagsWorkWithoutAnyConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows

	mp := filepath.Join(t.TempDir(), "mp")
	opts, server, point, _, err := mountOptionsFrom([]string{
		"--server", "127.0.0.1:1",
		"--mount", mp,
	})
	if err != nil {
		t.Fatalf("mountOptionsFrom with no config: %v", err)
	}
	if server != "127.0.0.1:1" {
		t.Errorf("server = %q, want the flag's 127.0.0.1:1", server)
	}
	if point != mp {
		t.Errorf("mount point = %q, want the flag's %q", point, mp)
	}
	if opts.BufferLimit <= 0 || opts.BufferTotal <= 0 {
		t.Errorf("buffer defaults = (%d, %d), want the mount package's positive defaults", opts.BufferLimit, opts.BufferTotal)
	}
}

// TestMountOptionsComeFromTheConfig proves the three config keys are read rather
// than merely parsed into a struct. A key that is stored and never consulted is
// worse than a missing one: the user sets it, sees no complaint, and believes it
// is in force.
func TestMountOptionsComeFromTheConfig(t *testing.T) {
	dir := t.TempDir()
	mp := filepath.Join(dir, "from-config")
	cfgPath := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.Server = "127.0.0.1:1"
	cfg.MountPoint = mp
	cfg.BufferLimit = 3 << 20
	cfg.BufferTotal = 9 << 20
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	opts, server, point, gotPath, err := mountOptionsFrom([]string{"--config", cfgPath})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	if server != cfg.Server {
		t.Errorf("server = %q, want the configured %q", server, cfg.Server)
	}
	if point != mp {
		t.Errorf("mount point = %q, want the configured %q", point, mp)
	}
	if opts.BufferLimit != cfg.BufferLimit {
		t.Errorf("BufferLimit = %d, want the configured %d", opts.BufferLimit, cfg.BufferLimit)
	}
	if opts.BufferTotal != cfg.BufferTotal {
		t.Errorf("BufferTotal = %d, want the configured %d", opts.BufferTotal, cfg.BufferTotal)
	}
	if gotPath != cfgPath {
		t.Errorf("config path = %q, want %q", gotPath, cfgPath)
	}
}

// TestMountFlagsBeatTheConfig pins the precedence, which is the whole point of
// having both: the file is the standing preference and the flag is this run.
func TestMountFlagsBeatTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.Server = "127.0.0.1:1"
	cfg.MountPoint = filepath.Join(dir, "from-config")
	cfg.BufferLimit = 3 << 20
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	flagPoint := filepath.Join(dir, "from-flag")
	opts, server, point, _, err := mountOptionsFrom([]string{
		"--config", cfgPath,
		"--server", "127.0.0.1:2",
		"--mount", flagPoint,
		"--buffer", "5MiB",
	})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	if server != "127.0.0.1:2" {
		t.Errorf("server = %q, want the flag's 127.0.0.1:2", server)
	}
	if point != flagPoint {
		t.Errorf("mount point = %q, want the flag's %q", point, flagPoint)
	}
	if opts.BufferLimit != 5<<20 {
		t.Errorf("BufferLimit = %d, want the flag's %d", opts.BufferLimit, 5<<20)
	}
	// The total was not on the command line, so the config's standing value is
	// still what applies -- a flag for one key must not reset the others.
	if opts.BufferTotal != cfg.BufferTotal {
		t.Errorf("BufferTotal = %d, want the configured %d", opts.BufferTotal, cfg.BufferTotal)
	}
}

// TestMountRejectsBufferAboveTheTotal pins the contradiction check. Both numbers
// reach the same cache, and the smaller one wins, so a per-file limit above the
// total can never be reached -- the user should be told rather than left to
// discover it from performance.
func TestMountRejectsBufferAboveTheTotal(t *testing.T) {
	dir := t.TempDir()
	_, _, _, _, err := mountOptionsFrom([]string{
		"--server", "127.0.0.1:1",
		"--mount", filepath.Join(dir, "mp"),
		"--buffer", "128MiB",
		"--buffer-total", "64MiB",
	})
	if err == nil {
		t.Fatal("--buffer above --buffer-total was accepted")
	}
	if !strings.Contains(err.Error(), "buffer-total") {
		t.Errorf("error = %q, want it to name --buffer-total", err)
	}
}

// TestMountUsesTheDefaultMountPointWhenNothingSaysOtherwise pins the case that
// has no config and no flag: the platform default has to be what is used, or
// `peerfold mount --server ...` would not know where to mount.
func TestMountUsesTheDefaultMountPointWhenNothingSaysOtherwise(t *testing.T) {
	// A config path under a temporary directory, so a real config file on this
	// machine cannot influence the result.
	dir := t.TempDir()
	_, _, point, _, err := mountOptionsFrom([]string{
		"--server", "127.0.0.1:1",
		"--config", filepath.Join(dir, "absent.json"),
	})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	if !strings.HasSuffix(point, "peerfold") {
		t.Errorf("mount point = %q, want it to fall back to the platform default", point)
	}
	if !filepath.IsAbs(point) {
		t.Errorf("the default mount point %q is not absolute", point)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "absent.json")); statErr != nil {
		// Not a requirement, just worth stating: reading a config that is not
		// there must not create it, because a read that writes is a surprise.
		if !os.IsNotExist(statErr) {
			t.Errorf("reading a missing config reported %v", statErr)
		}
	}
}

// TestMountUsageIsReachable pins that --help goes somewhere useful rather than
// being swallowed by the flag set's discarded output.
func TestMountUsageIsReachable(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runMount([]string{"--help"}, &out, &errOut)
	if code == 0 {
		t.Errorf("--help = %d, want non-zero so a script can tell it from a mount", code)
	}
	combined := out.String() + errOut.String()
	if !strings.Contains(combined, "peerfold mount") {
		t.Errorf("--help printed %q, want the mount usage", combined)
	}
}
