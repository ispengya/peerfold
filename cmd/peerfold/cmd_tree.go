package main

import (
	"flag"
	"fmt"
	"io"

	"peerfold/internal/config"
	"peerfold/internal/node"
	"peerfold/internal/viewer"
)

// cmdTree dispatches the tree commands. The write commands arrive in later tasks
// and still report pending work.
func cmdTree(cmd string, args []string, stdout, stderr io.Writer) int {
	switch cmd {
	case "ls":
		return cmdLs(args, stdout, stderr)
	case "cat":
		return cmdCat(args, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "TODO: %s %v\n", cmd, args)
		return 0
	}
}

// connect resolves the config, dials the server, and returns the viewer.
//
// Every read-only command needs the same three steps — find the config, find the
// server in it, dial — and they fail the same way, so they are done once here
// instead of once per command with slightly different wording each time.
func connect(cmd, cfgPath string, stderr io.Writer) (*viewer.Viewer, int, bool) {
	path0 := cfgPath
	if path0 == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "%s: locate config: %v\n", cmd, err)
			return nil, 1, false
		}
		path0 = p
	}
	cfg, err := loadOrDefault(path0)
	if err != nil {
		fmt.Fprintf(stderr, "%s: read config %s: %v\n", cmd, path0, err)
		return nil, 1, false
	}
	if cfg.Server == "" {
		fmt.Fprintf(stderr, "%s: no server configured: set one with `peerfold client --server`\n", cmd)
		return nil, 1, false
	}

	v, err := viewer.Dial(cfg.Server)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return nil, 1, false
	}
	return v, 0, true
}

// cmdCat writes one file from the aggregated tree to stdout.
//
// The file's bytes go to stdout unchanged and nothing else does, so
// `peerfold cat win/file > copy` produces a byte-for-byte copy. Every message,
// including a failure, goes to stderr.
func cmdCat(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: the per-user config path)")
	fs.Usage = func() {
		fmt.Fprint(stderr, catUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "cat: exactly one PATH is required, got %d\n", fs.NArg())
		return 2
	}

	v, code, ok := connect("cat", *cfgPath, stderr)
	if !ok {
		return code
	}
	defer v.Close()

	data, err := v.Cat(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "cat: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(data); err != nil {
		fmt.Fprintf(stderr, "cat: write to stdout: %v\n", err)
		return 1
	}
	return 0
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

	v, code, ok := connect("ls", *cfgPath, stderr)
	if !ok {
		return code
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

const catUsage = `usage: peerfold cat PATH

Writes one file from the aggregated tree to stdout, unchanged. PATH is
client/share/path, for example win/Documents/notes.txt.

flags:
`
