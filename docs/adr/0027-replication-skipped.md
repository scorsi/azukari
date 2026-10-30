# ADR 0027 — A skipped replication run

Status: Accepted — 2026-10-30. Adds to ADR 0009 (events and metrics of `azukari-server`).

## Context

ADR 0008 lists when a replication run never starts (an unavailable bucket, an unmounted `dir`
target, retention running on the bucket), and the M4 review added a `dir` destination that resolves
into a bucket. ADR 0009 names `replication-completed` and `replication-failed` only, and leaves the
values of `azukari_replication_runs_total`'s `result` label open.

## Decision

(owner, 2026-10-30)

| Event | Level | Fields |
|---|---|---|
| `replication-skipped` | warn | `bucket`, `replica`, `reason` (why the run did not start) |

`azukari_replication_runs_total{bucket,replica,result}` takes `result` = `completed`, `failed` or
`skipped`, one per run, the suffix of the run's event.

## Consequences

- A replica that stays skipped shows as a `skipped` count growing and a last success that ages,
  the same series ADR 0009 alerts on.
