package main

import (
	"fmt"
	"io"
)

// cmdClient is a stub: it accepts its arguments and reports pending work.
// Joining a server arrives in Task 6.
func cmdClient(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "TODO: client %v\n", args)
	return 0
}
