package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one command line and returns the process exit code, so
// tests can drive the CLI without spawning a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout)
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "server":
		return cmdServer(rest, stdout, stderr)
	case "client":
		return cmdClient(rest, stdout, stderr)
	case "ls", "cat", "put", "mkdir", "rm", "mv":
		return cmdTree(cmd, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

const usageText = `usage: peerfold <command> [args]

commands:
  server  run the aggregation server
  client  join a server and share local directories
  ls      list the aggregated tree
  cat     print a file from the aggregated tree
  put     write stdin to a file in the aggregated tree
  mkdir   create a directory in the aggregated tree
  rm      remove a file or directory in the aggregated tree
  mv      rename within one client's tree
`
