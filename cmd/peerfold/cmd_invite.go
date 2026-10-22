package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/ispengya/peerfold/internal/config"
	"github.com/ispengya/peerfold/internal/identity"
)

// cmdInvite creates a credential for another machine and prints it as one
// pasteable string. It does not need the server to be running: the credential
// lives in the server's token file, which the running server reads on each
// login, and the certificate is the one the server will present.
func cmdInvite(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		name    = fs.String("name", "", "principal name for the new machine")
		role    = fs.String("role", identity.RoleClient, "client or viewer")
		write   = fs.Bool("write", false, "allow this viewer to write (client tokens always can)")
		server  = fs.String("server", "", "address the invite should dial (host:port)")
		cfgPath = fs.String("config", "", "server config file (default: the per-user config path)")
	)
	fs.Usage = func() {
		fmt.Fprint(stderr, inviteUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 2
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "invite: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "invite: --name is required")
		return 2
	}
	if *role != identity.RoleClient && *role != identity.RoleViewer {
		fmt.Fprintf(stderr, "invite: --role must be client or viewer, got %q\n", *role)
		return 2
	}

	path := *cfgPath
	if path == "" {
		p, err := config.ServerPath()
		if err != nil {
			fmt.Fprintf(stderr, "invite: locate config: %v\n", err)
			return 1
		}
		path = p
	}
	cfg, err := loadOrDefault(path)
	if err != nil {
		fmt.Fprintf(stderr, "invite: read config %s: %v\n", path, err)
		return 1
	}

	addr := *server
	if addr == "" {
		addr = cfg.Server
	}
	if addr == "" && cfg.Listen != "" {
		addr = dialAddress(cfg.Listen)
	}
	if addr == "" {
		fmt.Fprintln(stderr, "invite: no server address: pass --server host:port")
		return 2
	}

	dir := filepath.Dir(path)
	id, err := identity.LoadOrCreateServer(dir)
	if err != nil {
		fmt.Fprintf(stderr, "invite: %v\n", err)
		return 1
	}
	token, err := identity.AddToken(filepath.Join(dir, "tokens.json"), *name, *role, *write)
	if err != nil {
		fmt.Fprintf(stderr, "invite: %v\n", err)
		return 1
	}

	inv := identity.Invite{
		Address:     addr,
		Fingerprint: id.Fingerprint(),
		Name:        *name,
		Role:        *role,
		Token:       token,
		Write:       *write,
	}
	fmt.Fprintln(stdout, inv.String())
	fmt.Fprintln(stderr, "invite: treat this string as a credential; re-running the command for the same name rotates it")
	return 0
}

const inviteUsage = `usage: peerfold invite [flags]

Creates a credential for one machine and prints an invite string. Paste that
string into the client or mount setup on the other machine; it carries the
address, the server certificate's fingerprint, the principal name and the
token. Creating a new invite for the same name rotates its token.

flags:
`
