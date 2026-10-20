package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/mount"
)

// dialAddress is the address a local client should dial for a server bound to
// listen. A wildcard bind (":7000", "0.0.0.0:7000", "[::]:7000") is not a
// dialable address on every platform, so local tools use loopback instead.
func dialAddress(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listen
}

// daemonFlag is the internal flag that marks a process as the background
// child of an interactive start. It suppresses prompts and runs normally.
const daemonFlag = "--daemon"

// interactiveServer guides the user through starting the server.
// It is called when `peerfold server` runs without flags.
func interactiveServer(input io.Reader, out, errw io.Writer) int {
	in := bufio.NewReader(input)
	home, _ := os.UserHomeDir()
	cfgDir := filepath.Join(home, "peerfold-server")
	cfgPath := filepath.Join(cfgDir, "config.json")

	fmt.Fprintf(out, "\n  peerfold server\n")
	fmt.Fprintf(out, "  ------------------------------------\n\n")

	// 1. Load or create config
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// No config yet — prompt for settings
		fmt.Fprintf(out, "  No config found. Let's set one up.\n\n")
		// The default is the wildcard on purpose: a loopback-only server
		// cannot serve a mount on another machine, which is the reason most
		// people run one.
		listen := promptString(in, out, "Listen address", "0.0.0.0:7000")

		cfg = config.Default()
		cfg.Listen = listen
		// The same file is what ls/cat/mount read the address from, so a
		// config created here has to answer that question too: "listen" is
		// what the server binds, "server" is what local clients dial.
		cfg.Server = dialAddress(listen)

		if err := config.Save(cfgPath, cfg); err != nil {
			fmt.Fprintf(errw, "  [X] Failed to save config: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "\n  [OK] Config saved: %s\n\n", cfgPath)
	} else {
		fmt.Fprintf(out, "  [OK] Loaded config: %s\n", cfgPath)
		if cfg.Server == "" && cfg.Listen != "" {
			// Older configs predate the two-field split and hold only the
			// listen address. Fill in the dial address so ls/cat/mount work.
			cfg.Server = dialAddress(cfg.Listen)
			if err := config.Save(cfgPath, cfg); err != nil {
				fmt.Fprintf(errw, "  [!] Could not save the server address: %v\n", err)
			}
		}
		fmt.Fprintf(out, "       Listen: %s\n\n", cfg.Listen)
	}

	// 2. Ask run mode
	fmt.Fprintf(out, "  Run mode:\n")
	mode := promptChoice(in, out, "", []string{"Foreground (this terminal)", "Background (detached)"}, 0)

	// 3. If background, ask auto-start
	if mode == 1 {
		autoStart := promptBool(in, out, "Start on login?", false)
		if autoStart {
			if err := setAutoStart("server", "server --daemon"); err != nil {
				fmt.Fprintf(errw, "  [!] Failed to set auto-start: %v\n", err)
			} else {
				fmt.Fprintf(out, "  [OK] Auto-start enabled\n")
			}
		} else {
			_ = removeAutoStart("server")
		}
	}

	fmt.Fprintf(out, "\n")

	// 4. Start
	listenFlag := cfg.Listen
	if listenFlag == "" {
		listenFlag = "0.0.0.0:7000"
	}

	if mode == 1 {
		// Background
		pid, err := launchBackground([]string{"server", "--daemon", "--listen", listenFlag})
		if err != nil {
			fmt.Fprintf(errw, "  [X] Failed to start: %v\n", err)
			return 1
		}
		writePID("server", pid)
		fmt.Fprintf(out, "  [OK] Server started in background (PID %d)\n", pid)
		fmt.Fprintf(out, "       Address: %s\n", listenFlag)
		fmt.Fprintf(out, "       Log:     %s\n\n", filepath.Join(cfgDir, "log.txt"))
		fmt.Fprintf(out, "  Commands: peerfold status | peerfold stop server\n\n")
		return 0
	}

	// Foreground — record PID so status can see it, then run inline
	writePID("server", os.Getpid())
	fmt.Fprintf(out, "  [OK] Starting server on %s (Ctrl-C to stop)\n\n", listenFlag)
	code := cmdServer([]string{"--listen", listenFlag}, out, errw)
	clearPID("server")
	return code
}

// interactiveClient guides the user through starting a client.
// It is called when `peerfold client` runs without flags.
func interactiveClient(input io.Reader, out, errw io.Writer) int {
	in := bufio.NewReader(input)
	home, _ := os.UserHomeDir()

	fmt.Fprintf(out, "\n  peerfold client\n")
	fmt.Fprintf(out, "  ------------------------------------\n\n")

	// 1. Determine name first (needed for config path)
	name := defaultClientName()

	// Check if there's exactly one existing client config
	existing := findSingleClientConfig(home)
	var cfg config.Config
	var cfgPath string

	if existing != "" {
		cfgPath = existing
		if c, err := config.Load(cfgPath); err == nil {
			cfg = c
			fmt.Fprintf(out, "  [OK] Loaded config: %s\n", cfgPath)
			fmt.Fprintf(out, "       Server:  %s\n", cfg.Server)
			fmt.Fprintf(out, "       Name:    %s\n", cfg.ClientName)
			for s, p := range cfg.Shares {
				fmt.Fprintf(out, "       Share:   %s = %s\n", s, p)
			}
			fmt.Fprintf(out, "\n")
		}
	}

	if cfg.Server == "" {
		fmt.Fprintf(out, "  No config found. Let's set one up.\n\n")

		name = promptString(in, out, "Client name", name)
		server := promptString(in, out, "Server address", "127.0.0.1:7000")
		shareName := promptString(in, out, "Share name", "docs")
		shareDir := promptString(in, out, "Share directory", defaultShareDir(name))

		// The generated default lives under peerfold-demo and does not exist on
		// a fresh machine. Creating it here is what makes the default usable;
		// without it the client saved the config, dialed, and then failed with
		// "lstat <dir>: no such file or directory".
		_, statErr := os.Stat(shareDir)
		if err := os.MkdirAll(shareDir, 0o755); err != nil {
			fmt.Fprintf(errw, "  [X] Failed to create share directory %s: %v\n", shareDir, err)
			return 1
		}
		if os.IsNotExist(statErr) {
			fmt.Fprintf(out, "  [OK] Created share directory: %s\n", shareDir)
		}

		cfg = config.Default()
		cfg.Server = server
		cfg.ClientName = name
		cfg.Shares = map[string]string{shareName: shareDir}

		cfgPath = filepath.Join(home, "peerfold-client-"+name, "config.json")
		if err := config.Save(cfgPath, cfg); err != nil {
			fmt.Fprintf(errw, "  [X] Failed to save config: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "\n  [OK] Config saved: %s\n\n", cfgPath)
	}

	component := "client-" + cfg.ClientName

	// 2. Ask run mode
	fmt.Fprintf(out, "  Run mode:\n")
	mode := promptChoice(in, out, "", []string{"Foreground (this terminal)", "Background (detached)"}, 0)

	fmt.Fprintf(out, "\n")

	// 3. Build CLI args. --daemon marks a background child and is stripped by
	// dispatch before cmdClient parses flags. The foreground path calls
	// cmdClient directly, so it must not carry the flag: doing so made
	// `peerfold client` fail with "flag provided but not defined: -daemon".
	cliArgs := []string{
		"--server", cfg.Server,
		"--name", cfg.ClientName,
	}
	for s, p := range cfg.Shares {
		cliArgs = append(cliArgs, "--share", fmt.Sprintf("%s=%s", s, p))
	}

	if mode == 1 {
		// Background
		launchArgs := append([]string{"client", daemonFlag}, cliArgs...)
		pid, err := launchBackground(launchArgs)
		if err != nil {
			fmt.Fprintf(errw, "  [X] Failed to start: %v\n", err)
			return 1
		}
		writePID(component, pid)
		fmt.Fprintf(out, "  [OK] Client '%s' started in background (PID %d)\n", cfg.ClientName, pid)
		fmt.Fprintf(out, "       Server: %s\n", cfg.Server)
		fmt.Fprintf(out, "       Log:    %s\n\n", filepath.Dir(cfgPath))
		fmt.Fprintf(out, "  Commands: peerfold status | peerfold stop client %s\n\n", cfg.ClientName)
		return 0
	}

	// Foreground — record PID so status can see it, then run inline
	writePID(component, os.Getpid())
	fmt.Fprintf(out, "  [OK] Starting client '%s' (Ctrl-C to stop)\n\n", cfg.ClientName)
	code := cmdClient(cliArgs, out, errw)
	clearPID(component)
	return code
}

// interactiveMount guides the user through mounting the federated tree.
// It is called when `peerfold mount` runs without flags.
//
// Mount is the read/write window onto the federation, so it only needs two
// things: where the server is and where the tree should appear. Both are
// written to the config before the mount starts, so the next run of
// `peerfold mount` can skip the questions.
func interactiveMount(input io.Reader, out, errw io.Writer) int {
	in := bufio.NewReader(input)
	cfgPath, err := config.MountPath()
	if err != nil {
		fmt.Fprintf(errw, "  [X] Failed to locate the mount config: %v\n", err)
		return 1
	}

	fmt.Fprintf(out, "\n  peerfold mount\n")
	fmt.Fprintf(out, "  ------------------------------------\n\n")

	cfg, migrated, loadErr := loadMountConfig(cfgPath, false)
	if loadErr != nil {
		cfg = config.Default()
		fmt.Fprintf(out, "  No config found. Let's set one up.\n\n")
	} else {
		if migrated {
			fmt.Fprintf(out, "  [OK] Using the settings saved by the previous version\n")
			fmt.Fprintf(out, "       They will be written to: %s\n", cfgPath)
		} else {
			fmt.Fprintf(out, "  [OK] Loaded config: %s\n", cfgPath)
		}
		if cfg.Server != "" {
			fmt.Fprintf(out, "       Server: %s\n", cfg.Server)
		}
		if cfg.MountPoint != "" {
			fmt.Fprintf(out, "       Mount:  %s\n", cfg.MountPoint)
		}
		fmt.Fprintf(out, "\n  Press Enter to keep a value in [brackets].\n\n")
	}

	server := cfg.Server
	if server == "" {
		server = "127.0.0.1:7000"
	}
	server = promptString(in, out, "Server address", server)

	mountPoint := cfg.MountPoint
	if mountPoint == "" {
		if p, err := mount.DefaultMountPoint(); err == nil {
			mountPoint = p
		}
	}
	mountPoint = promptString(in, out, "Mount point", mountPoint)

	if !filepath.IsAbs(mountPoint) {
		fmt.Fprintf(errw, "  [X] Mount point must be an absolute path (got %q)\n", mountPoint)
		return 1
	}

	cfg.Server = server
	cfg.MountPoint = mountPoint
	if err := config.Save(cfgPath, cfg); err != nil {
		fmt.Fprintf(errw, "  [X] Failed to save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "\n  [OK] Config saved: %s\n\n", cfgPath)

	// Run the mount with the values just gathered. The flags keep one code
	// path: interactive and scripted both end in runMount.
	return runMount([]string{"--server", server, "--mount", mountPoint}, out, errw)
}

// findSingleClientConfig finds a single existing client config file.
// If there are multiple, it returns empty (user must specify which).
func findSingleClientConfig(home string) string {
	matches, _ := filepath.Glob(filepath.Join(home, "peerfold-client-*", "config.json"))
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// defaultClientName returns the interactive setup's suggested client name.
//
// Windows sets COMPUTERNAME; elsewhere the host name is the identity, which
// is the same fallback cmd_client.go uses when --name is omitted.
func defaultClientName() string {
	if n := os.Getenv("COMPUTERNAME"); n != "" {
		return n
	}
	if n, err := os.Hostname(); err == nil && n != "" {
		return n
	}
	return "peerfold"
}

// isDaemon reports whether the process was launched as a background child.
func isDaemon(args []string) bool {
	for _, a := range args {
		if a == daemonFlag {
			return true
		}
	}
	return false
}

// stripDaemon removes the --daemon flag from args.
func stripDaemon(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a != daemonFlag {
			out = append(out, a)
		}
	}
	return out
}
