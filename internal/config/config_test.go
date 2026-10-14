package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"peerfold/internal/config"
)

func TestConfigDefaultListen(t *testing.T) {
	if got := config.Default().Listen; got != ":7000" {
		t.Fatalf("Default().Listen = %q, want %q", got, ":7000")
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	// Parent directory does not exist: DefaultPath() lands under
	// os.UserConfigDir(), which may not have a peerfold/ directory yet.
	path := filepath.Join(t.TempDir(), "peerfold", "config.json")
	want := config.Config{
		Listen:     ":7000",
		Server:     "10.0.0.1:7000",
		ClientName: "laptop",
		Shares:     map[string]string{"Documents": "/home/me/Documents"},
		Aggregate:  true,
	}

	if err := config.Save(path, want); err != nil {
		t.Fatalf("Save(%q): %v", path, err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

// TestConfigNilSharesRoundTrip pins the exact-inverse contract for a config
// that declares no shares. Neither Save nor Load normalizes Shares, so a nil
// map comes back nil. Re-adding normalization on either side makes this fail:
// it is genuinely falsifiable, unlike the earlier version of this test.
func TestConfigNilSharesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := config.Config{Listen: ":7000"}
	if want.Shares != nil {
		t.Fatal("this test is only meaningful with a nil Shares map")
	}

	if err := config.Save(path, want); err != nil {
		t.Fatalf("Save(%q): %v", path, err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v (Shares must not be normalized on either side)", got, want)
	}
}

// TestConfigLoadStaysFaithful pins the "Load is a faithful read" rule: an
// omitted key keeps its zero value instead of being defaulted, which is what
// keeps Save/Load exact inverses.
func TestConfigLoadStaysFaithful(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"server":"10.0.0.1:7000"}`), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	if got.Listen != "" {
		t.Fatalf("Load().Listen = %q, want %q (an omitted key must not be defaulted)", got.Listen, "")
	}
	if got.Server != "10.0.0.1:7000" {
		t.Fatalf("Load().Server = %q, want %q", got.Server, "10.0.0.1:7000")
	}
	if got.Shares != nil {
		t.Fatalf("Load().Shares = %#v, want nil (an omitted key must not be normalized)", got.Shares)
	}
}

func TestConfigLoadMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "config.json")

	_, err := config.Load(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load(%q) error = %v, want an error satisfying errors.Is(err, os.ErrNotExist)", path, err)
	}
}

func TestConfigDefaultPath(t *testing.T) {
	p, err := config.DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath(): %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("DefaultPath() = %q, want an absolute path", p)
	}
	if filepath.Base(p) != "config.json" {
		t.Fatalf("DefaultPath() = %q, want it to end in config.json", p)
	}
	if got := filepath.Base(filepath.Dir(p)); got != "peerfold" {
		t.Fatalf("DefaultPath() = %q, want its parent directory to be %q, got %q", p, "peerfold", got)
	}
}
