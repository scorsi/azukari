package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	adminv1 "github.com/scorsi/azukari/gen/azukari/admin/v1"
	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/key"
	"github.com/scorsi/azukari/internal/logging"
)

// AdminSocket is the administration socket's name in the store's runtime directory (ADR 0024).
const AdminSocket = "admin.sock"

// ListenAdmin listens on the Unix socket path, mode 0660 in the process's group: the file's
// permissions are the authorisation (ADR 0005, 0024). A stale socket file is removed first.
func ListenAdmin(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// Created under the umask (0077 in the unit), then opened to the group: never wider meanwhile.
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

type uidKey struct{}

// ServeAdmin serves the administration calls of ADR 0024 on ln, plain HTTP/1.1, until ctx is
// done. Headers and bodies are bounded as on the network (ADR 0003); a call's answer is not:
// a retention run takes what it takes.
func (s *Server) ServeAdmin(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           http.HandlerFunc(s.admin),
		ReadHeaderTimeout: HeaderTimeout,
		IdleTimeout:       HeaderTimeout,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, uidKey{}, peerUID(c))
		},
		ErrorLog: logging.Std(s.log, func(string) string { return "http-error" }),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		return srv.Close()
	case err := <-errc:
		return err
	}
}

// peerUID is the connecting process's uid (SO_PEERCRED on Linux), -1 if unknown.
func peerUID(c net.Conn) int {
	uid := -1
	if uc, ok := c.(*net.UnixConn); ok {
		if raw, err := uc.SyscallConn(); err == nil {
			raw.Control(func(fd uintptr) { uid = sockUID(int(fd)) })
		}
	}
	return uid
}

// admin answers one call and logs it as admin-call (ADR 0009): uid, call, arguments, and the
// outcome.
func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	o := &outcome{}
	var args []any
	s.serveAdmin(w, r, o, &args)
	attrs := append([]any{"event", "admin-call", "uid", r.Context().Value(uidKey{}), "call", o.call}, args...)
	attrs = append(attrs, "code", o.code)
	if o.reason != "" {
		attrs = append(attrs, "reason", o.reason)
	}
	if o.err != nil {
		attrs = append(attrs, "err", o.err.Error())
	}
	s.log.Info("", attrs...)
}

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request, o *outcome, args *[]any) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/admin/b/")
	name, call, _ := strings.Cut(rest, "/")
	if !ok || r.Method != http.MethodPost || call != "retention" && call != "snapshots" && call != "delete-snapshot" {
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", nil)
		return
	}
	o.call = call
	*args = append(*args, "bucket", name)

	if r.ContentLength > maxControlBytes {
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", errors.New("body over 4 MiB"))
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(BodyTimeout)); err != nil {
		refuse(w, r, o, http.StatusInternalServerError, "internal", err)
		return
	}
	in, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxControlBytes))
	if err != nil {
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
		return
	}
	// The body is in: the call may now take as long as it needs (ADR 0024).
	_ = rc.SetReadDeadline(time.Time{})

	var resp proto.Message
	switch call {
	case "retention":
		var req adminv1.RetentionRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
			return
		}
		*args = append(*args, "dry_run", req.DryRun)
		p, err := s.RunRetention(name, req.DryRun)
		if err != nil {
			refuseAdmin(w, r, o, err)
			return
		}
		resp = &adminv1.RetentionResponse{Snapshots: uint64(p.Snapshots), Refs: uint64(p.Refs), Chunks: uint64(p.Chunks),
			Bytes: uint64(p.Bytes), DryRun: req.DryRun || !s.buckets[name].ret.Enabled}
	case "snapshots":
		var req adminv1.SnapshotsRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
			return
		}
		*args = append(*args, "namespace", req.Namespace)
		list, err := s.Snapshots(name, req.Namespace)
		if err != nil {
			refuseAdmin(w, r, o, err)
			return
		}
		sr := &adminv1.SnapshotsResponse{}
		for _, sn := range list {
			sr.Snapshots = append(sr.Snapshots, &adminv1.Snapshot{Name: sn.Name, ReceiveTime: sn.Received.UnixNano(), Size: uint64(sn.Bytes)})
		}
		resp = sr
	default:
		var req adminv1.DeleteSnapshotRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
			return
		}
		*args = append(*args, "namespace", req.Namespace, "snapshot", req.Name)
		if err := s.DeleteSnapshot(name, req.Namespace, req.Name); err != nil {
			refuseAdmin(w, r, o, err)
			return
		}
		resp = &adminv1.DeleteSnapshotResponse{}
	}
	reply(w, r, o, resp)
}

// refuseAdmin maps the operator calls' errors to ADR 0003's and ADR 0024's reasons. A bucket
// failing during the call was already logged as bucket-unavailable by the call.
func refuseAdmin(w http.ResponseWriter, r *http.Request, o *outcome, err error) {
	switch {
	case errors.Is(err, ErrUnknownBucket):
		refuse(w, r, o, http.StatusNotFound, "bucket-unknown", nil)
	case errors.Is(err, bucket.ErrNotFound):
		refuse(w, r, o, http.StatusNotFound, "snapshot-unknown", nil)
	case errors.Is(err, bucket.ErrUnavailable):
		refuse(w, r, o, http.StatusServiceUnavailable, "bucket-unavailable", err)
	case errors.Is(err, key.ErrInvalid):
		refuse(w, r, o, http.StatusBadRequest, "key-invalid", err)
	default:
		refuse(w, r, o, http.StatusInternalServerError, "internal", err)
	}
}

// reply writes a 200 with one protobuf message (ADR 0003).
func reply(w http.ResponseWriter, r *http.Request, o *outcome, m proto.Message) {
	out, err := proto.Marshal(m)
	if err != nil {
		refuse(w, r, o, http.StatusInternalServerError, "internal", err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/x-protobuf")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	o.code = http.StatusOK
	writeHeader(w, o.code)
	n, _ := w.Write(out)
	o.bytes = int64(n)
}
