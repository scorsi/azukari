# ADR 0012 — Beyond the connection limit

Status: Accepted — 2026-10-29. Supersedes ADR 0003 on the answer beyond the connection limit
(503 `busy`).

## Context

ADR 0003 caps the store at 64 connections and lists 503 `busy` for the connection limit. An
HTTP answer needs a finished TLS handshake: answering `busy` spends, on every extra
connection, the handshake and the memory the limit exists to protect.

## Options considered

- **Answer 503 `busy`** from a small extra pool that completes the handshake under a short
  deadline, then closes: the documented reason, more code on HTTP/1.1 and HTTP/2, still a
  handshake per refused connection.
- **Close the connection at once**, before the handshake: no work for a refused connection;
  the client sees a transport error.

## Decision

Beyond the limit the store closes a new connection at once, without a handshake or an answer
(owner, 2026-10-29). Clients treat a closed or reset connection like any transport error and
retry with backoff. The reason `busy` stays defined for any later limit that is checked after
the request is read.

## Consequences

- A store at its limit costs nothing per refused connection.
- `azukari-backup` retries transport errors (M2); no client depends on `busy`.
