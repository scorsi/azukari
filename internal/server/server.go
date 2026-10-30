// Package server serves ADR 0003's calls over mutual TLS (ADR 0005), HTTP/1.1 and HTTP/2.
package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/svid"
)

// Limits of ADR 0003. Variables so tests can shrink them.
var (
	// HeaderTimeout bounds the TLS handshake and a request's headers, and closes a connection
	// idle between requests: on HTTP/2 that is a connection held after its preface. An HTTP/2
	// header block left incomplete while other streams are open is bounded by the ping health
	// check (see Serve).
	HeaderTimeout = 10 * time.Second
	// BodyTimeout bounds a request's body, and a response's (GET), per request or stream.
	BodyTimeout = 60 * time.Second
	// ShutdownTimeout bounds a graceful stop: requests in flight get this long to finish.
	ShutdownTimeout = 70 * time.Second
)

const (
	// MaxConns is the cap on open connections across every listener; beyond it a new
	// connection is closed at once (answering 503 busy would need its handshake first).
	MaxConns = 64
	// MaxStreams is the cap on concurrent streams per HTTP/2 connection.
	MaxStreams = 16
)

// Server holds the buckets and the SVID source.
type Server struct {
	log     *slog.Logger
	src     *svid.Source
	buckets map[string]*served
	granted map[string]bool // IDs in some bucket's nodes: any other ID gets 403 on every request
	// OnSnapshot is called, from concurrent requests, after each new snapshot root is stored.
	// New sets it to the azukari_last_snapshot_timestamp_seconds update (ADR 0009).
	OnSnapshot func(bucket, namespace string, at time.Time)
	m          metrics
	// The retention and replication clock (ADR 0007, 0008); tests replace it.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
	// isMount checks a dir replica's mount_point (ADR 0008) as New checked the buckets'.
	isMount func(string) (bool, error)
	// onPhase, when set by a test, is called before each rsync call of a replication run.
	onPhase func(phase string)
}

type served struct {
	path  string
	nodes map[string]bool
	b     *bucket.Bucket // nil: unavailable since start (ADR 0004)
	ret   config.Retention
	// Retention and operator deletions hold it for writing, one at a time; replication runs
	// hold it for reading and never wait for it (ADR 0008: skipped while retention runs).
	run      sync.RWMutex
	replicas []config.Replica // its class's, in the class's order (ADR 0008)
}

// New opens every bucket (ADR 0004), logging bucket-created and bucket-unavailable. isMount
// is bucket.IsMountPoint outside tests.
func New(cfg *config.Server, src *svid.Source, log *slog.Logger, isMount func(string) (bool, error)) (*Server, error) {
	s := &Server{log: log, src: src, buckets: map[string]*served{}, granted: map[string]bool{},
		m: metrics{requests: map[reqKey]uint64{}, received: map[string]uint64{}, snapshots: map[nsKey]int64{},
			prunedBytes: map[string]uint64{}, pruneSuccess: map[string]int64{},
			replicationSuccess: map[replicaKey]int64{}, replicationRuns: map[runKey]uint64{}},
		now: time.Now, after: time.After, isMount: isMount}
	s.OnSnapshot = s.recordSnapshot
	replicas := map[string]config.Replica{}
	for _, r := range cfg.Replicas {
		replicas[r.Name] = r
	}
	for _, c := range cfg.Buckets {
		reserve, err := config.ParseSize(c.DiskReserve)
		if err != nil {
			return nil, err
		}
		sb := &served{path: c.Path, nodes: map[string]bool{}, ret: c.Retention}
		s.m.prunedBytes[c.Name], s.m.pruneSuccess[c.Name] = 0, 0
		for _, id := range c.Nodes {
			sb.nodes[id] = true
			s.granted[id] = true
		}
		for _, r := range cfg.Classes[c.Class].Replicas {
			sb.replicas = append(sb.replicas, replicas[r])
			s.m.replicationSuccess[replicaKey{c.Name, r}] = 0
		}
		switch state, err := bucket.Open(c.Path, c.MountPoint, isMount); state {
		case bucket.Unavailable:
			log.Error("", "event", "bucket-unavailable", "bucket", c.Name, "err", err.Error())
		case bucket.Created:
			log.Info("", "event", "bucket-created", "bucket", c.Name)
			fallthrough
		default:
			sb.b = bucket.New(c.Path, c.MaxObjectBytes, reserve)
		}
		s.buckets[c.Name] = sb
		s.seedSnapshots(c.Name, sb)
	}
	return s, nil
}

// handshakeError is how net/http reports a failed TLS handshake; it offers no other hook.
const handshakeError = "http: TLS handshake error"

// Serve serves HTTPS on every listener until ctx is done, then stops gracefully.
func (s *Server) Serve(ctx context.Context, lns []net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		TLSConfig:         s.src.ServerConfig(), // built once: it reads the current SVID per handshake
		ReadHeaderTimeout: HeaderTimeout,
		IdleTimeout:       HeaderTimeout,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: MaxStreams,
			// The h2 server reads HEADERS and its CONTINUATIONs as one frame, and a client may send
			// nothing else in between (RFC 9113 6.10), so it cannot answer this ping: a header block
			// left incomplete closes the connection within HeaderTimeout of the last whole frame,
			// even while other streams keep the idle timer stopped.
			SendPingTimeout: HeaderTimeout / 2,
			PingTimeout:     HeaderTimeout / 2,
			// A peer that stops reading the connection closes it (per-stream write deadlines do not).
			WriteByteTimeout: BodyTimeout,
		},
		ErrorLog: logging.Std(s.log, func(line string) string {
			if strings.HasPrefix(line, handshakeError) {
				s.handshakeError()
				return "tls-handshake-error"
			}
			return "http-error"
		}),
	}
	slots := make(chan struct{}, MaxConns)
	errc := make(chan error, len(lns))
	var addrs []string
	for _, ln := range lns {
		addrs = append(addrs, ln.Addr().String())
		go func() { errc <- srv.ServeTLS(limitListener{ln, slots}, "", "") }()
	}
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		names = append(names, n)
	}
	slices.Sort(names)
	s.log.Info("", "event", "server-started", "buckets", names, "listen", addrs)

	select {
	case <-ctx.Done():
	case err := <-errc:
		srv.Close()
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return srv.Close()
	}
	return nil
}

// limitListener closes a new connection at once while MaxConns connections are open.
type limitListener struct {
	net.Listener
	slots chan struct{}
}

func (l limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitConn{Conn: c, slots: l.slots}, nil
		default:
			c.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *limitConn) Close() error {
	c.once.Do(func() { <-c.slots })
	return c.Conn.Close()
}
