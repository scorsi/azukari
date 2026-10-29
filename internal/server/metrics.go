package server

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/config"
)

// metrics holds ADR 0009's counters and the freshness gauge since process start (masu
// convention, written by hand: ADR 0002). Availability, free space and SVID expiry are read
// at scrape time.
type metrics struct {
	mu        sync.Mutex
	requests  map[reqKey]uint64
	received  map[string]uint64 // by bucket
	handshake uint64
	snapshots map[nsKey]int64 // Unix seconds, 0: no snapshot yet
}

type reqKey struct{ bucket, call, code string }

type nsKey struct{ bucket, namespace string }

// seedSnapshots reads the newest snapshot of every namespace allowed in sb (ADR 0009: the
// freshness series survives a restart). A namespace without one, or on an unavailable
// bucket, starts at 0.
func (s *Server) seedSnapshots(name string, sb *served) {
	for id := range sb.nodes {
		// A node ID the configuration checked: its host segment is the namespace (ADR 0005).
		u, err := url.Parse(id)
		if err != nil {
			continue
		}
		ns, err := config.CheckID(id, u.Host)
		if err != nil {
			continue
		}
		k := nsKey{name, ns}
		s.m.snapshots[k] = 0
		if sb.b == nil {
			continue
		}
		at, ok, err := sb.b.LastSnapshot(ns)
		if err != nil {
			s.log.Error("", "event", "snapshot-scan-failed", "bucket", name, "namespace", ns, "err", err.Error())
		} else if ok {
			s.m.snapshots[k] = at.Unix()
		}
	}
}

// recordSnapshot is the default OnSnapshot: azukari_last_snapshot_timestamp_seconds.
func (s *Server) recordSnapshot(bucket, namespace string, at time.Time) {
	s.m.mu.Lock()
	s.m.snapshots[nsKey{bucket, namespace}] = at.Unix()
	s.m.mu.Unlock()
}

// countRequest records an answered call; bucket is "" when the request never reached a
// configured bucket (peer not granted, bucket unknown).
func (s *Server) countRequest(o *outcome) {
	if o.call == "" {
		return // unrouted: call is a closed set (ADR 0003's calls), the log line tells the rest
	}
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.requests[reqKey{o.bucket, o.call, strconv.Itoa(o.code/100) + "xx"}]++
	if o.call == "put" && o.bucket != "" {
		s.m.received[o.bucket] += uint64(o.bytes)
	}
}

func (s *Server) handshakeError() {
	s.m.mu.Lock()
	s.m.handshake++
	s.m.mu.Unlock()
}

// MetricsHandler answers GET /metrics for the loopback listener (masu convention); version
// is azukari_build_info's.
func (s *Server) MetricsHandler(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		var b bytes.Buffer
		s.writeMetrics(&b, version)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write(b.Bytes())
	})
	return mux
}

func (s *Server) writeMetrics(w io.Writer, version string) {
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		names = append(names, n)
	}
	slices.Sort(names)

	family(w, "azukari_build_info", "gauge", "Build of the running binary.")
	fmt.Fprintf(w, "azukari_build_info{version=%s} 1\n", q(version))

	family(w, "azukari_bucket_available", "gauge", "1 while the bucket serves, 0 once unavailable (until a restart).")
	for _, n := range names {
		sb, up := s.buckets[n], 0
		if sb.b != nil && sb.b.Err() == nil {
			up = 1
		}
		fmt.Fprintf(w, "azukari_bucket_available{bucket=%s} %d\n", q(n), up)
	}

	family(w, "azukari_bucket_free_bytes", "gauge", "Space left to the store on an available bucket's filesystem.")
	for _, n := range names {
		sb := s.buckets[n]
		if sb.b == nil || sb.b.Err() != nil {
			continue // an absent disk would report the filesystem beneath it
		}
		if free, err := bucket.FreeSpace(sb.path); err == nil {
			fmt.Fprintf(w, "azukari_bucket_free_bytes{bucket=%s} %d\n", q(n), free)
		}
	}

	family(w, "azukari_svid_expiry_timestamp_seconds", "gauge", "NotAfter of the SVID in use.")
	fmt.Fprintf(w, "azukari_svid_expiry_timestamp_seconds %d\n", s.src.NotAfter().Unix())

	// Render the locked state, write after: a stalled scraper must not hold up requests.
	var b bytes.Buffer
	s.m.mu.Lock()
	family(&b, "azukari_requests_total", "counter", "Answered calls by status class.")
	for _, k := range sorted(s.m.requests, func(a, b reqKey) int {
		return cmp.Or(strings.Compare(a.bucket, b.bucket), strings.Compare(a.call, b.call), strings.Compare(a.code, b.code))
	}) {
		fmt.Fprintf(&b, "azukari_requests_total{bucket=%s,call=%s,code=%s} %d\n", q(k.bucket), q(k.call), q(k.code), s.m.requests[k])
	}
	family(&b, "azukari_received_bytes_total", "counter", "Body bytes read by put calls.")
	for _, n := range sorted(s.m.received, strings.Compare) {
		fmt.Fprintf(&b, "azukari_received_bytes_total{bucket=%s} %d\n", q(n), s.m.received[n])
	}
	family(&b, "azukari_last_snapshot_timestamp_seconds", "gauge", "Receive time of each namespace's newest snapshot, 0 for none.")
	for _, k := range sorted(s.m.snapshots, func(a, b nsKey) int {
		return cmp.Or(strings.Compare(a.bucket, b.bucket), strings.Compare(a.namespace, b.namespace))
	}) {
		fmt.Fprintf(&b, "azukari_last_snapshot_timestamp_seconds{bucket=%s,namespace=%s} %d\n", q(k.bucket), q(k.namespace), s.m.snapshots[k])
	}
	family(&b, "azukari_tls_handshake_errors_total", "counter", "Failed TLS handshakes on the store's listeners.")
	fmt.Fprintf(&b, "azukari_tls_handshake_errors_total %d\n", s.m.handshake)
	s.m.mu.Unlock()
	w.Write(b.Bytes())
}

func family(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// q quotes a label value with the exposition format's three escapes.
func q(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

func sorted[K comparable, V any](m map[K]V, less func(a, b K) int) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, less)
	return keys
}
