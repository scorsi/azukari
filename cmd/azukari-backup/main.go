// Command azukari-backup is the backup component of a node: it chunks, deduplicates, encrypts, pushes and restores (ADR 0001).
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
)

const usage = `usage: azukari-backup check <config.toml>

commands:
  check   validate the configuration file and exit (reads no key, SVID nor path)
`

func main() {
	if len(os.Args) != 3 || os.Args[1] != "check" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Schema v1 lines on stderr (ADR 0009).
	log := logging.New(os.Stderr, slog.LevelInfo)
	if _, err := config.LoadBackup(os.Args[2]); err != nil {
		log.Error("", "event", "config-invalid", "err", err)
		os.Exit(1)
	}
}
