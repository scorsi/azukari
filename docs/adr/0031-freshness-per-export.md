# ADR 0031 — Freshness per export

Status: Accepted — 2026-10-30. Amends ADR 0009 (freshness) and ADR 0030 (its labels).

## Context

ADR 0030 writes the freshness series as `{namespace,export}`. A node granted on two buckets would
then produce two series with the same labels. The store also learns a node's exports only from their
snapshots: before an export's first snapshot it cannot publish a series for it, while ADR 0009's
per-node series read 0 for a node that never backed up.

## Decision

(owner, 2026-10-30)

- `azukari_last_snapshot_timestamp_seconds{bucket,namespace,export}`: the bucket label stays, as in
  ADR 0009.
- No series exists for an export before its first snapshot (rebuilt at start from the bucket);
  there is no 0 value. "Never backed up" is the absence of the series, "stale" its age.

## Consequences

- An alert on a missing backup tests absence for the exports the deployment expects, and age for
  the others.
