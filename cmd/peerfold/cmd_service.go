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

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/viewer"
)

// statusQueryTimeout bounds the status command's conversation with the server.
// Status is a snapshot and must stay quick even when the address in the config
// points at a machine that is down or gone.
const statusQueryTimeout = 2 * time.Second

// ── PID management ─────────────────────────────────────────
//
// Portable: the pid file layout is the same on every platform. Whether a pid
// is alive and how a process is launched live in proc_windows.go / proc_unix.go.

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

// killProcess terminates a process, first gracefully then forcefully.
//
// SIGTERM is deliverable on Unix. Windows has no equivalent: os.Process.Signal
// only implements Kill there, so a non-Kill signal fails instead of being
// delivered. That failure is used as the signal that there is no grace to wait
// for, rather than sleeping out the deadline and then killing anyway.
//
// The grace is long enough for a client to finish the request it was serving
// and wait for the server's goodbye acknowledgement. It is still bounded: a
// process that ignores the signal is killed rather than allowed to make the
// stop command hang.
func killProcess(pid int) error {
	if !isProcessAlive(pid) {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// Try graceful first.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if !isProcessAlive(pid) {
			return nil
		}
		return proc.Kill()
	}
	deadline := time.Now().Add(10 * time.Second)
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

// launchBackground starts this executable in the background and returns its PID.
func launchBackground(args []string) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = backgroundSysProcAttr()
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

// ── Interactive prompts ────────────────────────────────────
//
// The prompts share one *bufio.Reader for the whole interactive session. A
// fresh Scanner per question would read ahead past the newline and swallow
// the next answers whenever input arrives as a block (a pipe, a test, a
// here-doc). One reader keeps the leftovers for the next prompt.

func promptString(in *bufio.Reader, out io.Writer, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(out, "  %s [%s]: ", label, defaultVal)
	} else {
		fmt.Fprintf(out, "  %s: ", label)
	}
	line, err := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return defaultVal
	}
	if line == "" {
		return defaultVal
	}
	return line
}

func promptChoice(in *bufio.Reader, out io.Writer, label string, options []string, defaultIdx int) int {
	for i, opt := range options {
		marker := " "
		if i == defaultIdx {
			marker = ">"
		}
		fmt.Fprintf(out, "  %s %s\n", marker, opt)
	}
	fmt.Fprintf(out, "  Choose [1-%d, default %d]: ", len(options), defaultIdx+1)
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
		return n - 1
	}
	return defaultIdx
}

func promptBool(in *bufio.Reader, out io.Writer, label string, defaultVal bool) bool {
	hint := "y/N"
	if defaultVal {
		hint = "Y/n"
	}
	fmt.Fprintf(out, "  %s [%s]: ", label, hint)
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	switch line {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return defaultVal
}

// ── The service commands ───────────────────────────────────

// cmdStatus shows running state of all peerfold components.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	home, _ := os.UserHomeDir()

	fmt.Fprintf(stdout, "\n  peerfold status\n")
	fmt.Fprintf(stdout, "  ------------------------------------\n\n")

	// Ask the configured server first. Its reachability decides whether the
	// Server line describes a local process or the remote aggregation this
	// machine belongs to: "stopped" here means "no local server", and on a
	// machine that only mounts or supplies that would be misleading.
	addr := statusServerAddress()
	var (
		remoteMounts    []viewer.Mount
		remoteClients   []viewer.Client
		mountsErr       error
		clientsErr      error
		remoteReachable bool
	)
	if addr != "" {
		if v, err := viewer.DialTimeout(addr, statusQueryTimeout); err == nil {
			remoteReachable = true
			remoteMounts, mountsErr = v.ListMounts()
			remoteClients, clientsErr = v.ListClients()
			v.Close()
		}
	}

	// A local server config means this machine is meant to run the server.
	// Only then are the log path and auto-start line about anything here.
	localServerConfigured := false
	if path, err := config.ServerPath(); err == nil {
		if _, err := config.Load(path); err == nil {
			localServerConfigured = true
		}
	}

	// Server
	serverPID, serverAlive, _ := readPID("server")
	fmt.Fprintf(stdout, "  Server:  ")
	switch {
	case serverAlive:
		fmt.Fprintf(stdout, "running (PID %d)\n", serverPID)
	case remoteReachable:
		// A reachable server is the fact that matters, whether it is this
		// machine's process or another one's. The log and auto-start lines
		// describe a local server and are omitted: they would point at a
		// process that is not the one answering.
		fmt.Fprintf(stdout, "running at %s (remote)\n", addr)
	case localServerConfigured:
		fmt.Fprintf(stdout, "stopped\n")
	case addr != "":
		fmt.Fprintf(stdout, "unreachable (%s)\n", addr)
	default:
		fmt.Fprintf(stdout, "stopped\n")
	}
	if localServerConfigured && (serverAlive || !remoteReachable) {
		fmt.Fprintf(stdout, "           Log: %s\n", filepath.Join(home, "peerfold-server", "log.txt"))
		if hasAutoStart("server") {
			fmt.Fprintf(stdout, "           Auto-start: enabled\n")
		}
	}

	// Clients: the local processes plus the suppliers the server reports. The
	// name is the identity, so a client that is both local and connected is
	// printed once, as local. A local config whose process is stopped is
	// skipped when the server still reports that name: the connection is the
	// fact that matters.
	clientDirs, _ := filepath.Glob(filepath.Join(home, "peerfold-client-*"))
	remoteByName := make(map[string]viewer.Client, len(remoteClients))
	for _, c := range remoteClients {
		remoteByName[c.Name] = c
	}
	fmt.Fprintf(stdout, "\n")
	shown := make(map[string]bool, len(clientDirs)+len(remoteClients))
	for _, dir := range clientDirs {
		name := strings.TrimPrefix(filepath.Base(dir), "peerfold-client-")
		pid, alive, _ := readPID("client-" + name)
		switch {
		case alive:
			fmt.Fprintf(stdout, "  Client:  %s (running, PID %d)\n", name, pid)
			shown[name] = true
		case remoteByName[name].Name == "":
			fmt.Fprintf(stdout, "  Client:  %s (stopped)\n", name)
			shown[name] = true
		}
	}
	for _, c := range remoteClients {
		if shown[c.Name] {
			continue
		}
		fmt.Fprintf(stdout, "  Client:  %s (remote)\n", c.Name)
		shown[c.Name] = true
	}
	if len(shown) == 0 {
		fmt.Fprintf(stdout, "  Client:  (none configured)\n")
	}

	// Mount. It is a foreground process, so the pid file is the only way to
	// see it from another terminal. The values come from the running mount's
	// state file when it left one: --server and --mount need not match the
	// config, and status should report what was dialled, not what was saved.
	mountPID, mountAlive, _ := readPID("mount")
	state, stateErr := readMountState()
	var (
		mountCfg    config.Config
		mountCfgErr error
	)
	if path, err := config.MountPath(); err != nil {
		mountCfgErr = err
	} else {
		mountCfg, mountCfgErr = config.Load(path)
	}
	point := mountCfg.MountPoint
	if stateErr == nil {
		point = state.MountPoint
	}

	fmt.Fprintf(stdout, "\n")
	switch {
	case mountAlive:
		fmt.Fprintf(stdout, "  Mount:   running (PID %d)\n", mountPID)
	case mountCfgErr == nil || stateErr == nil:
		fmt.Fprintf(stdout, "  Mount:   stopped\n")
	default:
		fmt.Fprintf(stdout, "  Mount:   (none configured)\n")
	}
	if point != "" {
		fmt.Fprintf(stdout, "           Point:   %s\n", point)
	}

	// Other machines' mounts. This machine's own mount is filtered out by its
	// point: it is already the Mount line above, and listing it twice under two
	// names (local and server view) reads like two mounts.
	var others []viewer.Mount
	for _, m := range remoteMounts {
		if mountAlive && point != "" && m.MountPoint == point {
			continue
		}
		others = append(others, m)
	}
	if len(others) > 0 {
		fmt.Fprintf(stdout, "\n  Remote mounts:\n")
		for _, m := range others {
			name := m.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Fprintf(stdout, "    %s  %s\n", name, m.MountPoint)
		}
	}

	// A server from before these queries existed answers with "not
	// implemented". One line says the fix; the rest of the output is already
	// accurate.
	if remoteReachable && statusUnsupported(mountsErr) && statusUnsupported(clientsErr) {
		fmt.Fprintf(stdout, "\n  Remote details: (server does not support them; update and restart it)\n")
	}

	fmt.Fprintf(stdout, "\n")
	return 0
}

// statusUnsupported reports whether err is an older server's answer to a query
// it does not implement.
func statusUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not implemented")
}

// statusServerAddress returns the server whose mounts and clients status should
// ask about, or "" when no local config names one.
//
// The server's own config comes first: on the machine that aggregates, that is
// the authority. Then the mount config and any client config, so a machine that
// runs only one role still reports the federation it belongs to.
func statusServerAddress() string {
	if path, err := config.ServerPath(); err == nil {
		if cfg, err := config.Load(path); err == nil {
			if cfg.Server != "" {
				return cfg.Server
			}
			if cfg.Listen != "" {
				// An older config predates the listen/server split.
				return dialAddress(cfg.Listen)
			}
		}
	}
	if path, err := config.MountPath(); err == nil {
		if cfg, err := config.Load(path); err == nil && cfg.Server != "" {
			return cfg.Server
		}
	}
	home, _ := os.UserHomeDir()
	dirs, _ := filepath.Glob(filepath.Join(home, "peerfold-client-*"))
	for _, dir := range dirs {
		if cfg, err := config.Load(filepath.Join(dir, "config.json")); err == nil && cfg.Server != "" {
			return cfg.Server
		}
	}
	return ""
}

// cmdStop stops a running component.
func cmdStop(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: peerfold stop <server|client|mount|all>")
		return 2
	}

	target := args[0]
	switch target {
	case "all":
		stopped := 0
		// The mount goes first: it is the consumer, and unmounting before the
		// server disappears lets it flush and go away cleanly instead of
		// entering its reconnect loop on the way out.
		if stopComponent("mount", stdout) {
			stopped++
		}
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

	case "mount":
		if stopComponent("mount", stdout) {
			fmt.Fprintf(stdout, "\n  Mount stopped.\n")
		} else {
			fmt.Fprintf(stdout, "\n  Mount is not running.\n")
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
			listen = config.Default().Listen
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
