//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// isProcessAlive reports whether pid is running.
//
// Signal 0 does not deliver anything: it only checks that a process with this
// pid exists and that we may signal it. ESRCH means it is gone; any other
// error (EPERM, for instance) still means it is alive.
func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

// backgroundSysProcAttr is the platform half of launchBackground.
//
// Setsid detaches the child into its own session so it survives the parent
// terminal going away — the Unix equivalent of CREATE_NEW_PROCESS_GROUP
// combined with a clean process-group break.
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true,
	}
}

// ── Auto-start (XDG autostart) ──────────────────────────────
//
// Desktop Linux reads ~/.config/autostart/*.desktop on login. That is the
// portable user-level hook across GNOME, KDE, XFCE and friends.
//
// Headless servers have no desktop session; the README documents a systemd
// user unit for that case rather than inventing a second installer here.

func autoStartDesktopPath(component string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "autostart", "peerfold-"+component+".desktop"), nil
}

// setAutoStart writes an XDG autostart entry for a component.
func setAutoStart(component, args string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	path, err := autoStartDesktopPath(component)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	execLine := exe
	for _, a := range strings.Fields(args) {
		execLine += " " + quoteDesktopArg(a)
	}
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=peerfold %s
Comment=peerfold federated filesystem
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, component, execLine)
	return os.WriteFile(path, []byte(content), 0o644)
}

// removeAutoStart deletes the XDG autostart entry.
func removeAutoStart(component string) error {
	path, err := autoStartDesktopPath(component)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// hasAutoStart reports whether an XDG autostart entry exists.
func hasAutoStart(component string) bool {
	path, err := autoStartDesktopPath(component)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// quoteDesktopArg quotes one Exec= argument if it contains spaces or quotes.
// The Desktop Entry spec has its own escaping rules; this covers the paths a
// peerfold install actually produces.
func quoteDesktopArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// defaultShareDir returns the interactive setup's suggested share directory.
func defaultShareDir(clientName string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join("/tmp", "peerfold-demo", clientName)
	}
	return filepath.Join(home, "peerfold-demo", clientName)
}
