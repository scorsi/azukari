// Command azukari-backup is the backup component of a node: it chunks, deduplicates, encrypts, pushes and restores (ADR 0001).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/scorsi/azukari/internal/backup"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
)

const usage = `usage: azukari-backup check|run <config.toml>
       azukari backup snapshots [--from-dir <bucket copy>] <config.toml>
       azukari backup restore [--verify] [--from-dir <bucket copy>] <config.toml> <snapshot> <target>

commands:
  check      validate the configuration file and exit (reads no key, SVID nor path)
  run        back up the configured paths into a new snapshot
  snapshots  list the node's snapshots, one line each: name, start, end, totals
  restore    restore a snapshot into target, an empty or absent directory;
             --verify reads and checks everything and writes nothing

--from-dir reads a copy of the bucket's directory (a replica) instead of the store: no
network, no SVID, server and [tls] are not read; the namespace is the one whose config
matches the data key.
`

func main() {
	args := os.Args[1:]
	var verify bool
	var fromDir string
	for len(args) > 1 && (args[0] == "restore" || args[0] == "snapshots") {
		if args[1] == "--verify" && args[0] == "restore" && !verify {
			verify, args = true, append(args[:1], args[2:]...)
		} else if args[1] == "--from-dir" && len(args) > 2 && fromDir == "" && args[2] != "" {
			fromDir, args = args[2], append(args[:1], args[3:]...)
		} else {
			break
		}
	}
	if len(args) > 1 && strings.HasPrefix(args[1], "--") {
		args = nil // an unknown, repeated or incomplete option
	}
	want := map[string]int{"check": 2, "run": 2, "snapshots": 2, "restore": 4}
	if len(args) == 0 || want[args[0]] == 0 || len(args) != want[args[0]] {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Schema v1 lines on stderr (ADR 0009).
	log := logging.New(os.Stderr, slog.LevelInfo)
	load := config.LoadBackup
	if fromDir != "" {
		load = config.LoadBackupOffline
	}
	cfg, err := load(args[1])
	failed := map[string]string{"check": "config-invalid", "run": "backup-failed",
		"snapshots": "snapshots-failed", "restore": "restore-failed"}[args[0]]
	if err != nil {
		log.Error("", "event", failed, "err", err)
		os.Exit(1)
	}
	ctx := context.Background()
	switch args[0] {
	case "run":
		_, err = backup.Run(ctx, cfg, log)
	case "snapshots":
		err = backup.Snapshots(ctx, cfg, fromDir, os.Stdout, log)
	case "restore":
		err = backup.Restore(ctx, cfg, fromDir, args[2], args[3], verify, log)
	}
	if err != nil {
		os.Exit(1)
	}
}
