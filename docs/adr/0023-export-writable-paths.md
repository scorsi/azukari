# ADR 0023 — Writable paths of an export

Status: Accepted — 2026-10-29. Amends ADR 0021 (the export unit is writable only in its
directory).

## Context

ADR 0021 runs each export with `ProtectSystem=strict` and only its own directory writable;
systemd makes that directory a separate mount. ADR 0022 lets an export hard-link immutable files
of its service and give them the export's group. Linux refuses a link across two mounts (`EXDEV`),
even on one filesystem, and the service's files are read-only to the unit (`EROFS` on `chgrp`):
the linking export cannot work in that sandbox.

## Options considered

- **Writable paths per export**: an export that links widens its writable paths to a common
  parent of its data and its directory; it still runs as the service's user, whose permissions
  bound what it can write.
- **The service links its files itself** when it seals them: the sandbox stays strict, every house
  product writes that code.
- **Copy instead of linking**: one more copy of the data.

## Decision

Writable paths per export (owner, 2026-10-29). `services.azukari-backup.exports.<name>.writablePaths`
defaults to the export's directory; when set, it must contain a path that is the export directory or
one of its parents. The unit's `ReadWritePaths` is that list; everything else stays as ADR 0021
says.

## Consequences

- A linking export (closed days of an append-only store) is declared with a common parent, for
  example the persistent root holding both the service's data and the exports.
- The wider writable area is the service user's own: the export cannot write what that user could
  not.
