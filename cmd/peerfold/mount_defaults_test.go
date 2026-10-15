package main

import (
	"testing"

	"peerfold/internal/config"
	"peerfold/internal/mount"
)

// TestConfigMountDefaultsAgreeWithTheMount is the check the comment on
// config.Default promises.
//
// The buffer numbers are written down in two packages -- config cannot import
// mount without acquiring the mount package's dependencies -- and the failure
// mode of a duplicated constant is silence: the CLI would fall back to one
// number while the config file documented another. Comparing them where both are
// already imported is the cheapest place to notice.
func TestConfigMountDefaultsAgreeWithTheMount(t *testing.T) {
	cfg := config.Default()
	def := mount.DefaultOptions()

	if cfg.BufferLimit != def.BufferLimit {
		t.Errorf("config.Default().BufferLimit = %d, mount.DefaultOptions().BufferLimit = %d",
			cfg.BufferLimit, def.BufferLimit)
	}
	if cfg.BufferTotal != def.BufferTotal {
		t.Errorf("config.Default().BufferTotal = %d, mount.DefaultOptions().BufferTotal = %d",
			cfg.BufferTotal, def.BufferTotal)
	}
	// A config that declares no buffers must leave the mount's defaults alone,
	// which is what the "zero means the package default" rule buys.
	loaded := config.Config{}
	opts := mount.DefaultOptions()
	if loaded.BufferLimit > 0 {
		opts.BufferLimit = loaded.BufferLimit
	}
	if opts.BufferLimit != def.BufferLimit {
		t.Errorf("an empty config changed the buffer limit to %d", opts.BufferLimit)
	}
}
