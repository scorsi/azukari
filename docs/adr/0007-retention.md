# ADR 0007 — Retention and pruning

Status: Accepted — 2026-10-29

## Context

Only the store deletes, from the bucket's configuration, by age, or the operator through the
local socket (ADR 0001, 0005; owner, 2026-10-29). The store cannot read a backup: it knows
each snapshot's name, the plaintext list of chunks it references (`refs/<name>`, ADR 0006) and
what it stored when. Nodes add only; a compromised node can add anything to its own
namespace, including fake snapshots with any name, but cannot delete.

Pruning must never remove a chunk that a snapshot, finished or being written, still needs.
A backup run asks `missing`, uploads what is missing, then writes `refs` and the root: a
chunk reported present at the start of a run may be pruned before the run ends.

## Options considered

Policy:

- **Keep every snapshot younger than `keep`**, plus the newest of each node whatever its
  age (owner, 2026-10-29): one number; with deduplication, a daily snapshot costs only what changed.
- **Thinning** (keep N daily, M weekly, P monthly): longer history for the same space, more
  configuration, and an attacker who adds one snapshot per period can push the real ones out
  of their slots.

Age of a snapshot:

- **The time in its name**: chosen by the node, so a compromised node chooses it.
- **The time the store received its root**, recorded by the store.

A chunk pruned during a run:

- **A grace period alone**: delete an unreferenced chunk only if received long enough ago;
  a run longer than the grace still loses.
- **Check references when they arrive**: the store refuses a `refs` object naming a chunk it
  does not hold, under the same per-namespace lock as pruning; the client uploads what is
  missing and retries. Correct whatever the timing.

## Decision

- **Receive time in the object header**: the header of ADR 0004 gains the store's receive
  time (8 bytes, nanoseconds since the epoch, within the 64 bytes). A snapshot's age is its
  root's receive time.
- **Policy per bucket**; the values are the deployment's, the product sets only the defaults
  shown:

  ```toml
  [buckets.retention]
  enabled = false          # default; until true, a run only logs what it would delete
  keep = "<duration>"      # required; every snapshot received within it is kept
  grace = "48h"            # default; unreferenced chunks younger than this are kept
  run_at = "<hh:mm>"       # required; local time, daily; also at the operator's request
  ```

  Durations take `d`, `h`, `m`, `s` and must be positive; there is no floor, so a test can
  run retention within minutes (owner, 2026-10-29). The newest complete snapshot of each node
  is always kept, so a node that stopped backing up keeps its last backup until the operator
  removes it.
- **`refs` objects** are the sorted concatenation of 32-byte chunk identifiers, without
  duplicates; the store refuses any other content (400 `refs-invalid`). On `PUT refs/<name>`
  the store, holding the namespace's lock, checks that every listed chunk exists; if one does
  not, it answers 409 `refs-incomplete` and stores nothing. `azukari-backup` then asks
  `missing` for the whole list, uploads, and retries (at most three times, then the run fails).
- **A run**, per bucket and per namespace, under the namespace's lock:
  1. complete snapshots (a root and its `refs`) older than `keep`, except the newest:
     delete the root, then its `refs`;
  2. `refs` without a root (an interrupted run) older than `grace`: delete;
  3. load every remaining `refs` into a set; delete every chunk not in it and received more
     than `grace` ago;
  4. `fsync` each touched directory; log one `prune-completed` event with counts and bytes.

  A root is always deleted before its `refs`, and `refs` before chunks: no state on disk
  shows a snapshot whose chunks are gone. A run interrupted at any step leaves only more data
  than needed.
- **Operator calls** on the local socket (ADR 0005), through `azukari`: run retention now
  (`--dry-run` available whatever `enabled` says), delete one snapshot (root then `refs`; its
  chunks go at the next run), and list a namespace's snapshots with their receive times and
  sizes.

## Consequences

- A compromised node can fill its namespace but not make the store delete a real snapshot
  younger than `keep`; its flood is pruned like anything else once old enough. `keep` is
  also the time to notice a compromise before its first real snapshots expire; a short `keep`
  with `enabled = true` is the operator's choice and shortens it.
- Deletion is off until the operator enables it per bucket: the first weeks show, in the
  logs, what retention would do.
- Replicas receive deletions according to the replication ADR, which can keep them longer.
- Pruning reads every `refs` of a namespace at each run: a few MB per hundred thousand
  chunks.
