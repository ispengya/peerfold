package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"peerfold/internal/config"
)

// ── PID management ─────────────────────────────────────────

// pidFilePath returns the PID file for a component ("server" or "client-<name>").
func pidFilePath(component string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := "peerfold-" + component
	return filepath.Join(home, dir, component+".pid"), nil
}

// writePID records the process ID of a running component.
func writePID(component string, pid int) error {
	path, err := pidFilePath(component)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

// readPID returns the recorded PID and whether it is alive.
func readPID(component string) (int, bool, error) {
	path, err := pidFilePath(component)
	if err != nil {
		return 0, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, err
	}
	return pid, isProcessAlive(pid), nil
}

// clearPID removes the PID file.
func clearPID(component string) error {
	path, err := pidFilePath(component)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// isProcessAlive checks if a process with the given PID is running.
func isProcessAlive(pid int) bool {
	// On Windows, os.FindProcess always succeeds; we must actually try to
	// query the process. Opening with PROCESS_QUERY_LIMITED_INFORMATION is
	// the lightest way.
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
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

// killProcess terminates a process, first gracefully then forcefully.
func killProcess(pid int) error {
	if !isProcessAlive(pid) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// Try graceful first
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Force kill
	return proc.Kill()
}

// ── Background process launch ──────────────────────────────

// launchBackground starts peerfold.exe in the background and returns its PID.
func launchBackground(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	// Redirect output to log file
	component := "peerfold"
	if len(args) > 0 {
		component = args[0]
	}
	home, err := os.UserHomeDir()
	if err == nil {
		logDir := filepath.Join(home, "peerfold-"+component)
		os.MkdirAll(logDir, 0o755)
		logFile := filepath.Join(logDir, component+".log")
		if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			cmd.Stdout = f
			cmd.Stderr = f
			defer f.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

// ── Auto-start registry (Windows) ──────────────────────────

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

// ── Registry helpers (Windows-specific via reg.exe) ────────

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

// ── Interactive prompts ────────────────────────────────────

func promptString(in io.Reader, out io.Writer, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(out, "  %s [%s]: ", label, defaultVal)
	} else {
		fmt.Fprintf(out, "  %s: ", label)
	}
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			return defaultVal
		}
		return line
	}
	return defaultVal
}

func promptChoice(in io.Reader, out io.Writer, label string, options []string, defaultIdx int) int {
	for i, opt := range options {
		marker := " "
		if i == defaultIdx {
			marker = ">"
		}
		fmt.Fprintf(out, "  %s %s\n", marker, opt)
	}
	fmt.Fprintf(out, "  Choose [1-%d, default %d]: ", len(options), defaultIdx+1)
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
	}
	return defaultIdx
}

func promptBool(in io.Reader, out io.Writer, label string, defaultVal bool) bool {
	hint := "y/N"
	if defaultVal {
		hint = "Y/n"
	}
	fmt.Fprintf(out, "  %s [%s]: ", label, hint)
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		line := strings.ToLower(strings.TrimSpace(scanner.Text()))
		switch line {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
	return defaultVal
}

// ── The service commands ───────────────────────────────────

// cmdStatus shows running state of all peerfold components.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	home, _ := os.UserHomeDir()

	fmt.Fprintf(stdout, "\n  peerfold status\n")
	fmt.Fprintf(stdout, "  ------------------------------------\n\n")

	// Server
	serverPID, serverAlive, _ := readPID("server")
	serverStatus := "stopped"
	if serverAlive {
		serverStatus = fmt.Sprintf("running (PID %d)", serverPID)
	}
	fmt.Fprintf(stdout, "  Server:  %s\n", serverStatus)
	fmt.Fprintf(stdout, "           Log: %s\n", filepath.Join(home, "peerfold-server", "log.txt"))

	// Auto-start
	if hasAutoStart("server") {
		fmt.Fprintf(stdout, "           Auto-start: enabled\n")
	} else {
		fmt.Fprintf(stdout, "           Auto-start: disabled\n")
	}

	// Clients
	clientDirs, _ := filepath.Glob(filepath.Join(home, "peerfold-client-*"))
	fmt.Fprintf(stdout, "\n")
	for _, dir := range clientDirs {
		name := strings.TrimPrefix(filepath.Base(dir), "peerfold-client-")
		component := "client-" + name
		pid, alive, _ := readPID(component)
		status := "stopped"
		if alive {
			status = fmt.Sprintf("running (PID %d)", pid)
		}
		fmt.Fprintf(stdout, "  Client %-15s %s\n", name+":", status)
		fmt.Fprintf(stdout, "                   Log: %s\n", filepath.Join(dir, "log.txt"))
		if hasAutoStart(component) {
			fmt.Fprintf(stdout, "                   Auto-start: enabled\n")
		}
	}

	if len(clientDirs) == 0 {
		fmt.Fprintf(stdout, "  Client:  (none configured)\n")
	}

	fmt.Fprintf(stdout, "\n")
	return 0
}

// cmdStop stops a running component.
func cmdStop(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: peerfold stop <server|client|all>")
		return 2
	}

	target := args[0]
	switch target {
	case "all":
		stopped := 0
		if stopComponent("server", stdout) {
			stopped++
		}
		home, _ := os.UserHomeDir()
		dirs, _ := filepath.Glob(filepath.Join(home, "peerfold-client-*"))
		for _, dir := range dirs {
			name := strings.TrimPrefix(filepath.Base(dir), "peerfold-client-")
			if stopComponent("client-"+name, stdout) {
				stopped++
			}
		}
		fmt.Fprintf(stdout, "\n  Stopped %d component(s).\n", stopped)
		return 0

	case "server":
		if stopComponent("server", stdout) {
			fmt.Fprintf(stdout, "\n  Server stopped.\n")
		} else {
			fmt.Fprintf(stdout, "\n  Server is not running.\n")
		}
		return 0

	case "client":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: peerfold stop client <name>")
			return 2
		}
		component := "client-" + args[1]
		if stopComponent(component, stdout) {
			fmt.Fprintf(stdout, "\n  Client %s stopped.\n", args[1])
		} else {
			fmt.Fprintf(stdout, "\n  Client %s is not running.\n", args[1])
		}
		return 0

	default:
		fmt.Fprintf(stderr, "unknown target %q\n", target)
		return 2
	}
}

func stopComponent(component string, out io.Writer) bool {
	pid, alive, err := readPID(component)
	if err != nil || !alive {
		clearPID(component)
		return false
	}
	fmt.Fprintf(out, "  Stopping %s (PID %d)...", component, pid)
	if err := killProcess(pid); err != nil {
		fmt.Fprintf(out, " force killed\n")
	} else {
		fmt.Fprintf(out, " done\n")
	}
	clearPID(component)
	return true
}

// cmdRestart restarts a component in background mode.
func cmdRestart(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: peerfold restart <server|client> [name]")
		return 2
	}

	// Stop first
	_ = cmdStop(args, stdout, stderr)
	time.Sleep(500 * time.Millisecond)

	// Start in background using saved config
	home, _ := os.UserHomeDir()

	switch args[0] {
	case "server":
		cfgPath := filepath.Join(home, "peerfold-server", "config.json")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "  [X] No config found. Run 'peerfold server' first.\n")
			return 1
		}
		listen := cfg.Listen
		if listen == "" {
			listen = "127.0.0.1:7000"
		}
		pid, err := launchBackground([]string{"server", "--daemon", "--listen", listen})
		if err != nil {
			fmt.Fprintf(stderr, "  [X] Failed to start: %v\n", err)
			return 1
		}
		writePID("server", pid)
		fmt.Fprintf(stdout, "\n  [OK] Server restarted (PID %d)\n", pid)
		fmt.Fprintf(stdout, "       Address: %s\n\n", listen)
		return 0

	case "client":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		if name == "" {
			fmt.Fprintln(stderr, "usage: peerfold restart client <name>")
			return 2
		}
		cfgPath := filepath.Join(home, "peerfold-client-"+name, "config.json")
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "  [X] No config found for client '%s'.\n", name)
			return 1
		}
		cliArgs := []string{"client", "--daemon", "--server", cfg.Server, "--name", cfg.ClientName}
		for s, p := range cfg.Shares {
			cliArgs = append(cliArgs, "--share", fmt.Sprintf("%s=%s", s, p))
		}
		pid, err := launchBackground(cliArgs)
		if err != nil {
			fmt.Fprintf(stderr, "  [X] Failed to start: %v\n", err)
			return 1
		}
		writePID("client-"+cfg.ClientName, pid)
		fmt.Fprintf(stdout, "\n  [OK] Client '%s' restarted (PID %d)\n\n", cfg.ClientName, pid)
		return 0

	default:
		fmt.Fprintf(stderr, "unknown target %q\n", args[0])
		return 2
	}
}
