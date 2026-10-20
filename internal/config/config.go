package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the on-disk configuration for both the server and the client.
//
// Save and Load are exact inverses: neither fills in defaults nor normalizes
// Shares, so Load(Save(c)) equals c field for field. Shares is therefore nil
// for a config that declares no shares; a caller that needs an initialized
// map should start from Default.
type Config struct {
	Listen     string            `json:"listen"`
	Server     string            `json:"server"`
	ClientName string            `json:"client_name"`
	Shares     map[string]string `json:"shares"`
	Aggregate  bool              `json:"aggregate"`

	// MountPoint is where `peerfold mount` mounts when --mount is not given.
	// Empty means "the platform's default", which is resolved at mount time
	// rather than stored: the default is under the user's home directory, and
	// writing an absolute path into a config file makes the file wrong as soon as
	// it is copied to another machine.
	MountPoint string `json:"mount_point"`

	// BufferLimit and BufferTotal are the D3 pair: the read cache for one open
	// file and for all of them together. Zero means "use the mount package's
	// default", which is what an omitted key gets.
	//
	// They are in the config at all because the right numbers depend on the
	// machine: a 4 GiB laptop and a 64 GiB workstation should not be told the
	// same limits by a program that could have asked.
	BufferLimit int `json:"buffer_limit"`
	BufferTotal int `json:"buffer_total"`

	// ChunkSize is how many bytes one ranged read asks for. It is the knob
	// that trades first-byte latency against throughput on a high-RTT link;
	// zero means "use the mount package's default".
	ChunkSize int `json:"chunk_size"`
}

// Default returns the values used when no config file exists yet.
//
// The buffer numbers are the mount package's defaults written down, not a
// second opinion. They are duplicated rather than imported so that this package
// stays free of the mount package's dependencies, and the consequence is that
// changing either one alone leaves the pair disagreeing;
// TestConfigMountDefaultsAgreeWithTheMount in cmd/peerfold fails when that
// happens, which is where the pair can be compared without the import.
func Default() Config {
	return Config{
		// All interfaces, not loopback: a server that only answers on
		// 127.0.0.1 is unreachable from the machines whose trees it is meant
		// to aggregate. Deployments that must stay local pass --listen
		// 127.0.0.1:7000 (and should, because the protocol has no auth).
		Listen: "0.0.0.0:7000",
		Shares: map[string]string{},

		// Left empty because it is resolved from the home directory at mount
		// time: a stored absolute path is wrong as soon as the config is copied
		// to another machine.
		MountPoint: "",

		BufferLimit: 64 << 20,
		BufferTotal: 256 << 20,
		ChunkSize:   512 << 10,
	}
}

// Load reads a config file verbatim: it merges no defaults and normalizes
// nothing, so an omitted key stays at its zero value and Save/Load are exact
// inverses. Callers that want defaults start from Default and overlay the
// loaded values.
//
// A missing file reports an error satisfying errors.Is(err, os.ErrNotExist).
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Save writes the config file, creating its parent directory if needed: the
// per-user config paths live directly in the home directory, which exists for a
// real user but is still created defensively for a bare environment.
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

// ServerPath returns the config path the server reads its listen address from: a
// config.json inside the server's own directory under the home directory.
func ServerPath() (string, error) {
	return homePath("peerfold-server", "config.json")
}

// ClientPath returns the config path for the client named name: one directory
// per client, keyed by name, so several clients on one machine do not share a
// file. The directory also holds the client's log, so everything one client
// writes lives in one place.
func ClientPath(name string) (string, error) {
	return homePath("peerfold-client-"+name, "config.json")
}

// MountPath returns the config path for the mount role.
//
// Mount is a client of the aggregated tree, not the server: its mount point
// and read-cache settings belong to this machine's mount. Keeping them in the
// server's directory made a viewer's file look like the server's, and a
// machine that never runs a server still needed one to mount.
func MountPath() (string, error) {
	return homePath("peerfold-mount", "config.json")
}

// homePath joins a directory and file name onto the user's home directory.
func homePath(dir, name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dir, name), nil
}
