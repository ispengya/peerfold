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
// SIGTERM is portable: on Windows Go maps it to TerminateProcess, which is
// the only graceful-ish stop that OS offers a console process.
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
	server, point := mountCfg.Server, mountCfg.MountPoint
	if stateErr == nil {
		server, point = state.Server, state.MountPoint
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
	if server != "" {
		fmt.Fprintf(stdout, "           Server:  %s\n", server)
	}
	if point != "" {
		fmt.Fprintf(stdout, "           Point:   %s\n", point)
	}

	// Remote mounts and clients. The state above describes only this machine,
	// so the server is asked what is attached to it -- that is the only way a
	// mount or client on another machine becomes visible here. The query is
	// itself an ordinary viewer: it declares no mount point and is not a
	// supplier, so it never lists itself.
	if addr := statusServerAddress(); addr != "" {
		fmt.Fprintf(stdout, "\n")
		v, err := viewer.DialTimeout(addr, statusQueryTimeout)
		if err != nil {
			fmt.Fprintf(stdout, "  Remote server: (server %s unreachable)\n", addr)
		} else {
			mounts, mountsErr := v.ListMounts()
			clients, clientsErr := v.ListClients()
			v.Close()

			// A server from before either query existed answers both with
			// "not implemented". One line says the fix; repeating it per query
			// would read like two different problems.
			if statusUnsupported(mountsErr) && statusUnsupported(clientsErr) {
				fmt.Fprintf(stdout, "  Remote server: (does not support listing mounts or clients; update and restart it)\n")
			} else {
				printRemoteMounts(stdout, addr, mounts, mountsErr)
				printRemoteClients(stdout, addr, clients, clientsErr)
			}
		}
	}

	fmt.Fprintf(stdout, "\n")
	return 0
}

// statusUnsupported reports whether err is an older server's answer to a query
// it does not implement.
func statusUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not implemented")
}

// printRemoteMounts writes the mounts attached to the configured server.
func printRemoteMounts(out io.Writer, addr string, mounts []viewer.Mount, err error) {
	switch {
	case err != nil:
		// The wire's own wording is not what a user needs to read: the fix is
		// to update and restart the server.
		if statusUnsupported(err) {
			fmt.Fprintf(out, "  Remote mounts: (server does not support listing mounts; update and restart it)\n")
		} else {
			fmt.Fprintf(out, "  Remote mounts: (%v)\n", err)
		}
	case len(mounts) == 0:
		fmt.Fprintf(out, "  Remote mounts: (none)\n")
	default:
		fmt.Fprintf(out, "  Remote mounts via %s:\n", addr)
		for _, m := range mounts {
			name := m.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Fprintf(out, "    %s  %s", name, m.MountPoint)
			printPeerDetail(out, m.RemoteAddr, m.Since)
			fmt.Fprintln(out)
		}
	}
}

// printRemoteClients writes the suppliers attached to the configured server.
func printRemoteClients(out io.Writer, addr string, clients []viewer.Client, err error) {
	switch {
	case err != nil:
		if statusUnsupported(err) {
			fmt.Fprintf(out, "  Clients: (server does not support listing clients; update and restart it)\n")
		} else {
			fmt.Fprintf(out, "  Clients: (%v)\n", err)
		}
	case len(clients) == 0:
		fmt.Fprintf(out, "  Clients via %s: (none)\n", addr)
	default:
		fmt.Fprintf(out, "  Clients via %s:\n", addr)
		for _, c := range clients {
			name := c.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Fprintf(out, "    %s", name)
			if c.Aggregate {
				fmt.Fprintf(out, "  (aggregate)")
			}
			printPeerDetail(out, c.RemoteAddr, c.Since)
			fmt.Fprintln(out)
		}
	}
}

// printPeerDetail writes the "from ... up ..." tail shared by remote mounts and
// clients. Missing values are omitted rather than printed as empty fields.
func printPeerDetail(out io.Writer, remoteAddr string, since time.Time) {
	if remoteAddr != "" {
		fmt.Fprintf(out, "  from %s", remoteAddr)
	}
	if !since.IsZero() {
		up := time.Since(since)
		if up < 0 {
			// Server and this machine disagree about the time; a negative age
			// would read as nonsense.
			up = 0
		}
		fmt.Fprintf(out, "  up %s", up.Round(time.Second))
	}
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
