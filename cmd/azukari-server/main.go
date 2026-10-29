// Command azukari-server is the store: it keeps what nodes push, prunes and replicates it (ADR 0001).
package main

import (
	"fmt"
	"os"
)

const usage = `usage: azukari-server <command> [arguments]

no command yet
`

// No subcommand exists yet: every invocation prints the usage and exits 2.
func main() {
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
