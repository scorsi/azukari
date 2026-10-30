# ADR 0025 — A root is accepted only with its refs

Status: Accepted — 2026-10-30. Amends ADR 0007 (a chunk pruned during a run).

## Context

ADR 0007 checks references when they arrive: the store refuses a `refs` object naming a chunk it
does not hold. A backup run then writes its root. Between the two, pruning treats the `refs` as an
interrupted run's: once older than `grace` it deletes it, then the chunks nothing else names. A run
paused longer than `grace` after its `refs` (a suspended host, a stalled link) then writes a root
the store accepts, and the snapshot looks complete without its chunks (M3 review, 2026-10-30).

## Options considered

- **The root requires its `refs`**: on `PUT snapshots/<name>` the store, holding the namespace's
  lock, refuses the root if `refs/<name>` is absent. Pruning never deletes a chunk a present
  `refs` names, so the chunks are there too.
- **The root requires its chunks**: the store cannot read the root (it is sealed); it would read
  the `refs` anyway.
- **Keep ADR 0007 and size `grace` above any run**: the race stays, only rarer.

## Decision

The root requires its `refs` (owner, 2026-10-30).

- On `PUT <namespace>/snapshots/<name>` the store, holding the namespace's lock, checks that
  `<namespace>/refs/<name>` exists; if not, it answers 409 `refs-incomplete` and stores nothing.
- `azukari-backup`, on that answer, writes its `refs` again as ADR 0007 says (asks `missing` for the
  whole list, uploads, retries), then the root; at most three attempts in all, then the run fails.

## Consequences

- A stored root always has its `refs`, and every chunk that `refs` names exists: whatever the
  timing, a snapshot that looks complete is complete.
- One existence check under the namespace's lock per backup run.
