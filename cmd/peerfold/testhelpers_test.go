package main

import (
	"io"
	"strings"
)

// runWithEmptyStdin drives the CLI with no standard input.
//
// It exists because almost every command ignores stdin, and passing an empty
// reader at each call site would bury what the test is about. The tests that do
// care about stdin call run directly.
func runWithEmptyStdin(args []string, stdout, stderr io.Writer) int {
	return run(args, strings.NewReader(""), stdout, stderr)
}
