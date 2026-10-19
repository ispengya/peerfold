package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/mount"
	"github.com/ispengya/peerfold/internal/viewer"
)

// loadMountConfig reads the mount role's config. When the default path has no
// file yet, it seeds the values from the server config, which is where earlier
// versions kept them; the interactive mount's save then moves them into the
// mount's own directory. An explicit --config never falls back.
//
// migrated reports that the values came from the old location, so the
// interactive flow can say where they were read from.
func loadMountConfig(path string, explicit bool) (cfg config.Config, migrated bool, err error) {
	cfg, err = config.Load(path)
	if err == nil || explicit || !errors.Is(err, os.ErrNotExist) {
		return cfg, false, err
	}
	oldPath, perr := config.ServerPath()
	if perr != nil {
		return config.Config{}, false, err
	}
	old, oerr := config.Load(oldPath)
	if oerr != nil {
		// The mount file is the one the caller asked about; report its absence.
		return config.Config{}, false, err
	}
	return config.Config{
		Server:      old.Server,
		MountPoint:  old.MountPoint,
		BufferLimit: old.BufferLimit,
		BufferTotal: old.BufferTotal,
	}, true, nil
}

// mountState is what a running mount records for the service commands. The
// pid file alone would let status say "running" but not which server the mount
// dialled or where it is mounted, and those flags need not match the config.
type mountState struct {
	Server     string `json:"server"`
	MountPoint string `json:"mount_point"`
}

// mountStatePath is where a running mount records its command line. It lives
// beside the mount config so everything the mount role owns sits in one
// directory, and status finds it without knowing the flags it was started with.
func mountStatePath() (string, error) {
	cfgPath, err := config.MountPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "state.json"), nil
}

// writeMountState records the server and mount point of a running mount.
func writeMountState(server, mountPoint string) error {
	path, err := mountStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(mountState{Server: server, MountPoint: mountPoint}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// readMountState reads what a mount last recorded. The caller still has to
// check the pid: the file outlives a mount that was killed outright.
func readMountState() (mountState, error) {
	path, err := mountStatePath()
	if err != nil {
		return mountState{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return mountState{}, err
	}
	var s mountState
	if err := json.Unmarshal(b, &s); err != nil {
		return mountState{}, err
	}
	return s, nil
}

// clearMountState removes the state file. Errors are ignored for the same
// reason clearPID ignores them: the process is exiting and there is no one
// left to tell.
func clearMountState() {
	if path, err := mountStatePath(); err == nil {
		_ = os.Remove(path)
	}
}

// cmdMount is the entry point for `peerfold mount`.
func cmdMount(args []string, stdout, stderr io.Writer) int {
	return runMount(args, stdout, stderr)
}

// runMount parses the command line, dials the server and runs the mount until
// the filesystem goes away.
//
// The context is context.Background because this command does not stop on a
// context: SIGINT is handled inside mount.Runner, which flushes before
// unmounting. A second signal path here would be a second place to get that
// order wrong.
func runMount(args []string, stdout, stderr io.Writer) int {
	opts, server, mountPoint, _, err := mountOptionsFrom(args)
	if errors.Is(err, flag.ErrHelp) {
		// --help is a request, not a mistake, and it has to print the flags
		// rather than the phrase "flag: help requested".
		fmt.Fprint(stderr, mountUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "mount: %v\n", err)
		return 2
	}

	// Say where it will mount BEFORE doing anything that can fail. The mount
	// point has a default, and a default the user cannot see is a directory they
	// will go looking for afterwards; printing it first means the line is there
	// even when the server turns out to be down.
	abs, err := filepath.Abs(mountPoint)
	if err != nil {
		fmt.Fprintf(stderr, "mount: resolve mount point %s: %v\n", mountPoint, err)
		return 1
	}
	mountPoint = abs
	fmt.Fprintf(stdout, "mounting on %s\n", mountPoint)

	// server comes from the config file when there is no --server, and
	// mountOptionsFrom resolves that: it has the config open already.
	if server == "" {
		fmt.Fprintln(stderr, "mount: no server to dial: pass --server or set one in the config")
		return 2
	}

	v, err := viewer.Dial(server)
	if err != nil {
		fmt.Fprintf(stderr, "mount: %v\n", err)
		return 1
	}
	defer v.Close()

	// The mount runs in the foreground with no other handle on it, so it
	// records its pid and what it dialled before going into the FUSE loop.
	// `peerfold status` reports that, and `peerfold stop mount` reaches the
	// process through the pid file. Neither is needed to mount, so a failure
	// warns rather than aborting. One mount per user: a second one overwrites
	// these, just as the config already assumes a single mount point.
	defer func() {
		clearMountState()
		_ = clearPID("mount")
	}()
	if err := writePID("mount", os.Getpid()); err != nil {
		fmt.Fprintf(stderr, "mount: could not record the pid: %v\n", err)
	}
	if err := writeMountState(server, mountPoint); err != nil {
		fmt.Fprintf(stderr, "mount: could not record its state: %v\n", err)
	}

	// The reconnector dials again after the connection dies. A failed dial
	// returns an error and never a nil viewer with a nil error: the mount would
	// install that nil caller and every later operation would panic somewhere far
	// from the cause.
	reconnect := func() (mount.Caller, error) {
		fresh, err := viewer.Dial(server)
		if err != nil {
			return nil, err
		}
		return mount.NewViewerCaller(fresh), nil
	}

	r := &mount.Runner{
		Mount:     mountPoint,
		Caller:    mount.NewViewerCaller(v),
		Opts:      opts,
		Reconnect: reconnect,
	}

	fmt.Fprintf(stdout, "mounted; press Ctrl-C to unmount\n")
	if err := r.Run(context.Background()); err != nil {
		fmt.Fprintf(stderr, "mount: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "unmounted")
	return 0
}

// mountOptionsFrom parses the mount command line into options and a mount point.
//
// The flags are read twice -- once into an Options and once to find the ones a
// config file can also supply -- which is why this takes the argument list rather
// than a parsed FlagSet: a caller that has already parsed wants mountOptionsFrom
// to see the same words.
func mountOptionsFrom(args []string) (mount.Options, string, string, string, error) {
	fs := flag.NewFlagSet("mount", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	var (
		server    = fs.String("server", "", "server address to dial (host:port)")
		mountPath = fs.String("mount", "", "where to mount (default: the per-user mount point)")
		buffer    = fs.String("buffer", "", "read cache per open file (default: 64MiB)")
		bufferTot = fs.String("buffer-total", "", "read cache across all open files (default: 256MiB)")
		cfgPath   = fs.String("config", "", "config file (default: the per-user config path)")
	)
	if err := fs.Parse(args); err != nil {
		return mount.Options{}, "", "", "", err
	}
	if fs.NArg() > 0 {
		return mount.Options{}, "", "", "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// Flags beat the config, and the config beats the package defaults. The
	// config is read here because it can supply the mount point and both buffer
	// limits, so the three have to be resolved in one place.
	path := *cfgPath
	explicit := path != ""
	if !explicit {
		p, err := config.MountPath()
		if err != nil {
			return mount.Options{}, "", "", "", fmt.Errorf("locate config: %w", err)
		}
		path = p
	}
	cfg, _, err := loadMountConfig(path, explicit)
	if err != nil {
		return mount.Options{}, "", "", "", fmt.Errorf("read config %s: %w", path, err)
	}

	opts := mount.DefaultOptions()
	srv := *server
	if srv == "" {
		srv = cfg.Server
	}
	if cfg.BufferLimit > 0 {
		opts.BufferLimit = cfg.BufferLimit
	}
	if cfg.BufferTotal > 0 {
		opts.BufferTotal = cfg.BufferTotal
	}
	if *buffer != "" {
		n, err := parseSize(*buffer)
		if err != nil {
			return mount.Options{}, "", "", "", fmt.Errorf("--buffer: %w", err)
		}
		opts.BufferLimit = n
	}
	if *bufferTot != "" {
		n, err := parseSize(*bufferTot)
		if err != nil {
			return mount.Options{}, "", "", "", fmt.Errorf("--buffer-total: %w", err)
		}
		opts.BufferTotal = n
	}

	// A limit larger than the total it is part of is a contradiction. The cache
	// would run at whatever the smaller number allows, so the per-file limit
	// would be quietly halved or worse -- and a user who set 512MiB wants to know
	// it is not in force rather than to find out from a performance problem.
	if opts.BufferTotal > 0 && opts.BufferLimit > opts.BufferTotal {
		return mount.Options{}, "", "", "", fmt.Errorf(
			"--buffer %d is larger than --buffer-total %d, so the per-file limit "+
				"could never be reached; raise --buffer-total or lower --buffer",
			opts.BufferLimit, opts.BufferTotal)
	}

	point := *mountPath
	if point == "" {
		point = cfg.MountPoint
	}
	if point == "" {
		p, err := mount.DefaultMountPoint()
		if err != nil {
			return mount.Options{}, "", "", "", fmt.Errorf("determine the default mount point: %w", err)
		}
		point = p
	}
	// A relative mount point resolves against the process's working directory, so
	// the same command would mount somewhere different depending on where it was
	// run. The default is absolute; an explicit one has to be too.
	if !filepath.IsAbs(point) {
		return mount.Options{}, "", "", "", fmt.Errorf(
			"--mount %q is not an absolute path; a relative mount point depends on "+
				"the working directory", point)
	}

	return opts, srv, point, path, nil
}

// parseSize reads a byte size: a plain number is bytes, and a suffix scales it.
//
// Both the binary suffixes (KiB, MiB, GiB, TiB) and the short ones (K, M, G, T)
// are accepted, and the short ones are the binary ones -- they mean 1024, not
// 1000. That is the convention the buffer arguments have always used, and a
// limit that is 2.4% lower than the user asked for is the kind of difference
// nobody notices until a file that used to fit does not.
//
// "B" is accepted as an explicit bytes suffix so that "64B" and "64" agree.
//
// A fractional size is refused. The result is an int, and ParseInt on the scaled
// value would silently truncate "1.5MiB" to 1MiB.
func parseSize(s string) (int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, errors.New("empty size")
	}

	upper := strings.ToUpper(t)
	mult := 1
	for _, suffix := range []struct {
		text string
		mult int
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
		{"B", 1},
	} {
		if v, ok := strings.CutSuffix(upper, suffix.text); ok {
			upper, mult = v, suffix.mult
			break
		}
	}

	upper = strings.TrimSpace(upper)
	if upper == "" {
		return 0, fmt.Errorf("size %q has no number", s)
	}
	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q is not a plain number followed by an optional unit", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q is negative", s)
	}

	// The multiply is done in 64 bits and then range-checked, because on a 32-bit
	// platform an int is 32 bits and 2GiB does not fit.
	scaled := n * int64(mult)
	if scaled > int64(maxInt) {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return int(scaled), nil
}

// maxInt is the largest value an int can hold on this platform.
const maxInt = int(^uint(0) >> 1)

const mountUsage = `usage: peerfold mount [flags]

Mounts the federated tree as a directory, so files can be opened, edited and
saved with ordinary programs. Reading is what the mount is for by default;
writes go back to the client that owns the file.

The mount point defaults to a directory under your home. With no --server the
address in the config file is used, and --mount, --buffer and --buffer-total all
fall back to the config before the built-in default.

flags:
  --server ADDR         server to dial (config: server)
  --mount PATH          where to mount (config: mount_point; default: ~/peerfold)
  --buffer SIZE         read cache per open file (config: buffer_limit; default: 64MiB)
  --buffer-total SIZE   read cache across all open files (config: buffer_total; default: 256MiB)
  --config PATH         config file (default: the per-user config path)
`
