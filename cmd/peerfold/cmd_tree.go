package main

import (
	"flag"
	"fmt"
	"io"

	"peerfold/internal/config"
	"peerfold/internal/node"
	"peerfold/internal/viewer"
)

// cmdTree dispatches the tree commands. Only ls exists so far: the write
// commands arrive in later tasks and still report pending work.
func cmdTree(cmd string, args []string, stdout, stderr io.Writer) int {
	switch cmd {
	case "ls":
		return cmdLs(args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "TODO: %s %v\n", cmd, args)
		return 0
	}
}

// cmdLs lists the aggregated tree at PATH, or the top level when PATH is
// omitted.
//
// It reports to stdout and errors to stderr, so redirecting stdout captures
// exactly the listing and nothing else.
func cmdLs(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: the per-user config path)")
	fs.Usage = func() {
		fmt.Fprint(stderr, lsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(stderr, "ls: at most one PATH can be listed, got %d\n", fs.NArg())
		return 2
	}
	path := ""
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}

	path0 := *cfgPath
	if path0 == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "ls: locate config: %v\n", err)
			return 1
		}
		path0 = p
	}
	cfg, err := loadOrDefault(path0)
	if err != nil {
		fmt.Fprintf(stderr, "ls: read config %s: %v\n", path0, err)
		return 1
	}
	if cfg.Server == "" {
		fmt.Fprintf(stderr, "ls: no server configured: set one with `peerfold client --server`\n")
		return 1
	}

	v, err := viewer.Dial(cfg.Server)
	if err != nil {
		fmt.Fprintf(stderr, "ls: %v\n", err)
		return 1
	}
	defer v.Close()

	nodes, err := v.List(path)
	if err != nil {
		fmt.Fprintf(stderr, "ls: %v\n", err)
		return 1
	}
	for _, n := range nodes {
		fmt.Fprintf(stdout, "%s\t%s\n", kindLetter(n.Kind), n.Path)
	}
	return 0
}

// kindLetter is the single character ls prints for a node's kind: the same
// letters ls uses, so the output reads the way a user expects.
func kindLetter(k node.Kind) string {
	switch k {
	case node.KindDir:
		return "d"
	case node.KindFile:
		return "f"
	default:
		// Anything else is a kind this version does not know how to name, so
		// it is shown as unknown rather than guessed at.
		return "?"
	}
}

const lsUsage = `usage: peerfold ls [PATH]

Lists the aggregated tree. PATH is empty for the connected clients, a client
name for its shares, or client/path for a directory inside one.

flags:
`
