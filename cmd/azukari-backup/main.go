// Command azukari-backup is the backup component of a node: it chunks, deduplicates, encrypts, pushes and restores (ADR 0001).
package main

import (
	"fmt"
	"os"
)

const usage = `usage: azukari-backup <command> [arguments]

no command yet
`

// No subcommand exists yet: every invocation prints the usage and exits 2.
func main() {
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
