package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// commands are the words this program accepts, in the order usageText lists
// them. dispatch uses a switch rather than ranging over this, so the two are
// separate lists; dispatcherKnows reads this one and the usage text, which is
// what lets a test catch a command that is documented and not implemented.
var commands = []string{"server", "client", "mount", "ls", "cat", "put", "mkdir", "rm", "mv"}

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
	return dispatch(cmd, rest, stdin, stdout, stderr)
}

// dispatch runs one command and returns the process exit code.
func dispatch(cmd string, rest []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch cmd {
	case "server":
		return cmdServer(rest, stdout, stderr)
	case "client":
		return cmdClient(rest, stdout, stderr)
	case "mount":
		return cmdMount(rest, stdout, stderr)
	case "ls", "cat", "put", "mkdir", "rm", "mv":
		return cmdTree(cmd, rest, stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}
}

// dispatcherKnows reports whether cmd is one of the words this program accepts.
func dispatcherKnows(cmd string) bool {
	for _, c := range commands {
		if c == cmd {
			return true
		}
	}
	return false
}

func usage(w io.Writer) {
	fmt.Fprint(w, usageText)
}

const usageText = `usage: peerfold <command> [args]

commands:
  server  run the aggregation server
  client  join a server and share local directories
  mount   mount the federated tree as a directory
  ls      list the aggregated tree
  cat     print a file from the aggregated tree
  put     write stdin to a file in the aggregated tree
  mkdir   create a directory in the aggregated tree
  rm      remove a file or directory in the aggregated tree
  mv      rename within one client's tree
`
