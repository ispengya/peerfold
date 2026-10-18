package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"peerfold/internal/config"
)

// daemonFlag is the internal flag that marks a process as the background
// child of an interactive start. It suppresses prompts and runs normally.
const daemonFlag = "--daemon"

// interactiveServer guides the user through starting the server.
// It is called when `peerfold server` runs without flags.
func interactiveServer(in io.Reader, out, errw io.Writer) int {
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
		listen := promptString(in, out, "Listen address", "127.0.0.1:7000")

		cfg = config.Default()
		cfg.Listen = listen

		if err := config.Save(cfgPath, cfg); err != nil {
			fmt.Fprintf(errw, "  [X] Failed to save config: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "\n  [OK] Config saved: %s\n\n", cfgPath)
	} else {
		fmt.Fprintf(out, "  [OK] Loaded config: %s\n", cfgPath)
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
		listenFlag = "127.0.0.1:7000"
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

	// Foreground — run inline
	fmt.Fprintf(out, "  [OK] Starting server on %s (Ctrl-C to stop)\n\n", listenFlag)
	return cmdServer([]string{"--listen", listenFlag}, out, errw)
}

// interactiveClient guides the user through starting a client.
// It is called when `peerfold client` runs without flags.
func interactiveClient(in io.Reader, out, errw io.Writer) int {
	home, _ := os.UserHomeDir()

	fmt.Fprintf(out, "\n  peerfold client\n")
	fmt.Fprintf(out, "  ------------------------------------\n\n")

	// 1. Determine name first (needed for config path)
	name := os.Getenv("COMPUTERNAME")
	if name == "" {
		name = "peerfold"
	}

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
		shareDir := promptString(in, out, "Share directory", "C:\\peerfold-demo\\"+name)

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

	// 3. Build CLI args
	cliArgs := []string{"client", "--daemon",
		"--server", cfg.Server,
		"--name", cfg.ClientName,
	}
	for s, p := range cfg.Shares {
		cliArgs = append(cliArgs, "--share", fmt.Sprintf("%s=%s", s, p))
	}

	if mode == 1 {
		// Background
		pid, err := launchBackground(cliArgs)
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

	// Foreground — run inline
	fmt.Fprintf(out, "  [OK] Starting client '%s' (Ctrl-C to stop)\n\n", cfg.ClientName)
	return cmdClient(cliArgs[1:], out, errw)
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
