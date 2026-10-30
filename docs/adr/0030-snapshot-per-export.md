# ADR 0030 — One snapshot per export

Status: Accepted — 2026-10-30. Amends ADR 0006 (keys, root, commands), 0007 (newest kept, operator
calls), 0009 (labels and fields), 0011 (backup configuration), 0021 (one run per node), 0024 (admin
calls); ADR 0028 and 0029 apply unchanged to the new arguments.

## Context

A node runs one backup timer: each run reruns all its exports and writes one snapshot holding them
all (ADR 0006, 0021). Services on one node want different cadences (a store sealing half-day
segments every few hours, a full application dump once a day), and the operator thinks in services:
"list masu's backups", "restore forgejo's dump of yesterday", not "restore the node, keep one
directory" (owner, 2026-10-30). The lab's network should carry nothing when nothing is due.

## Options considered

- **One snapshot per export, each export on its own timer**: an export is the unit of cadence,
  listing, restore and retention.
- **One snapshot per node, a cadence per export** (the earlier draft of this ADR): exports not due
  are skipped and a run with nothing due does not start; the operator still restores a node and
  keeps a directory, and retention stays per node.
- **The whole node at the fastest cadence**: slow exports rerun, every run reaches the store.

## Decision

One snapshot per export (owner, 2026-10-30).

- **Keys** (ADR 0006): `<node>/snapshots/<export>/<name>` and `<node>/refs/<export>/<name>`;
  chunks, `config` and the key check are unchanged, so chunks deduplicate across a node's exports.
  `<export>` is a key segment (ADR 0003, 0015). The root's aad is `snapshots/<export>/<name>`; its
  `paths` are the export's. This is format version 2: the bucket's `FORMAT` reads `2`, and a store
  or a restore refuses a bucket of another version.
- **Configuration** (ADR 0011): `paths` gives way to one table per export,
  `[exports.<name>] paths = ["<absolute>", …]`, the same rules for paths as today, paths of two
  exports never overlapping.
- **Commands**: `azukari-backup run <config> <export>` backs up that export's paths only;
  `snapshots <config> <export>` and `restore [--verify] [--from-dir <copy>] <config> <export>
  <snapshot> <target>`; through `azukari backup …` as ADR 0028 says.
- **Module** (ADR 0021): each export gets, besides `azukari-export-<name>.service`, its own
  `azukari-backup-<name>.service` (runs the export, then `run … <name>`) and timer, with
  `exports.<name>.every` (default daily) and the module's random delay. The node-wide backup timer
  goes away. A timer that does not fire sends nothing; a run sends its new chunks, the identifier
  lists of `missing` and `refs`, its metadata and its root.
- **Retention** (ADR 0007): the newest complete snapshot of each **export** of each node is always
  kept; `keep` and `grace` stay per bucket. ADR 0025's check is per `<export>/<name>`.
- **Operator calls** (ADR 0024): `snapshots <bucket> <namespace> <export>` and
  `delete-snapshot <bucket> <namespace> <export> <name>`.
- **Observability** (ADR 0009): events about a snapshot carry `export`;
  `azukari_last_snapshot_timestamp_seconds{namespace,export}`.

## Consequences

- An export's cadence costs only its own runs; a quiet export touches neither its node's disk nor
  the network between runs.
- The operator lists and restores a service by name; a node's full restore is one restore per export.
- A version 1 bucket cannot be read by this version: a deployment starts a new bucket (or empties
  it) and its copies.
- Per-export retention durations become a small later change, not decided here.
