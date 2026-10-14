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

func TestConfigSharesType(t *testing.T) {
	shares := map[string]string{"Documents": "/home/me/Documents"}
	got := config.Config{Shares: shares}

	if !reflect.DeepEqual(got.Shares, map[string]string{"Documents": "/home/me/Documents"}) {
		t.Fatalf("Shares = %#v, want the map passed in", got.Shares)
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
