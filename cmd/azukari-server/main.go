// Command azukari-server is the store: it keeps what nodes push, prunes and replicates it (ADR 0001).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/server"
	"github.com/scorsi/azukari/internal/svid"
)

// version is set by the Nix build (-X main.version=…): azukari_build_info.
var version = "dev"

const usage = `usage: azukari-server check|run <config.toml>

commands:
  check   validate the configuration file and exit (reads no SVID, mount nor bucket)
  run     serve the buckets until SIGTERM or SIGINT; renewed SVID files are picked up, SIGHUP forces it;
          the administration socket is $RUNTIME_DIRECTORY/admin.sock, none when the variable is unset
`

func main() {
	if len(os.Args) != 3 || os.Args[1] != "check" && os.Args[1] != "run" {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Schema v1 lines on stderr (ADR 0009).
	log := logging.New(os.Stderr, slog.LevelInfo)
	cfg, err := config.LoadServer(os.Args[2])
	if err != nil {
		log.Error("", "event", "config-invalid", "err", err.Error())
		os.Exit(1)
	}
	if os.Args[1] == "run" {
		if err := run(cfg, log); err != nil {
			log.Error("", "event", "server-failed", "err", err.Error())
			os.Exit(1)
		}
	}
}

func run(cfg *config.Server, log *slog.Logger) error {
	src, err := svid.Load(cfg.TLS)
	if err != nil {
		return err
	}
	srv, err := server.New(cfg, src, log, bucket.IsMountPoint)
	if err != nil {
		return err
	}
	// The loopback /metrics listener, plain HTTP: the only unauthenticated port (ADR 0005, 0009).
	mln, err := net.Listen("tcp", cfg.ListenMetrics)
	if err != nil {
		return err
	}
	msrv := &http.Server{Handler: srv.MetricsHandler(version), ReadHeaderTimeout: server.HeaderTimeout}
	go msrv.Serve(mln)
	defer msrv.Close()

	var lns []net.Listener
	for _, addr := range cfg.Listen {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		lns = append(lns, ln)
	}

	// Renewed files are picked up before each handshake (ADR 0016); SIGHUP forces a load.
	src.OnReload = func(err error) {
		if err != nil {
			log.Error("", "event", "svid-reload-failed", "err", err.Error())
		} else {
			log.Info("", "event", "svid-reloaded", "not_after", src.NotAfter().UTC().Format(time.RFC3339))
		}
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			src.Reload() // reported through OnReload
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv.Schedule(ctx) // daily retention runs (ADR 0007)

	// The local administration socket (ADR 0005, 0024), in the unit's RuntimeDirectory.
	if dir := os.Getenv("RUNTIME_DIRECTORY"); dir != "" {
		aln, err := server.ListenAdmin(filepath.Join(dir, server.AdminSocket))
		if err != nil {
			return err
		}
		go func() {
			if err := srv.ServeAdmin(ctx, aln); err != nil {
				log.Error("", "event", "server-failed", "err", err.Error())
				os.Exit(1)
			}
		}()
	}
	return srv.Serve(ctx, lns)
}
