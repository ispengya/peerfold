package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the on-disk configuration for both the server and the client.
type Config struct {
	Listen     string            `json:"listen"`
	Server     string            `json:"server"`
	ClientName string            `json:"client_name"`
	Shares     map[string]string `json:"shares"`
	Aggregate  bool              `json:"aggregate"`
}

// Default returns the values used when no config file exists yet.
func Default() Config {
	return Config{
		Listen: ":7000",
		Shares: map[string]string{},
	}
}

// Load reads a config file. A missing file reports an error satisfying
// errors.Is(err, os.ErrNotExist).
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	if c.Shares == nil {
		c.Shares = map[string]string{}
	}
	return c, nil
}

// Save writes the config file, creating its parent directory if needed:
// DefaultPath lives under a peerfold/ directory that may not exist yet.
func Save(path string, c Config) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// DefaultPath returns the config path under os.UserConfigDir().
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "peerfold", "config.json"), nil
}
