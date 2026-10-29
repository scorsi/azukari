# ADR 0014 — Additional events of azukari-server

Status: Accepted — 2026-10-29. Adds to ADR 0009 (events of `azukari-server`).

## Context

The store logs a few situations ADR 0009's table does not name: a start that fails, the
errors `net/http` reports itself (failed TLS handshakes, malformed connections), and a bucket
whose snapshots cannot be scanned at start to rebuild freshness.

## Decision

These events join ADR 0009's table (owner, 2026-10-29), schema v1 like the others:

| Event | Level | Fields |
|---|---|---|
| `server-failed` | error | `err` (any start error; the process then exits 1) |
| `tls-handshake-error` | warn | `err` (from `net/http`'s error log, also counted by `azukari_tls_handshake_errors_total`) |
| `http-error` | warn | `err` (any other `net/http` error line) |
| `snapshot-scan-failed` | error | `bucket`, `namespace`, `err` (freshness then reads 0) |

## Consequences

- Every line the store writes has a documented event; `nisshi validate --logs` already
  accepts them.
