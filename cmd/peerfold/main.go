package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches one command line and returns the process exit code, so
// tests can drive the CLI without spawning a process.
//
// stdin is passed in rather than read from the process, for the same reason
// stdout and stderr are: a command whose input is a stream can only be tested
// from a stream the test supplies.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
		return cmdTree(cmd, rest, stdin, stdout, stderr)
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
