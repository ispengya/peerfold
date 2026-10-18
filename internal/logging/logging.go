// Package logging sets up the always-on diagnostic log the server and client
// write to, so a run can be reconstructed afterwards without a debug flag.
package logging

import (
	"os"
	"path/filepath"
)

// Open returns an append-mode file at the given home-relative path, creating the
// parent directory on first use. The returned file is the standard log package's
// output; the caller closes it on exit.
//
// The path is relative to the user's home directory and names both the owning
// component's directory and the file, e.g. "peerfold-server/log.txt" or
// "peerfold-client-alice/log.txt". Each component's log therefore lives inside
// its own directory, next to its config.
func Open(relPath string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	full := filepath.Join(home, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
