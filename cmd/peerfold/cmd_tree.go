package main

import (
	"fmt"
	"io"
)

// cmdTree is a stub: it accepts its arguments and reports pending work.
// Once implemented these subcommands report to stdout, so the pending
// notice goes to stderr to keep stdout a clean data channel.
func cmdTree(cmd string, args []string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "TODO: %s %v\n", cmd, args)
	return 0
}
