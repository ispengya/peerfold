package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"

	"peerfold/internal/client"
	"peerfold/internal/config"
	"peerfold/internal/logging"
)

// cmdClient joins a server and shares local directories.
//
// The configuration is written before the connection is attempted: "peerfold
// client --share ... " is meant to feel like one command that joins, so a
// server that happens to be down must not also discard what the user just
// declared. Re-running the command later reconnects with the same settings.
func cmdClient(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		server    = fs.String("server", "", "server address to dial (host:port)")
		shares    = shareList{}
		name      = fs.String("name", "", "client name (default: this host's name)")
		aggregate = fs.Bool("aggregate", false, "ask to be the aggregating client")
		cfgPath   = fs.String("config", "", "config file (default: the per-user config path)")
	)
	fs.Var(&shares, "share", "share a directory as NAME=PATH (repeatable)")
	fs.Usage = func() {
		fmt.Fprint(stderr, clientUsage)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "client: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	path := *cfgPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "client: locate config: %v\n", err)
			return 1
		}
		path = p
	}

	cfg, err := loadOrDefault(path)
	if err != nil {
		fmt.Fprintf(stderr, "client: read config %s: %v\n", path, err)
		return 1
	}

	// A flag that was not given must not erase what the config already holds.
	if *server != "" {
		cfg.Server = *server
	}
	if *name != "" {
		cfg.ClientName = *name
	}
	if *aggregate {
		cfg.Aggregate = true
	}
	if cfg.Shares == nil {
		cfg.Shares = map[string]string{}
	}
	for _, s := range shares {
		cfg.Shares[s.Name] = s.Path
	}

	clientName := cfg.ClientName
	if clientName == "" {
		host, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(stderr, "client: determine host name: %v\n", err)
			return 1
		}
		clientName = host
		cfg.ClientName = clientName
	}

	// The always-on diagnostic log, one file per client so two clients on one
	// machine do not write over each other. Like the server's, a failure to open
	// it is not fatal.
	if f, err := logging.Open("client-" + clientName + ".log"); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}

	if cfg.Server == "" {
		fmt.Fprintln(stderr, "client: no server to dial: pass --server or set one in the config")
		return 2
	}
	if len(cfg.Shares) == 0 {
		fmt.Fprintln(stderr, "client: no directories to share: pass --share NAME=PATH")
		return 2
	}

	// Resolve the shares before saving anything. A share that does not exist is a
	// mistake in the command that was just typed, and writing it to the config would
	// make it permanent: the merge below is add-only, so every later run would fail
	// on an entry the user cannot see how to remove.
	sh, err := client.NewShares(shareArgs(cfg.Shares), nil)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	if err := config.Save(path, cfg); err != nil {
		fmt.Fprintf(stderr, "client: save config %s: %v\n", path, err)
		return 1
	}

	c, err := client.Dial(client.Options{
		Server:    cfg.Server,
		Name:      clientName,
		Shares:    sh,
		Aggregate: cfg.Aggregate,
		OnBatch: func(sent int) {
			fmt.Fprintf(stdout, "shared %d entries\n", sent)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer c.Close()

	if err := c.UploadTree(); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	// The upload is a snapshot: it says what the shares hold at this instant.
	// Without a watcher the tree then never changes again, which is the whole
	// feature — a client that does not watch is a client that lied about what is
	// on its disk the moment anything moved. So this is started here, after the
	// snapshot, for the same reason the scanner runs before the watcher: the
	// events from here on describe what changed *since* the scan.
	//
	// A watcher that fails to start is reported and stepped over rather than
	// fatal. The snapshot is still indexed and still readable; what is lost is
	// liveness, and a client that connects read-only is more useful than one that
	// refuses to connect. Closing it before the client keeps the order the other
	// way round: the watcher writes on this connection, so it must stop first.
	watcher, err := c.Watch()
	if err != nil {
		fmt.Fprintf(stderr, "client: watching for changes: %v\n", err)
		fmt.Fprintln(stderr, "client: the tree is indexed but will not follow changes on disk")
	} else {
		defer watcher.Close()
	}

	role := "client"
	if c.Aggregate() {
		role = "aggregating client"
	}
	fmt.Fprintf(stdout, "joined %s as %s (%s)\n", cfg.Server, c.Name(), role)

	// Asking to aggregate and being told no is a ruling, not a failure — v1
	// allows exactly one aggregator and the first one wins. But the line above
	// says "client" either way, so a user who passed --aggregate would be left
	// believing the flag was honored. Say it out loud instead: the flag was
	// seen, and this is why it had no effect.
	if cfg.Aggregate && !c.Aggregate() {
		fmt.Fprintln(stderr, "client: --aggregate was not granted: another client is already aggregating; "+
			"running as a plain supplier")
	}

	// Stay connected: the index belongs to this connection, so exiting would
	// withdraw the tree that was just published. The connection is what keeps
	// the share alive, so the process now waits for an interrupt — or for the
	// connection to end on its own, which is a failure and has to be reported.
	// Waiting only for the interrupt would leave a process that looks healthy
	// while the server has already withdrawn everything it published.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	select {
	case <-sig:
		fmt.Fprintln(stdout, "disconnected")
		return 0
	case <-c.Done():
		if err := c.Err(); err != nil {
			fmt.Fprintf(stderr, "client: the connection ended: %v\n", err)
		} else {
			fmt.Fprintln(stderr, "client: the connection ended")
		}
		return 1
	}
}

// shareArg is one --share NAME=PATH.
type shareArg struct {
	Name string
	Path string
}

// shareList collects repeated --share flags. flag.Value would let a caller pass
// a comma-separated list, but a path may contain a comma, so NAME=PATH is the
// only accepted form.
type shareList []shareArg

func (s *shareList) String() string {
	parts := make([]string, 0, len(*s))
	for _, e := range *s {
		parts = append(parts, e.Name+"="+e.Path)
	}
	return strings.Join(parts, ",")
}

func (s *shareList) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want NAME=PATH, got %q", v)
	}
	if name == "" || path == "" {
		return fmt.Errorf("want NAME=PATH with both parts set, got %q", v)
	}
	*s = append(*s, shareArg{Name: name, Path: path})
	return nil
}

// shareArgs renders a config's shares as the NAME=PATH form NewShares takes,
// in a stable order so the error a caller sees does not depend on map
// iteration order.
func shareArgs(shares map[string]string) []string {
	names := make([]string, 0, len(shares))
	for name := range shares {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name+"="+shares[name])
	}
	return out
}

// loadOrDefault reads the config, treating a missing file as the defaults
// rather than an error: the first run of a fresh install has no file yet.
func loadOrDefault(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return config.Default(), nil
	}
	return config.Config{}, err
}

const clientUsage = `usage: peerfold client [flags]

Joins a server and shares local directories with it. The flags are merged into
the config file, which is written before the connection is attempted, so a
server that is down does not discard the shares you just declared.

flags:
`
