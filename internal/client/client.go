// Package client is azukari-backup's side of the protocol (ADR 0003): put, get, missing and
// list on one bucket of the store, over mutual TLS (ADR 0005).
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"

	azukariv1 "github.com/scorsi/azukari/gen/azukari/v1"
	"github.com/scorsi/azukari/internal/config"
	"github.com/scorsi/azukari/internal/svid"
)

// Retry and batch bounds. Variables so tests can shrink them.
var (
	// Attempts bounds the tries of one request on a transport error, 500 or 503 (ADR 0012, 0013).
	Attempts = 5
	// Backoff is the wait before the first retry, doubled before each next one.
	Backoff = 500 * time.Millisecond
	// MissingBatch is the most keys sent in one missing call (ADR 0003).
	MissingBatch = 10000
	// ListLimit is the page size asked of list (ADR 0003).
	ListLimit uint32 = 10000
)

// Error is a refusal of the store: its status and Error{reason} (ADR 0003, 0013). Reason is
// empty when the answer carried no Error message.
type Error struct {
	Status int
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("store answered %d %s", e.Status, e.Reason) }

// Client talks to one bucket of the store.
type Client struct {
	http *http.Client
	base string // https://<host>:<port>/v1/b/<bucket>
}

// New builds a client for cfg's store and bucket, presenting src's SVID and requiring the
// store's ID to be cfg's server_id. It speaks HTTP/2 when the store offers it.
func New(cfg *config.Backup, src *svid.Source) *Client {
	tr := &http.Transport{TLSClientConfig: src.ClientConfig(cfg.TLS.ServerID), ForceAttemptHTTP2: true}
	return &Client{http: &http.Client{Transport: tr}, base: cfg.Server + "/v1/b/" + cfg.Bucket}
}

// Close closes the idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Put stores body at key. created is false when the key already held the same bytes (a
// retry); other bytes there are an *Error 409 (object-exists, refs-incomplete).
func (c *Client) Put(ctx context.Context, key string, body []byte) (created bool, err error) {
	sum := sha256.Sum256(body)
	code, _, err := c.do(ctx, http.MethodPut, "/o/"+key, body, func(h http.Header) {
		h.Set("Azukari-Sha256", hex.EncodeToString(sum[:]))
	})
	return code == http.StatusCreated, err
}

// Get returns the object at key, checked against the store's Azukari-Sha256.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	_, resp, err := c.do(ctx, http.MethodGet, "/o/"+key, nil, nil)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(resp.body)
	if want := resp.header.Get("Azukari-Sha256"); want != hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("GET %s: body does not match Azukari-Sha256 %q", key, want)
	}
	return resp.body, nil
}

// Missing returns the keys that hold no object, asking in batches of MissingBatch.
func (c *Client) Missing(ctx context.Context, keys []string) ([]string, error) {
	var missing []string
	for len(keys) > 0 {
		n := min(len(keys), MissingBatch)
		var out azukariv1.MissingResponse
		if err := c.control(ctx, "missing", &azukariv1.MissingRequest{Keys: keys[:n]}, &out); err != nil {
			return nil, err
		}
		missing = append(missing, out.Keys...)
		keys = keys[n:]
	}
	return missing, nil
}

// List yields every object under prefix in key order, one page at a time; it stops at the
// first error, yielded last.
func (c *Client) List(ctx context.Context, prefix string) iter.Seq2[*azukariv1.Object, error] {
	return func(yield func(*azukariv1.Object, error) bool) {
		req := &azukariv1.ListRequest{Prefix: prefix, Limit: ListLimit}
		for {
			var out azukariv1.ListResponse
			if err := c.control(ctx, "list", req, &out); err != nil {
				yield(nil, err)
				return
			}
			for _, o := range out.Objects {
				if !yield(o, nil) {
					return
				}
			}
			if out.Next == "" {
				return
			}
			req.StartAfter = out.Next
		}
	}
}

func (c *Client) control(ctx context.Context, call string, req, out proto.Message) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return err
	}
	_, resp, err := c.do(ctx, http.MethodPost, "/"+call, body, func(h http.Header) {
		h.Set("Content-Type", "application/x-protobuf")
	})
	if err != nil {
		return err
	}
	if err := proto.Unmarshal(resp.body, out); err != nil {
		return fmt.Errorf("%s: %w", call, err)
	}
	return nil
}

type response struct {
	header http.Header
	body   []byte
}

// do sends a request until it gets an answer other than 500 or 503, at most Attempts times,
// waiting Backoff, then twice as long, between tries. A transport error (a connection the
// store closed beyond its limit, ADR 0012) is retried too; a 4xx never is. Any status >= 300
// comes back as *Error.
func (c *Client) do(ctx context.Context, method, path string, body []byte, set func(http.Header)) (int, *response, error) {
	wait := Backoff
	for attempt := 1; ; attempt++ {
		code, resp, err := c.once(ctx, method, path, body, set)
		var se *Error
		retry := ctx.Err() == nil && (err != nil && !errors.As(err, &se) ||
			code == http.StatusInternalServerError || code == http.StatusServiceUnavailable)
		if !retry || attempt == Attempts {
			if err != nil && se == nil {
				err = fmt.Errorf("%s %s: %w", method, path, err)
			}
			return code, resp, err
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, set func(http.Header)) (int, *response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body) // sets Content-Length, even 0
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if set != nil {
		set(req.Header)
	}
	r, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return 0, nil, err
	}
	if r.StatusCode >= 300 {
		e := &Error{Status: r.StatusCode}
		var m azukariv1.Error
		if r.Header.Get("Content-Type") == "application/x-protobuf" && proto.Unmarshal(data, &m) == nil {
			e.Reason = m.Reason
		}
		return r.StatusCode, nil, e
	}
	return r.StatusCode, &response{header: r.Header, body: data}, nil
}
