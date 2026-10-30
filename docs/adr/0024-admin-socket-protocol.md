# ADR 0024 — Protocol of the administration socket

Status: Accepted — 2026-10-30. Completes ADR 0005 (local administration) and ADR 0007 (operator
calls).

## Context

ADR 0005 gives the store a Unix socket for local administration and leaves its calls to the
retention ADR; ADR 0007 names three calls (run retention, delete a snapshot, list a namespace's
snapshots) but no ADR says how they travel on the socket.

## Options considered

- **The network protocol of ADR 0003 over the socket**: HTTP/1.1, one protobuf message each way,
  the same `Error{reason}`; `net/http` and the generated messages already in the store; `curl
  --unix-socket` reaches it.
- **A line protocol of its own** (one JSON request per line): shorter to write, a second format to
  keep.
- **gRPC**: a new module against ADR 0002, for three calls.

## Decision

ADR 0003's protocol over the socket (owner, 2026-10-30).

- **Socket**: `admin.sock` in the store's runtime directory (`/run/azukari/admin.sock` under the
  module), mode `0660`, group `azukari-admin` (ADR 0005). Plain HTTP/1.1, no TLS: the file's
  permissions are the authorisation. The store logs one `admin-call` event per call with the
  caller's uid (`SO_PEERCRED`), the call and its arguments (ADR 0009).
- **Messages** in package `azukari.admin.v1` (`proto/azukari/admin/v1/`), apart from the network
  API. Every call is a `POST`:

  | Call | Request | Response |
  |---|---|---|
  | `/v1/admin/b/<bucket>/retention` | `RetentionRequest{dry_run}` | `RetentionResponse` with the run's counts and bytes, once the run is over |
  | `/v1/admin/b/<bucket>/snapshots` | `SnapshotsRequest{namespace}` | `SnapshotsResponse{snapshots{name, receive_time, size}}`, oldest first |
  | `/v1/admin/b/<bucket>/delete-snapshot` | `DeleteSnapshotRequest{namespace, name}` | `DeleteSnapshotResponse{}` |

- **Errors** are `Error{reason}` as in ADR 0003, with the same statuses, plus: 404
  `snapshot-unknown`; 503 `bucket-unavailable` when the bucket's disk is not mounted.
  An unknown namespace lists no snapshot.
- **Limits**: ADR 0003's header and body deadlines; no response deadline, a retention run takes
  what it takes and the caller waits, also for a run of that bucket already in progress (one run per
  bucket at a time).
- **`azukari`** speaks this protocol only: `azukari retention <bucket> [--dry-run]`, `azukari
  snapshots <bucket> <namespace>`, `azukari delete-snapshot <bucket> <namespace> <name>`, with
  `--socket <path>` defaulting to `/run/azukari/admin.sock`. It prints one line per result and
  exits non-zero with the error's reason.

## Consequences

- An operator is a member of `azukari-admin`; nothing on the network can delete.
- The store's handlers, errors and tests serve both listeners; the admin messages can change
  without touching the nodes' API.
