# ADR 0003 — Protocol

Status: Accepted — 2026-10-29

## Context

Nodes put, get and list objects in buckets; the `backup` type only adds (ADR 0001). Objects
are opaque bytes: a backup chunk is already compressed and encrypted, so the transport has
nothing to gain from compressing it. Control messages (listing, which objects are missing,
errors) are small and structured. The sibling stores speak house protobuf over HTTPS POST,
debuggable with `curl` (masu 0003, nisshi 0003). S3 is out of scope (0001).

## Options considered

- **Everything in protobuf**, object bytes inside a message: a size cap per request, the
  whole object in memory twice, chunking of large objects in the protocol.
- **gRPC with streaming**: a framework and HTTP/2 for a handful of calls; not `curl`-able.
- **Protobuf for control, raw bodies for objects**: objects stream in and out with
  `Content-Length`; control calls are one protobuf message each way.

## Decision

Protobuf for control, raw bodies for objects (owner, 2026-10-29). Package `azukari.v1`,
`proto/azukari/v1/`. HTTPS with mutual TLS only (ADR on identity); **HTTP/1.1 and HTTP/2**
(ALPN), from Go's `net/http` (owner, 2026-10-29): a client may run several object transfers
on one connection, and every deadline below applies per request (per stream on HTTP/2),
set through `http.ResponseController`.

### Keys

A key is 1 to 256 bytes of `[a-z0-9._-]` in `/`-separated segments, no empty segment, no
segment `.` or `..`, no leading or trailing `/`. A key that does not match is refused before
anything is read. In a `backup` bucket, the first segment of every key is a node's name
and the client writes it explicitly (owner, 2026-10-29): a key, a `missing` key or a `list`
prefix whose first segment is not the caller's node (ADR on identity) is refused with 403
`namespace-forbidden`, before anything else is read.

### Calls

| Call | Request | Response |
|---|---|---|
| `PUT /v1/b/<bucket>/o/<key>` | raw body, `Content-Length` and `Azukari-Sha256` (hex of the body) required | `201` created; `200` the key exists with the same bytes (a retry); `409` it exists with other bytes |
| `GET /v1/b/<bucket>/o/<key>` | — | `200` raw body, `Content-Length`, `Azukari-Sha256` |
| `POST /v1/b/<bucket>/missing` | `MissingRequest{keys}` (at most 10 000) | `MissingResponse{keys}`: those that do not exist |
| `POST /v1/b/<bucket>/list` | `ListRequest{prefix, start_after, limit}` (limit at most 10 000) | `ListResponse{objects{key, size, sha256}, next}` in key order |

No network call deletes or overwrites in a `backup` bucket: deletion belongs to the store's
retention and to its local administration. A later bucket type that needs either gets it in
its own ADR.

A `PUT` is visible only once complete: the store writes the body aside, checks its SHA-256
against `Azukari-Sha256`, makes it durable, then creates the key only if absent. A
connection cut mid-body leaves nothing.

### Errors

Every error carries `Error{reason}` (protobuf, `application/x-protobuf`), `reason` a stable
kebab-case string tested by the clients:

| Status | Reasons |
|---|---|
| 400 | `key-invalid`, `sha256-mismatch`, `sha256-missing`, `request-invalid`, `refs-invalid` (ADR 0007) |
| 403 | `forbidden` (identity without this right on this bucket), `namespace-forbidden` (another node's namespace in a `backup` bucket) |
| 404 | `bucket-unknown`, `object-unknown` |
| 409 | `object-exists`, `refs-incomplete` (ADR 0007) |
| 411 | `length-required` |
| 413 | `object-too-large` (the bucket's `max_object_bytes`), `too-many-keys` |
| 503 | `busy` (connection limit), `stopped` (writer stopped after an uncertain commit) |
| 507 | `disk-reserve` (free space below the bucket's reserve) |

A TLS handshake without a valid client certificate fails before HTTP: there is no 401.

### Limits

Request headers within 10 s; a body within 60 s (objects are capped per bucket; a `backup`
chunk is a few MiB); at most 64 connections and 16 concurrent streams per HTTP/2 connection;
control bodies at most 4 MiB. The HTTP/2 preface and HEADERS frames are bounded as HTTP/1
headers are: a test holds a connection after its preface and checks that the store closes it
(the gap masu 0003 found in another stack). Answers sent
before the body (403, 404, 411, 413, 400 on the key) never wait for that body.

## Consequences

- A backup run asks `missing` for its chunk keys in batches and uploads only those: the
  common case (little changed) costs a few requests, not one per chunk.
- Integrity is checked end to end on every object: the client's hash at `PUT`, the store's
  hash at `GET`, and inside the object the encryption's own tag (format ADR).
