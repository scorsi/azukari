# ADR 0013 — Internal error answer

Status: Accepted — 2026-10-29. Adds to ADR 0003 (Errors).

## Context

ADR 0003 lists every error a client causes. Some failures are the store's own and are not
an unavailable bucket: an object whose header is corrupt on disk, an unexpected error reading
a stored file. They need a status and a stable reason too.

## Decision

The store answers **500** with `Error{reason: "internal"}` (owner, 2026-10-29), logs the request
as `request-refused` with the error, and keeps serving; the client may retry. A failure that
makes a bucket's filesystem uncertain still makes it unavailable (ADR 0004, 503).

## Consequences

- Clients test one more reason; a retry of a 500 is safe since objects never change.
