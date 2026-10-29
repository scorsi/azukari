package server

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
	"github.com/scorsi/azukari/internal/bucket"
	"github.com/scorsi/azukari/internal/key"
)

// outcome is what the request and request-refused events report (ADR 0009). bucket and key
// are set only once checked, so no line carries an unbounded or foreign value.
type outcome struct {
	call, bucket, peer, key string
	code                    int
	reason                  string
	bytes                   int64
	err                     error
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	o := &outcome{}
	s.serve(w, r, o)
	attrs := []any{"bucket", o.bucket, "peer_id", o.peer, "call", o.call, "key", o.key,
		"code", o.code, "bytes", o.bytes, "dur_us", time.Since(start).Microseconds()}
	if o.code < 400 || o.code == http.StatusNotFound {
		s.log.Info("", append([]any{"event", "request"}, attrs...)...)
		return
	}
	attrs = append(attrs, "reason", o.reason)
	if o.err != nil {
		attrs = append(attrs, "err", o.err.Error())
	}
	s.log.Warn("", append([]any{"event", "request-refused"}, attrs...)...)
}

// refuse answers Error{reason} (ADR 0003).
func refuse(w http.ResponseWriter, r *http.Request, o *outcome, code int, reason string, err error) {
	o.code, o.reason, o.err = code, reason, err
	body, _ := proto.Marshal(&azukariv1.Error{Reason: reason})
	h := w.Header()
	h.Set("Content-Type", "application/x-protobuf")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.ProtoMajor == 1 && r.ContentLength != 0 {
		// Never wait for a body we refuse: net/http would otherwise read up to 256 KiB of it
		// first. HTTP/2 resets the stream instead (Connection: close there would end every stream).
		h.Set("Connection", "close")
	}
	w.WriteHeader(code)
	w.Write(body)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, o *outcome) {
	rest, isV1 := strings.CutPrefix(r.URL.Path, "/v1/b/")
	name, k, isObject := strings.Cut(rest, "/o/")
	switch {
	case isV1 && isObject && r.Method == http.MethodPut:
		o.call = "put"
	case isV1 && isObject && r.Method == http.MethodGet:
		o.call = "get"
	default:
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", nil)
		return
	}

	// Rights (ADR 0005), before anything is read.
	if r.TLS == nil {
		refuse(w, r, o, http.StatusForbidden, "forbidden", nil)
		return
	}
	id, host, err := s.src.Peer(r.TLS)
	if err != nil {
		refuse(w, r, o, http.StatusForbidden, "forbidden", err)
		return
	}
	o.peer = id
	if !s.granted[id] {
		refuse(w, r, o, http.StatusForbidden, "forbidden", nil)
		return
	}
	sb := s.buckets[name]
	if sb == nil {
		refuse(w, r, o, http.StatusNotFound, "bucket-unknown", nil)
		return
	}
	o.bucket = name
	if !sb.nodes[id] || key.Check(host) != nil {
		refuse(w, r, o, http.StatusForbidden, "forbidden", nil)
		return
	}
	if err := key.Check(k); err != nil {
		refuse(w, r, o, http.StatusBadRequest, "key-invalid", err)
		return
	}
	o.key = k
	if key.Namespace(k) != host {
		refuse(w, r, o, http.StatusForbidden, "namespace-forbidden", nil)
		return
	}
	if sb.b == nil {
		refuse(w, r, o, http.StatusServiceUnavailable, "bucket-unavailable", nil)
		return
	}

	rc := http.NewResponseController(w)
	if o.call == "get" {
		if err := rc.SetWriteDeadline(time.Now().Add(BodyTimeout)); err != nil {
			refuse(w, r, o, http.StatusInternalServerError, "internal", err)
			return
		}
		s.get(w, r, o, sb.b, name)
		return
	}
	if r.ContentLength < 0 || r.ContentLength == 0 && r.Header.Get("Content-Length") == "" {
		refuse(w, r, o, http.StatusLengthRequired, "length-required", nil)
		return
	}
	hexSum := r.Header.Get("Azukari-Sha256")
	if hexSum == "" {
		refuse(w, r, o, http.StatusBadRequest, "sha256-missing", nil)
		return
	}
	var sum [32]byte
	if n, err := hex.Decode(sum[:], []byte(hexSum)); err != nil || n != len(sum) || len(hexSum) != 2*len(sum) {
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", errors.New("Azukari-Sha256 is not 64 hex digits"))
		return
	}
	if err := rc.SetReadDeadline(time.Now().Add(BodyTimeout)); err != nil {
		refuse(w, r, o, http.StatusInternalServerError, "internal", err)
		return
	}
	body := &countReader{r: r.Body}
	created, err := sb.b.Put(k, r.ContentLength, sum, body)
	o.bytes = body.n
	switch {
	case body.err != nil && body.err != io.EOF:
		// Cut, late or short body: nothing was stored (ADR 0003).
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", body.err)
	case err != nil:
		s.refuseErr(w, r, o, name, err)
	case created:
		o.code = http.StatusCreated
		w.WriteHeader(o.code)
	default:
		o.code = http.StatusOK
		w.WriteHeader(o.code)
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, o *outcome, b *bucket.Bucket, name string) {
	h, body, err := b.Get(o.key)
	if err != nil {
		s.refuseErr(w, r, o, name, err)
		return
	}
	defer body.Close()
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Length", strconv.FormatUint(h.Size, 10))
	hd.Set("Azukari-Sha256", hex.EncodeToString(h.SHA256[:]))
	o.code = http.StatusOK
	w.WriteHeader(o.code)
	// A failure here (client gone, deadline) can no longer change the status: bytes tells it.
	o.bytes, _ = io.CopyN(w, body, int64(h.Size))
}

// refuseErr maps the bucket's refusals to ADR 0003's statuses.
func (s *Server) refuseErr(w http.ResponseWriter, r *http.Request, o *outcome, name string, err error) {
	switch {
	case errors.Is(err, bucket.ErrBecameUnavailable):
		s.log.Error("", "event", "bucket-unavailable", "bucket", name, "err", err.Error())
		refuse(w, r, o, http.StatusServiceUnavailable, "bucket-unavailable", err)
	case errors.Is(err, bucket.ErrUnavailable):
		refuse(w, r, o, http.StatusServiceUnavailable, "bucket-unavailable", err)
	case errors.Is(err, key.ErrInvalid):
		refuse(w, r, o, http.StatusBadRequest, "key-invalid", err)
	case errors.Is(err, bucket.ErrTooLarge):
		refuse(w, r, o, http.StatusRequestEntityTooLarge, "object-too-large", err)
	case errors.Is(err, bucket.ErrDiskReserve):
		refuse(w, r, o, http.StatusInsufficientStorage, "disk-reserve", err)
	case errors.Is(err, bucket.ErrSHA256Mismatch):
		refuse(w, r, o, http.StatusBadRequest, "sha256-mismatch", err)
	case errors.Is(err, bucket.ErrSizeMismatch):
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
	case errors.Is(err, bucket.ErrExists):
		refuse(w, r, o, http.StatusConflict, "object-exists", err)
	case errors.Is(err, bucket.ErrNotFound):
		refuse(w, r, o, http.StatusNotFound, "object-unknown", nil)
	default:
		// shortcut: ADR 0003 names no reason for a server-side failure (e.g. a corrupt object header); "internal" until one does.
		refuse(w, r, o, http.StatusInternalServerError, "internal", err)
	}
}

// countReader counts the body's bytes and remembers its read error.
type countReader struct {
	r   io.Reader
	n   int64
	err error
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil {
		c.err = err
	}
	return n, err
}
