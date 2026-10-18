// Package logging sets up the always-on diagnostic log the server and client
// write to, so a run can be reconstructed afterwards without a debug flag.
package logging

import (
	"os"
	"path/filepath"
)

// Open returns an append-mode file for the named log, creating the per-user
// peerfold-logs directory on first use. The returned file is the standard log
// package's output; the caller closes it on exit.
//
// The name is a bare file name such as "server.log" or "client-alice.log".
// Everything lives under one directory because the point is a fixed, findable
// place rather than a hidden one per component.
func Open(name string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "peerfold-logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
