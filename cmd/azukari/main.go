// Command azukari is the operator command line; administration goes through the store's local
// socket only (ADR 0001, 0005), in ADR 0024's protocol.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	adminv1 "github.com/scorsi/azukari/gen/azukari/admin/v1"
	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
	"github.com/scorsi/azukari/internal/logging"
)

const usage = `usage: azukari [--socket <path>] <command> [arguments]

commands:
  retention <bucket> [--dry-run]                 run the bucket's retention now, wait for its end
  snapshots <bucket> <namespace>                 list a namespace's snapshots, oldest first
  delete-snapshot <bucket> <namespace> <name>    delete one snapshot; its chunks go at the next run
  backup snapshots [--from-dir <bucket copy>] <config.toml>
                                                 list this node's snapshots
  backup restore [--verify] [--from-dir <bucket copy>] <config.toml> <snapshot> <target>
                                                 restore a snapshot into target, an empty or absent directory

--socket defaults to /run/azukari/admin.sock. backup runs azukari-backup, installed next to
azukari, with the same arguments, environment and privileges (ADR 0028).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0, 1 for a refused or failed call, 2 for a usage error.
func run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) > 0 && argv[0] == "backup" {
		return delegate(argv[1:], stderr)
	}
	socket, dryRun, badFlag := "/run/azukari/admin.sock", false, false
	var args []string
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--dry-run":
			dryRun = true
		case a == "--socket" && i+1 < len(argv):
			i++
			socket = argv[i]
		case strings.HasPrefix(a, "--socket="):
			socket = strings.TrimPrefix(a, "--socket=")
		case strings.HasPrefix(a, "-"):
			badFlag = true
		default:
			args = append(args, a)
		}
	}
	var req, resp proto.Message
	switch {
	case badFlag:
	case len(args) == 2 && args[0] == "retention":
		req, resp = &adminv1.RetentionRequest{DryRun: dryRun}, &adminv1.RetentionResponse{}
	case len(args) == 3 && args[0] == "snapshots" && !dryRun:
		req, resp = &adminv1.SnapshotsRequest{Namespace: args[2]}, &adminv1.SnapshotsResponse{}
	case len(args) == 4 && args[0] == "delete-snapshot" && !dryRun:
		req, resp = &adminv1.DeleteSnapshotRequest{Namespace: args[2], Name: args[3]}, &adminv1.DeleteSnapshotResponse{}
	}
	if req == nil {
		fmt.Fprint(stderr, usage)
		return 2
	}
	// Schema v1 lines on stderr (ADR 0009).
	log := logging.New(stderr, slog.LevelInfo)
	if reason, err := call(socket, args[0], args[1], req, resp); err != nil {
		attrs := []any{"event", "admin-failed", "call", args[0], "err", err.Error()}
		if reason != "" {
			attrs = append(attrs, "reason", reason)
		}
		log.Error("", attrs...)
		return 1
	}
	switch r := resp.(type) {
	case *adminv1.RetentionResponse:
		fmt.Fprintf(stdout, "snapshots=%d refs=%d chunks=%d bytes=%d dry_run=%t\n", r.Snapshots, r.Refs, r.Chunks, r.Bytes, r.DryRun)
	case *adminv1.SnapshotsResponse:
		for _, s := range r.Snapshots {
			fmt.Fprintf(stdout, "%s %s %d\n", s.Name, time.Unix(0, s.ReceiveTime).UTC().Format(time.RFC3339Nano), s.Size)
		}
	default:
		fmt.Fprintf(stdout, "deleted %s %s\n", args[2], args[3])
	}
	return 0
}

// delegate replaces this process with azukari-backup from azukari's own directory, never PATH
// (ADR 0028); it returns only on a usage error or a failed exec.
func delegate(args []string, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "snapshots" && args[0] != "restore") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err == nil {
		bin := filepath.Join(filepath.Dir(self), "azukari-backup")
		err = syscall.Exec(bin, append([]string{bin}, args...), os.Environ())
		err = fmt.Errorf("exec %s: %w", bin, err)
	}
	logging.New(stderr, slog.LevelInfo).Error("", "event", "admin-failed", "call", "backup", "err", err.Error())
	return 1
}

// call posts req to /v1/admin/b/<bucket>/<name> on the socket and decodes the 200 into resp;
// a refusal returns its Error's reason. No timeout: a retention run takes what it takes.
func call(socket, name, bucket string, req, resp proto.Message) (string, error) {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	body, err := proto.Marshal(req)
	if err != nil {
		return "", err
	}
	r, err := (&http.Client{Transport: tr}).Post("http://azukari/v1/admin/b/"+url.PathEscape(bucket)+"/"+name,
		"application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	out, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if r.StatusCode != http.StatusOK {
		var e azukariv1.Error
		if err := proto.Unmarshal(out, &e); err != nil || e.Reason == "" {
			return "", fmt.Errorf("status %d", r.StatusCode)
		}
		return e.Reason, fmt.Errorf("%d %s", r.StatusCode, e.Reason)
	}
	return "", proto.Unmarshal(out, resp)
}
