package server

import (
	"crypto/sha256"
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
	code, count             int // count: missing's keys asked, list's objects answered
	reason                  string
	bytes                   int64
	err                     error
	admin                   bool // an administration call: no response deadline (ADR 0024)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	o := &outcome{}
	s.serve(w, r, o)
	s.countRequest(o)
	keyOrCount := []any{"key", o.key}
	if o.call == "missing" || o.call == "list" {
		keyOrCount = []any{"count", o.count}
	}
	attrs := append([]any{"bucket", o.bucket, "peer_id", o.peer, "call", o.call}, keyOrCount...)
	attrs = append(attrs, "code", o.code, "bytes", o.bytes, "dur_us", time.Since(start).Microseconds())
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
	writeHeader(w, o)
	w.Write(body)
}

// writeHeader starts a response with status o.code under BodyTimeout's write deadline, so no
// network response, control or refusal included, waits forever on a client that stops reading
// (ADR 0003). Administration responses have no deadline (ADR 0024).
func writeHeader(w http.ResponseWriter, o *outcome) {
	if !o.admin {
		// net/http's writers support deadlines on HTTP/1 and HTTP/2; nothing to refuse past this point anyway.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(BodyTimeout))
	}
	w.WriteHeader(o.code)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, o *outcome) {
	rest, isV1 := strings.CutPrefix(r.URL.Path, "/v1/b/")
	name, k, isObject := strings.Cut(rest, "/o/")
	ctlName, ctl, _ := strings.Cut(rest, "/")
	switch {
	case isV1 && isObject && r.Method == http.MethodPut:
		o.call = "put"
	case isV1 && isObject && r.Method == http.MethodGet:
		o.call = "get"
	case isV1 && !isObject && r.Method == http.MethodPost && (ctl == "missing" || ctl == "list"):
		o.call, name = ctl, ctlName
	default:
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", nil)
		return
	}
	if o.call == "get" && r.ContentLength != 0 {
		// HTTP/1 would wait for this body before answering, with no deadline: refuse it at once.
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", errors.New("GET with a body"))
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
	if o.call == "missing" || o.call == "list" {
		s.control(w, r, o, sb, host)
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

	if o.call == "get" {
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
	// DecodeString, not Decode into a [32]byte: Decode writes past it on a longer string.
	raw, err := hex.DecodeString(hexSum)
	if err != nil || len(raw) != sha256.Size {
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", errors.New("Azukari-Sha256 is not 64 hex digits"))
		return
	}
	sum := [sha256.Size]byte(raw)
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(BodyTimeout)); err != nil {
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
		writeHeader(w, o)
		ns := key.Namespace(k)
		// A root is <ns>/snapshots/<export>/<name> (ADR 0030).
		if snap, ok := strings.CutPrefix(k, ns+"/snapshots/"); ok && strings.Count(snap, "/") == 1 {
			exp, snap, _ := strings.Cut(snap, "/")
			s.log.Info("", "event", "snapshot-received", "bucket", name, "namespace", ns, "export", exp, "snapshot", snap)
			if s.OnSnapshot != nil {
				s.OnSnapshot(name, ns, exp, time.Now()) // the header's receive time, within the request's duration
			}
		}
	default:
		o.code = http.StatusOK
		writeHeader(w, o)
	}
}

// Limits of the control calls (ADR 0003).
const (
	maxControlBytes = 4 << 20
	maxKeys         = 10000
)

// control answers missing and list: one protobuf message each way (ADR 0003).
func (s *Server) control(w http.ResponseWriter, r *http.Request, o *outcome, sb *served, host string) {
	tooBig := func() {
		if o.call == "missing" {
			refuse(w, r, o, http.StatusRequestEntityTooLarge, "too-many-keys", nil)
		} else {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", errors.New("body over 4 MiB"))
		}
	}
	if r.ContentLength > maxControlBytes {
		tooBig()
		return
	}
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(BodyTimeout)); err != nil {
		refuse(w, r, o, http.StatusInternalServerError, "internal", err)
		return
	}
	in, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxControlBytes))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		tooBig()
		return
	case err != nil:
		refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
		return
	}

	var resp proto.Message
	if o.call == "missing" {
		var req azukariv1.MissingRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
			return
		}
		o.count = len(req.Keys)
		if len(req.Keys) > maxKeys {
			refuse(w, r, o, http.StatusRequestEntityTooLarge, "too-many-keys", nil)
			return
		}
		for _, k := range req.Keys {
			if err := key.Check(k); err != nil {
				refuse(w, r, o, http.StatusBadRequest, "key-invalid", err)
				return
			}
		}
		for _, k := range req.Keys {
			if key.Namespace(k) != host {
				refuse(w, r, o, http.StatusForbidden, "namespace-forbidden", nil)
				return
			}
		}
		if sb.b == nil {
			refuse(w, r, o, http.StatusServiceUnavailable, "bucket-unavailable", nil)
			return
		}
		keys, err := sb.b.Missing(req.Keys)
		if err != nil {
			s.refuseErr(w, r, o, o.bucket, err)
			return
		}
		resp = &azukariv1.MissingResponse{Keys: keys}
	} else {
		var req azukariv1.ListRequest
		if err := proto.Unmarshal(in, &req); err != nil {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", err)
			return
		}
		if req.Limit > maxKeys {
			refuse(w, r, o, http.StatusBadRequest, "request-invalid", errors.New("limit over 10000"))
			return
		}
		// The prefix stays in the caller's namespace: its node name alone, or that name and "/".
		prefix := req.Prefix
		if prefix == host {
			// shortcut: a bare key equal to the node name is not listed (no client writes one); list it too if one ever does.
			prefix += "/"
		}
		if !strings.HasPrefix(prefix, host+"/") {
			refuse(w, r, o, http.StatusForbidden, "namespace-forbidden", nil)
			return
		}
		if sb.b == nil {
			refuse(w, r, o, http.StatusServiceUnavailable, "bucket-unavailable", nil)
			return
		}
		limit := int(req.Limit)
		if limit == 0 {
			limit = maxKeys // proto3's unset limit: the largest page
		}
		objs, next, err := sb.b.List(prefix, req.StartAfter, limit)
		if err != nil {
			s.refuseErr(w, r, o, o.bucket, err)
			return
		}
		o.count = len(objs)
		lr := &azukariv1.ListResponse{Next: next}
		for _, ob := range objs {
			lr.Objects = append(lr.Objects, &azukariv1.Object{Key: ob.Key, Size: ob.Size, Sha256: ob.SHA256[:]})
		}
		resp = lr
	}
	reply(w, r, o, resp)
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
	writeHeader(w, o)
	// A failure here (client gone, deadline) can no longer change the status: bytes tells it.
	o.bytes, _ = io.CopyN(w, body, int64(h.Size))
}

// refuseErr maps the bucket's refusals to ADR 0003's statuses.
func (s *Server) refuseErr(w http.ResponseWriter, r *http.Request, o *outcome, name string, err error) {
	switch {
	case errors.Is(err, bucket.ErrStopped):
		if errors.Is(err, bucket.ErrBecameUnavailable) {
			s.log.Error("", "event", "bucket-unavailable", "bucket", name, "err", err.Error())
		}
		refuse(w, r, o, http.StatusServiceUnavailable, "stopped", err)
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
	case errors.Is(err, bucket.ErrRefsInvalid):
		refuse(w, r, o, http.StatusBadRequest, "refs-invalid", err)
	case errors.Is(err, bucket.ErrRefsIncomplete):
		refuse(w, r, o, http.StatusConflict, "refs-incomplete", err)
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
