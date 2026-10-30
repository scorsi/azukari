package server

import (
	"bytes"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scorsi/azukari/internal/logging"
	"github.com/scorsi/azukari/internal/testca"
)

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.MetricsHandler("1.2.3").ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("scrape: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	checkExposition(t, body)
	return body
}

// checkExposition holds the text format's rules masu validate also checks (metric
// convention): HELP and TYPE once per family before its samples, samples of a family
// together, counters in _total, no reserved label, a final newline.
func checkExposition(t *testing.T, body string) {
	t.Helper()
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("exposition does not end with a newline")
	}
	types := map[string]string{}
	done := map[string]bool{}
	cur := ""
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if f, ok := strings.CutPrefix(l, "# HELP "); ok {
			name, _, _ := strings.Cut(f, " ")
			if done[name] || name == cur {
				t.Fatalf("family %s declared twice", name)
			}
			if cur != "" {
				done[cur] = true
			}
			cur = name
			continue
		}
		if f, ok := strings.CutPrefix(l, "# TYPE "); ok {
			name, typ, _ := strings.Cut(f, " ")
			if name != cur || types[name] != "" {
				t.Fatalf("TYPE %q not right after its HELP", l)
			}
			if typ == "counter" != strings.HasSuffix(name, "_total") {
				t.Fatalf("%s: type %s vs _total suffix", name, typ)
			}
			types[name] = typ
			continue
		}
		name, rest, _ := strings.Cut(l, "{")
		if !strings.Contains(l, "{") {
			name, _, _ = strings.Cut(l, " ")
		}
		if name != cur || types[name] == "" {
			t.Fatalf("sample %q outside its family", l)
		}
		if strings.Contains(rest, "node=") || strings.Contains(rest, "service=") {
			t.Fatalf("reserved label in %q", l)
		}
		v := l[strings.LastIndex(l, " ")+1:]
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			t.Fatalf("value of %q: %v", l, err)
		}
	}
}

// value returns the sample of series (name and labels, as written), failing when absent.
func value(t *testing.T, body, series string) int64 {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if v, ok := strings.CutPrefix(l, series+" "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no series %s in\n%s", series, body)
	return 0
}

func TestMetrics(t *testing.T) {
	e := start(t)
	body := scrape(t, e.srv)
	for _, want := range []string{
		`azukari_build_info{version="1.2.3"} 1`,
		`azukari_bucket_available{bucket="backup"} 1`,
		`azukari_bucket_available{bucket="down"} 0`,
		`azukari_bucket_available{bucket="full"} 1`,
		`azukari_last_snapshot_timestamp_seconds{bucket="backup",namespace="node-a"} 0`,
		`azukari_last_snapshot_timestamp_seconds{bucket="down",namespace="node-a"} 0`,
		`azukari_tls_handshake_errors_total 0`,
		fmt.Sprintf("azukari_svid_expiry_timestamp_seconds %d", e.srv.src.NotAfter().Unix()),
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, `azukari_bucket_free_bytes{bucket="down"}`) || value(t, body, `azukari_bucket_free_bytes{bucket="backup"}`) <= 0 {
		t.Errorf("free bytes:\n%s", body)
	}
	if strings.Contains(body, "azukari_requests_total{") {
		t.Errorf("requests before any:\n%s", body)
	}

	c := e.client(t, nodeA, true)
	data := []byte("root")
	if a := e.do(t, c, "PUT", "/v1/b/backup/o/node-a/refs/s", bytes.NewReader(nil), hexSum(nil)); a.code != http.StatusCreated {
		t.Fatalf("refs: %d %q", a.code, a.reason) // a root needs its refs (ADR 0025)
	}
	before := time.Now().Unix()
	if a := e.do(t, c, "PUT", "/v1/b/backup/o/node-a/snapshots/s", bytes.NewReader(data), hexSum(data)); a.code != http.StatusCreated {
		t.Fatalf("snapshot: %d %q", a.code, a.reason)
	}
	after := time.Now().Unix()
	if a := e.do(t, c, "PUT", "/v1/b/down/o/node-a/chunks/x", bytes.NewReader(data), hexSum(data)); a.code != http.StatusServiceUnavailable {
		t.Fatalf("down: %d", a.code)
	}
	if a := e.do(t, e.client(t, stranger, true), "GET", "/v1/b/backup/o/node-z/x", nil, ""); a.code != http.StatusForbidden {
		t.Fatalf("stranger: %d", a.code)
	}
	if a := e.do(t, c, "DELETE", "/v1/b/backup/o/node-a/x", nil, ""); a.code != http.StatusBadRequest {
		t.Fatalf("delete: %d", a.code)
	}
	// A failed handshake: plain bytes on the TLS port.
	conn, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	conn.Read(make([]byte, 64))
	conn.Close()
	for !strings.Contains(e.logs.String(), `"tls-handshake-error"`) {
		time.Sleep(time.Millisecond)
	}

	body = scrape(t, e.srv)
	for series, want := range map[string]int64{
		`azukari_requests_total{bucket="backup",call="put",code="2xx"}`: 2,
		`azukari_requests_total{bucket="down",call="put",code="5xx"}`:   1,
		`azukari_requests_total{bucket="",call="get",code="4xx"}`:       1, // the stranger never reaches a bucket
		`azukari_received_bytes_total{bucket="backup"}`:                 4,
		`azukari_tls_handshake_errors_total`:                            1,
	} {
		if got := value(t, body, series); got != want {
			t.Errorf("%s = %d, want %d", series, got, want)
		}
	}
	if strings.Contains(body, `call=""`) {
		t.Errorf("unrouted request counted:\n%s", body) // the DELETE
	}
	if got := value(t, body, `azukari_last_snapshot_timestamp_seconds{bucket="backup",namespace="node-a"}`); got < before || got > after {
		t.Errorf("freshness %d, want within [%d, %d]", got, before, after)
	}

	// A restart rebuilds freshness from the disk.
	s2, err := New(e.cfg, e.srv.src, logging.New(&syncBuffer{}, slog.LevelInfo), func(p string) (bool, error) { return !strings.HasSuffix(p, "/down"), nil })
	if err != nil {
		t.Fatal(err)
	}
	body = scrape(t, s2)
	if got := value(t, body, `azukari_last_snapshot_timestamp_seconds{bucket="backup",namespace="node-a"}`); got < before || got > after {
		t.Errorf("rebuilt freshness %d, want within [%d, %d]", got, before, after)
	}
	if got := value(t, body, `azukari_last_snapshot_timestamp_seconds{bucket="full",namespace="node-a"}`); got != 0 {
		t.Errorf("full: %d", got)
	}
}

// The expiry gauge follows a successful SVID reload and ignores a failed one.
func TestMetricsSVIDReload(t *testing.T) {
	e := start(t)
	next := e.srv.src.NotAfter().Add(time.Hour)
	cert, key := e.ca.Issue(t, serverID, func(c *x509.Certificate) { c.NotAfter = next })
	testca.Write(t, e.tls.SVIDFile, cert)
	testca.Write(t, e.tls.KeyFile, key)
	if err := e.srv.src.Reload(); err != nil {
		t.Fatal(err)
	}
	series := "azukari_svid_expiry_timestamp_seconds"
	if got := value(t, scrape(t, e.srv), series); got != next.Unix() {
		t.Fatalf("after reload %d, want %d", got, next.Unix())
	}
	_, other := e.ca.Issue(t, serverID, nil)
	testca.Write(t, e.tls.KeyFile, other)
	if err := e.srv.src.Reload(); err == nil {
		t.Fatal("mismatched key reloaded")
	}
	if got := value(t, scrape(t, e.srv), series); got != next.Unix() {
		t.Fatalf("after failed reload %d, want %d", got, next.Unix())
	}
}
