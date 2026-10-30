// Command azukari-backup is the backup component of a node: it chunks, deduplicates, encrypts, pushes and restores (ADR 0001).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/scorsi/azukari/internal/backup"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
)

const usage = `usage: azukari-backup check <config.toml>
       azukari-backup run <config.toml> <export>
       azukari backup snapshots [--from-dir <bucket copy>] <config.toml> <export>
       azukari backup restore [--verify] [--from-dir <bucket copy>] <config.toml> <export> <snapshot> <target>

commands:
  check      validate the configuration file and exit (reads no key, SVID nor path)
  run        back up the export's paths into a new snapshot of that export
  snapshots  list the export's snapshots, one line each: name, start, end, totals
  restore    restore a snapshot of the export into target, an empty or absent directory;
             --verify reads and checks everything and writes nothing

--from-dir reads a copy of the bucket's directory (a replica) instead of the store: no
network, no SVID, server and [tls] are not read; the namespace is the one whose config
matches the data key.
`

func main() {
	call, verify, fromDir, args, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Schema v1 lines on stderr (ADR 0009).
	log := logging.New(os.Stderr, slog.LevelInfo)
	load := config.LoadBackup
	if fromDir != "" {
		load = config.LoadBackupOffline
	}
	cfg, err := load(args[0])
	failed := map[string]string{"check": "config-invalid", "run": "backup-failed",
		"snapshots": "snapshots-failed", "restore": "restore-failed"}[call]
	if err != nil {
		log.Error("", "event", failed, "err", err)
		os.Exit(1)
	}
	ctx := context.Background()
	switch call {
	case "run":
		_, err = backup.Run(ctx, cfg, args[1], log)
	case "snapshots":
		err = backup.Snapshots(ctx, cfg, fromDir, args[1], os.Stdout, log)
	case "restore":
		err = backup.Restore(ctx, cfg, fromDir, args[1], args[2], args[3], verify, log)
	}
	if err != nil {
		os.Exit(1)
	}
}

// parseArgs splits the command line into the call, its options and the remaining operands
// (rest). It reslices args and never writes to it: append on args[:1] would overwrite the
// option values while they are still being read.
func parseArgs(args []string) (call string, verify bool, fromDir string, rest []string, err error) {
	errUsage := errors.New("usage")
	if len(args) == 0 {
		return "", false, "", nil, errUsage
	}
	call, rest = args[0], args[1:]
	for len(rest) > 0 && (call == "restore" || call == "snapshots") {
		if rest[0] == "--verify" && call == "restore" && !verify {
			verify, rest = true, rest[1:]
		} else if rest[0] == "--from-dir" && len(rest) > 1 && fromDir == "" && rest[1] != "" {
			fromDir, rest = rest[1], rest[2:]
		} else {
			break
		}
	}
	want := map[string]int{"check": 1, "run": 2, "snapshots": 2, "restore": 4}[call]
	// A leading "--" is an unknown, repeated or incomplete option.
	if want == 0 || len(rest) != want || strings.HasPrefix(rest[0], "--") {
		return "", false, "", nil, errUsage
	}
	return call, verify, fromDir, rest, nil
}
