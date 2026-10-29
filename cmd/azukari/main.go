// Command azukari is the operator command line; administration goes through the store's local socket (ADR 0001).
package main

import (
	"fmt"
	"os"
)

const usage = `usage: azukari <command> [arguments]

no command yet
`

// No subcommand exists yet: every invocation prints the usage and exits 2.
func main() {
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
}
