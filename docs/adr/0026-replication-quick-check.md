# ADR 0026 — Replication compares sizes and modification times

Status: Accepted — 2026-10-30. Amends ADR 0008 (rsync compares names and sizes only).

## Context

ADR 0008 lets rsync compare names and sizes only, since objects never change once written. A key
can come back after retention removed it: a chunk pruned while a replica was unreachable is uploaded
again by a later backup, with the same name and size but a new object header (its receive time,
ADR 0007). Comparing sizes alone skips it forever and the replica stays different from the bucket
while every run reports success (M4 review, 2026-10-30).

## Options considered

- **Sizes and modification times** (rsync's default quick check, times preserved with `-t`): the
  store writes each object when it receives it, so a recreated object has a new modification time.
  No more reading than today.
- **Checksums** (`--checksum`): reads every object on both sides at each run, the off-site one
  included.
- **Compare object headers**: a copy protocol of our own.

## Decision

Sizes and modification times (owner, 2026-10-30): every rsync of a replication run preserves
modification times and uses rsync's default quick check; `--size-only` is not used.

## Consequences

- A recreated object is copied again at the next run; an unchanged one is not reread.
- An object whose modification time is changed by hand on either side is copied again, which costs
  only its transfer.
