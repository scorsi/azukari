# ADR 0015 — Uppercase letters in object keys

Status: Accepted — 2026-10-29. Supersedes the key alphabet of ADR 0003 (Keys).

## Context

ADR 0003 allows `[a-z0-9._-]` in object keys. ADR 0006 names snapshots
`<UTC yyyymmddThhmmssZ>-<8 hex random>`, with an uppercase `T` and `Z`: the store refuses
those keys.

## Options considered

- **Lowercase snapshot names** (`yyyymmddthhmmssz`): the key alphabet stays, the format of
  ADR 0006 changes.
- **Allow uppercase letters in keys**: the snapshot name of ADR 0006 stays as written.

## Decision

A key is 1 to 256 bytes of `[A-Za-z0-9._-]` (owner, 2026-10-29); every other rule of ADR 0003
stays (segments, no empty, `.` or `..` segment, no leading or trailing `/`). Keys are
case-sensitive: `a` and `A` are two keys. Bucket names keep `[a-z0-9-]` (ADR 0004). Snapshot
names are exactly ADR 0006's `yyyymmddThhmmssZ-<8 hex>`.

## Consequences

- Bucket filesystems must be case-sensitive (ext4, xfs, btrfs are; ADR 0004 already requires
  them).
- A node's `<host>` segment may hold uppercase letters as the internal CA allows; it is still
  checked as a key segment.
