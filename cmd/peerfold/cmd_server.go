package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"

	"peerfold/internal/config"
	"peerfold/internal/logging"
	"peerfold/internal/server"
)

// cmdServer runs the aggregation server until interrupted.
//
// The listen address comes from the config file, with --listen overriding it for
// one run rather than being written back: an address is a property of where this
// process was started, and a machine that binds a fixed port in production is
// usually started with the same --listen every time. The config's --server field
// is the other end of this one — it is what clients dial — so a server started
// with a --listen that does not match the clients' --server is a configuration
// mistake, not something this command can detect.
//
// Unlike `peerfold client`, nothing is written to the config here: a client's
// run declares shares that the next run should still have, while a server has
// nothing to accumulate.
//
// It stops when the process is interrupted. A test cannot interrupt the process
// it is running in, so it drives cmdServerWith instead.
func cmdServer(args []string, stdout, stderr io.Writer) int {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	interrupt := make(chan struct{}, 1)
	go func() {
		<-sig
		close(interrupt)
	}()

	return cmdServerWith(args, stdout, stderr, interrupt)
}

// cmdServerWith is cmdServer with its stop condition supplied by the caller:
// interrupt closes when the server should stop. cmdServer is the one that wires
// it to the process's interrupt signal, and a test can drive this directly.
func cmdServerWith(args []string, stdout, stderr io.Writer, interrupt <-chan struct{}) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		listen  = fs.String("listen", "", "address to listen on (default: the config's listen, else :7000)")
		cfgPath = fs.String("config", "", "config file (default: the per-user config path)")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, serverUsage)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "server: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	path := *cfgPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintf(stderr, "server: locate config: %v\n", err)
			return 1
		}
		path = p
	}

	cfg, err := loadOrDefault(path)
	if err != nil {
		fmt.Fprintf(stderr, "server: read config %s: %v\n", path, err)
		return 1
	}

	addr := cfg.Listen
	if addr == "" {
		addr = config.Default().Listen
	}
	if *listen != "" {
		addr = *listen
	}

	// The always-on diagnostic log: the server's activity goes to a fixed file
	// under the user's home, so a run can be reconstructed without a flag. A
	// failure to open it is not fatal -- the log is a convenience, not a
	// dependency -- so the server runs on without file logging.
	if f, err := logging.Open("server.log"); err == nil {
		log.SetOutput(f)
		defer f.Close()
	}

	s := server.New(addr)
	// New binds the listener, so a port already in use is reported here instead
	// of after the process has announced that it is running. The operating
	// system's own message is printed: "address already in use" tells a user
	// what to do, and "address is unavailable" does not.
	if s.Addr() == nil {
		fmt.Fprintf(stderr, "server: listen on %s: %v\n", addr, s.Err())
		return 1
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve() }()

	fmt.Fprintf(stdout, "listening on %s\n", s.Addr())

	select {
	case <-interrupt:
		// Closing the listener is what unblocks Serve, so the order matters:
		// Close first, then let Serve report that it stopped.
		_ = s.Close()
		if err := <-serveErr; err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(stderr, "server: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "stopped")
		return 0
	case err := <-serveErr:
		// Serve returned on its own, which for this command means it failed:
		// nothing else asks it to stop.
		if err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(stderr, "server: %v\n", err)
			return 1
		}
		return 0
	}
}

const serverUsage = `usage: peerfold server [flags]

Runs the aggregation server: every client dials out to it, and it holds the one
global index of what they share. It listens until interrupted.

flags:
`
