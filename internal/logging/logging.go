// Package logging is the always-on diagnostic log the server and client write
// to, so a run can be reconstructed afterwards without a debug flag.
//
// Infof, Debugf and Errorf write through the standard log package, whose output
// the command layer points at the component's own log file. The level is a
// prefix on the line rather than a filter: the log is always on, and the level
// says what kind of event the line records.
package logging

import (
	"log"
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
	f, err := os.OpenFile(full, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	// An existing file may have been created 0644 by an earlier version.
	_ = f.Chmod(0o600)
	return f, nil
}

// Infof records a normal operation: a request served, a change applied, a peer
// arriving or leaving.
func Infof(format string, args ...any) {
	log.Printf("[info] "+format, args...)
}

// Debugf records detail that helps trace a request through the layers, such as a
// byte range or an internal state transition.
func Debugf(format string, args ...any) {
	log.Printf("[debug] "+format, args...)
}

// Errorf records a failure worth knowing about.
func Errorf(format string, args ...any) {
	log.Printf("[error] "+format, args...)
}
