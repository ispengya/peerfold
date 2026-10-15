package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"peerfold/internal/config"
	"peerfold/internal/mount"
)

// TestMountZeroBufferInTheConfigKeepsTheDefault pins the "zero means unset" rule
// for the buffer keys.
//
// Config.Default writes the numbers down, but a config file written by hand --
// or by a version that did not have these keys -- loads as zero. Zero is
// therefore a real state and not a legal limit: a cache of nothing would send
// every read to the owner. The distinction is between "the file says 0" and "the
// file says nothing", and only the second one is reachable here.
func TestMountZeroBufferInTheConfigKeepsTheDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.Server = "127.0.0.1:1"
	cfg.BufferLimit = 0
	cfg.BufferTotal = 0
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	opts, _, _, _, err := mountOptionsFrom([]string{"--config", cfgPath})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	def := mount.DefaultOptions()
	if opts.BufferLimit != def.BufferLimit {
		t.Errorf("a config saying buffer_limit 0 gave BufferLimit %d, want the default %d",
			opts.BufferLimit, def.BufferLimit)
	}
	if opts.BufferTotal != def.BufferTotal {
		t.Errorf("a config saying buffer_total 0 gave BufferTotal %d, want the default %d",
			opts.BufferTotal, def.BufferTotal)
	}
}

// TestMountLeavesTheServerEmptyWhenNothingSuppliesIt pins the signal the command
// uses to report a missing server: an empty string, not an error.
//
// The options function deliberately does not decide that a missing server is
// fatal, because the command is what has to print it next to the config path.
// That makes "empty comes back" a contract between the two, and a contract
// nothing checks is where the two drift.
func TestMountLeavesTheServerEmptyWhenNothingSuppliesIt(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.Server = ""
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	_, server, _, _, err := mountOptionsFrom([]string{
		"--config", cfgPath,
		"--mount", filepath.Join(dir, "mp"),
	})
	if err != nil {
		t.Fatalf("mountOptionsFrom: %v", err)
	}
	if server != "" {
		t.Errorf("server = %q, want empty so runMount reports it", server)
	}
}

// TestParseSizeAtTheIntCeiling pins the range check's boundary.
//
// It is a direct call rather than a flag, and that is deliberate: a size that is
// merely too big for the cache is refused by the "buffer above total" check
// before the range check is reached, so a test that went through the flags would
// pass whether or not parseSize's own check worked at all.
func TestParseSizeAtTheIntCeiling(t *testing.T) {
	got, err := parseSize(strconv.Itoa(maxInt))
	if err != nil {
		t.Fatalf("parseSize(maxInt) = %v, want the largest int to be accepted", err)
	}
	if got != maxInt {
		t.Errorf("parseSize(maxInt) = %d, want %d", got, maxInt)
	}

	// The rejection side of the check cannot be reached on a 64-bit platform,
	// and saying so is more useful than a test that pretends otherwise.
	//
	// parseSize reads the number with ParseInt(..., 64), so the largest value it
	// can be handed at all is MaxInt64 -- which on a 64-bit platform IS maxInt.
	// There is no representable input that exceeds the ceiling, so the branch
	// exists for a 32-bit build, where maxInt is 2^31-1 and "3000000000" is a
	// well-formed int64 that does not fit an int.
	if int64(maxInt) == int64(^uint64(0)>>1) {
		t.Log("the range check is unreachable on this 64-bit platform: no " +
			"representable size exceeds maxInt")
	}
}

// TestBufferTotalAloneIsNotAContradiction pins the ordering rule: lowering the
// total below the per-file default is a real configuration -- one open file may
// use the whole cache -- and must not be refused as though it were a mistake.
//
// The check is "the per-file limit is above the total", and a version of it that
// compared the two without asking which was which would refuse this.
func TestBufferTotalAloneIsNotAContradiction(t *testing.T) {
	dir := t.TempDir()
	_, _, _, _, err := mountOptionsFrom([]string{
		"--server", "127.0.0.1:1",
		"--mount", filepath.Join(dir, "mp"),
		"--buffer-total", "128MiB",
		"--buffer", "64MiB",
	})
	if err != nil {
		t.Fatalf("a total above the per-file limit was refused: %v", err)
	}
}

// TestBufferTotalBelowTheDefaultIsRefused pins the other direction through the
// defaults rather than through flags: the default per-file limit is 64MiB, so a
// total of 1KiB cannot hold it. The user has to be told, because the cache would
// otherwise run at 1KiB while the per-file setting says 64MiB.
func TestBufferTotalBelowTheDefaultIsRefused(t *testing.T) {
	dir := t.TempDir()
	_, _, _, _, err := mountOptionsFrom([]string{
		"--server", "127.0.0.1:1",
		"--mount", filepath.Join(dir, "mp"),
		"--buffer-total", "1KiB",
	})
	if err == nil {
		t.Fatal("a total below the default per-file limit was accepted; the " +
			"per-file limit could never be reached")
	}
	if !strings.Contains(err.Error(), "buffer-total") {
		t.Errorf("error = %q, want it to name --buffer-total", err)
	}
}
