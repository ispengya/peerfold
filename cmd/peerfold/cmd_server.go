package main

import (
	"fmt"
	"io"
)

// cmdServer is a stub: it accepts its arguments and reports pending work.
// The real listener arrives in Task 5.
func cmdServer(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "TODO: server %v\n", args)
	return 0
}
