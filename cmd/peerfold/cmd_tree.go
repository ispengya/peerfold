package main

import (
	"flag"
	"fmt"
	"io"

	"peerfold/internal/config"
	"peerfold/internal/node"
	"peerfold/internal/viewer"
)

// cmdTree dispatches the tree commands: the reads, the writes, and the two
// commands that name a path rather than move bytes through the terminal.
//
// stdin is the source of a put and is unused by everything else. It is a
// parameter rather than os.Stdin so the command that reads a stream can be
// driven from one in a test.
func cmdTree(cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch cmd {
	case "ls":
		return cmdLs(args, stdout, stderr)
	case "cat":
		return cmdCat(args, stdout, stderr)
	case "put":
		return cmdPut(args, stdin, stderr)
	case "mkdir":
		return cmdMkdir(args, stderr)
	case "rm":
		return cmdRm(args, stderr)
	case "mv":
		return cmdMv(args, stderr)
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
		p, err := config.ServerPath()
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

// cmdPut writes stdin to PATH on the machine that owns it.
//
// The content is stdin and nothing else, so `peerfold put win/file < local`
// uploads a file without the command reading anything the user did not point at
// it. It is not a progress display: a command whose output is a transfer cannot
// also print to stdout without corrupting the transfer.
func cmdPut(args []string, stdin io.Reader, stderr io.Writer) int {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: the per-user config path)")
	exclusive := fs.Bool("exclusive", false, "fail if PATH already exists instead of replacing it")
	fs.Usage = func() {
		fmt.Fprint(stderr, putUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "put: exactly one PATH is required, got %d\n", fs.NArg())
		return 2
	}

	v, code, ok := connect("put", *cfgPath, stderr)
	if !ok {
		return code
	}
	defer v.Close()

	// stdin is read here, not read whole first: a put may be any size, and the
	// viewer streams whatever the reader produces.
	var err error
	if *exclusive {
		err = v.PutExclusive(fs.Arg(0), stdin)
	} else {
		err = v.Put(fs.Arg(0), stdin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "put: %v\n", err)
		return 1
	}
	return 0
}

// cmdMkdir creates a directory on the machine that owns the path.
func cmdMkdir(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mkdir", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: the per-user config path)")
	fs.Usage = func() {
		fmt.Fprint(stderr, mkdirUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "mkdir: exactly one PATH is required, got %d\n", fs.NArg())
		return 2
	}

	v, code, ok := connect("mkdir", *cfgPath, stderr)
	if !ok {
		return code
	}
	defer v.Close()

	if err := v.Mkdir(fs.Arg(0)); err != nil {
		fmt.Fprintf(stderr, "mkdir: %v\n", err)
		return 1
	}
	return 0
}

// cmdRm removes one file or one empty directory.
//
// There is no --recursive flag, and that is the point: the operation deletes
// something on somebody else's machine, and a single wrong path must not be able
// to take a tree with it. A directory with anything in it is refused.
func cmdRm(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: the per-user config path)")
	fs.Usage = func() {
		fmt.Fprint(stderr, rmUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "rm: exactly one PATH is required, got %d\n", fs.NArg())
		return 2
	}

	v, code, ok := connect("rm", *cfgPath, stderr)
	if !ok {
		return code
	}
	defer v.Close()

	if err := v.Remove(fs.Arg(0)); err != nil {
		fmt.Fprintf(stderr, "rm: %v\n", err)
		return 1
	}
	return 0
}

// cmdMv renames OLD to NEW within one client.
func cmdMv(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mv", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: the per-user config path)")
	fs.Usage = func() {
		fmt.Fprint(stderr, mvUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintf(stderr, "mv: exactly two paths are required, got %d\n", fs.NArg())
		return 2
	}

	v, code, ok := connect("mv", *cfgPath, stderr)
	if !ok {
		return code
	}
	defer v.Close()

	if err := v.Rename(fs.Arg(0), fs.Arg(1)); err != nil {
		fmt.Fprintf(stderr, "mv: %v\n", err)
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

const putUsage = `usage: peerfold put [--exclusive] PATH

Writes stdin to PATH on the machine that owns it, replacing whatever is there.
PATH is client/share/path, for example win/Documents/notes.txt.

With --exclusive the write fails if PATH already exists, which is what a caller
that must not clobber a file it did not know about wants. Checking first would
not be the same thing: the check and the write are one step here.

flags:
`

const mkdirUsage = `usage: peerfold mkdir PATH

Creates a directory on the machine that owns it. PATH is client/share/path. The
parents are not created: a mistyped path fails rather than building a tree
nobody named.

flags:
`

const rmUsage = `usage: peerfold rm PATH

Removes one file or one empty directory on the machine that owns it. PATH is
client/share/path.

A non-empty directory is refused. The delete is not recursive, so one wrong path
cannot take a tree with it.

flags:
`

const mvUsage = `usage: peerfold mv OLD NEW

Renames OLD to NEW. Both must name the same client and the same share, since a
rename is one step in one filesystem; it is never a copy. For example:

  peerfold mv win/Documents/draft.txt win/Documents/final.txt

flags:
`
