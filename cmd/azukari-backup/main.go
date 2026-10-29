// Command azukari-backup is the backup component of a node: it chunks, deduplicates, encrypts, pushes and restores (ADR 0001).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/scorsi/azukari/internal/backup"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
)

const usage = `usage: azukari-backup check|run <config.toml>

commands:
  check   validate the configuration file and exit (reads no key, SVID nor path)
  run     back up the configured paths into a new snapshot
`

func main() {
	if len(os.Args) != 3 || os.Args[1] != "check" && os.Args[1] != "run" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Schema v1 lines on stderr (ADR 0009).
	log := logging.New(os.Stderr, slog.LevelInfo)
	cfg, err := config.LoadBackup(os.Args[2])
	if os.Args[1] == "check" {
		if err != nil {
			log.Error("", "event", "config-invalid", "err", err)
			os.Exit(1)
		}
		return
	}
	if err != nil {
		log.Error("", "event", "backup-failed", "err", err)
		os.Exit(1)
	}
	if _, err := backup.Run(context.Background(), cfg, log); err != nil {
		os.Exit(1)
	}
}
