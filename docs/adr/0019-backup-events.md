# ADR 0019 — Events of azukari-backup

Status: Accepted — 2026-10-29. Completes ADR 0009 (events of `azukari-backup`); `restore-completed` amended by ADR 0020 (`owners`).

## Context

ADR 0009 names `azukari-backup`'s events without all their fields, and the `snapshots` command
logs a failure that ADR 0009 does not list.

## Decision

`azukari-backup`'s events (owner, 2026-10-29), schema v1, on stderr (the journal for the unit):

| Event | Level | Fields |
|---|---|---|
| `backup-completed` | info | `bucket`, `snapshot`, `entries`, `bytes_read`, `bytes_new`, `chunks_new`, `dur_us` |
| `backup-failed` | error | `err` |
| `file-skipped` | warn | `path`, `err` (why it is skipped) |
| `file-changed` | warn | `path` |
| `restore-completed` | info | `bucket`, `snapshot`, `verify`, `entries`, `bytes`, `chunks`, `dur_us` |
| `restore-failed` | error | `snapshot`, `verify`, `err` |
| `snapshots-failed` | error | `err` |

## Consequences

- Every line `azukari-backup` writes has a documented event; tests validate them with
  `nisshi validate --logs`.
