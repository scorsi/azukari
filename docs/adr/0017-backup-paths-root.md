# ADR 0017 — No `/` among the backed-up paths

Status: Accepted — 2026-10-29. Adds to ADR 0011 (`paths`).

## Context

ADR 0011 accepts any clean absolute, non-overlapping path in `azukari-backup`'s `paths`. An
entry's path is the absolute path without its leading `/` (ADR 0006, M0 decision), so `/` itself
would become an entry with an empty path, which restore cannot place and refuses: a backup of `/`
could never be restored.

## Options considered

- **Refuse `/`**: the operator lists the directories to back up.
- **Map `/` to the restore target itself**: an empty path meaning the target, more code in
  restore for a configuration nobody needs (service data lives in named directories).

## Decision

`paths` may not contain `/` (owner, 2026-10-29); `azukari-backup check` refuses it with a message
asking for the directories to back up.

## Consequences

- Every saved entry has a non-empty path; restore's rule "no empty path" holds without exception.
