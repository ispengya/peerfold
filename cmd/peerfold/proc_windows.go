//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// isProcessAlive reports whether pid is running.
//
// On Windows, os.FindProcess always succeeds; we must actually query the
// process. Opening with PROCESS_QUERY_LIMITED_INFORMATION is the lightest way.
func isProcessAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	// STILL_ACTIVE = 259
	return code == 259
}

// backgroundSysProcAttr is the platform half of launchBackground.
//
// CREATE_NEW_PROCESS_GROUP puts the child in its own process group so a
// Ctrl-C on the parent console does not also hit it.
func backgroundSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

// ── Auto-start (Windows registry) ───────────────────────────

const autoStartRegPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// setAutoStart registers a component to start on login.
func setAutoStart(component, args string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	value := fmt.Sprintf(`"%s" %s`, exe, args)
	return writeRegistry(autoStartRegPath, "peerfold-"+component, value)
}

// removeAutoStart removes the auto-start registration.
func removeAutoStart(component string) error {
	return deleteRegistry(autoStartRegPath, "peerfold-"+component)
}

// hasAutoStart checks if a component is registered for auto-start.
func hasAutoStart(component string) bool {
	_, err := readRegistry(autoStartRegPath, "peerfold-"+component)
	return err == nil
}

// ── Registry helpers (via reg.exe) ──────────────────────────

func writeRegistry(path, name, value string) error {
	cmd := exec.Command("reg", "add",
		`HKCU\`+path, "/v", name, "/t", "REG_SZ", "/d", value, "/f")
	return cmd.Run()
}

func readRegistry(path, name string) (string, error) {
	cmd := exec.Command("reg", "query",
		`HKCU\`+path, "/v", name)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	// Parse output: "name    REG_SZ    value"
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, name) {
			parts := strings.SplitN(line, "REG_SZ", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}
	return "", fmt.Errorf("not found")
}

func deleteRegistry(path, name string) error {
	cmd := exec.Command("reg", "delete",
		`HKCU\`+path, "/v", name, "/f")
	return cmd.Run()
}

// defaultShareDir returns the interactive setup's suggested share directory.
//
// D: is the preferred data drive; a machine with only C: falls back to the
// user profile rather than refusing to suggest anything. Shares are kept in a
// subdirectory instead of the drive root so the default never indexes an
// entire disk, and beside the mount point rather than inside it because the
// mount point must not exist when WinFsp mounts.
func defaultShareDir(clientName string) string {
	root := `D:\`
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			root = home
		} else {
			root = `C:\`
		}
	}
	return filepath.Join(root, "peerfold-shares", clientName)
}
