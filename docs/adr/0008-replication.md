# ADR 0008 — Replication

Status: Accepted — 2026-10-29

## Context

The store replicates buckets itself (ADR 0001): to an off-site storage service reachable by
rsync over SSH, and to a local directory on a removable disk for the most critical data
(owner, 2026-10-29). A bucket directory is the whole truth and objects never change once
written (ADR 0004): a replica is a copy of that directory, and `azukari-backup` restores from
such a copy (ADR 0006). Pruning deletes on the store (ADR 0007).

The dangers of a copy: a bucket whose disk did not mount looks empty, and a mirror would
then delete the replica; a replica copied in the wrong order holds a snapshot whose chunks
have not arrived; a compromised store holds the replicas' credentials.

## Options considered

Who copies:

- **A separate rsync unit shipped by the module**: no code, but the bucket's availability and
  the replication's result are outside the store, which cannot guard nor report them.
- **The store, with native SFTP in Go** (`pkg/sftp`, `x/crypto/ssh`): two new modules, a
  copy protocol to write.
- **The store, running `rsync` as a child process**: rsync's incremental copy over SSH or
  to a local path, driven, guarded and reported by the store.

Which data goes where:

- **Targets per bucket**: data that must reach the removable disk goes to a dedicated bucket
  (its nodes back it up there); no filter code. A node namespace holds everything a host
  backs up, so a filter by node cannot isolate one service.
- **Targets with a namespace filter**: one bucket, a list of nodes per target.

Deletions on replicas:

- **Mirror them** after pruning: a replica's size follows the store's; protection against a
  compromised store comes from the off-site service's own snapshots, which its SSH
  credentials cannot remove (a deployment choice).
- **Never delete on replicas**: they grow without bound, and need their own pruning later.

## Decision

- **The store runs `rsync`** (owner, 2026-10-29), a runtime dependency of `azukari-server`
  with `ssh`. Each target has its schedule; one run per bucket and target at a time.
- **Bucket classes** (owner, 2026-10-29): targets are declared once; a class, named by the
  deployment (for example `standard`, `critical`), lists the targets its buckets replicate
  to; each bucket has exactly one class. Other per-class settings may join a class later,
  each with its ADR.

  ```toml
  [classes.standard]
  replicas = ["offsite"]

  [classes.critical]
  replicas = ["offsite", "local"]

  [[buckets]]
  name = "backup-critical"
  class = "critical"
  # … (ADR 0004)

  [[replicas]]
  name = "offsite"                      # [a-z0-9-], unique
  kind = "ssh"                          # rsync over SSH
  destination = "<user>@<host>:<dir>"
  port = 22
  ssh_key_file = "<path>"
  known_hosts_file = "<path>"           # the target's host key, pinned
  every = "<duration>"

  [[replicas]]
  name = "local"
  kind = "dir"
  destination = "<absolute dir>"
  mount_point = "<absolute>"            # as ADR 0004: not mounted, the run is skipped
  every = "<duration>"
  ```

  Each bucket is copied into its own directory under a target's `destination`
  (`<destination>/<bucket>/`), the bucket's layout unchanged.
- **A run never starts** on an unavailable bucket (ADR 0004), on a `dir` target whose
  `mount_point` is not mounted, or while retention runs on that bucket. A run whose
  destination holds a `FORMAT` other than the bucket's stops before copying.
- **Order of a run**, so a replica never shows a snapshot without its chunks:
  1. copy `FORMAT`, then every new object except snapshot roots (`tmp/` never copied);
  2. copy new snapshot roots;
  3. mirror deletions (owner, 2026-10-29), in retention's order: roots, then `refs`, then chunks.

  rsync writes each file under a temporary name and renames it: a cut run leaves only
  complete objects. Objects never change, so rsync compares names and sizes only.
- **Results**: one `replication-completed` or `replication-failed` event per run (bytes,
  files, deletions, duration, rsync's exit code); the last success per target is a store
  metric (observability ADR).

## Consequences

- A disk that did not mount never empties a replica.
- The off-site service and the removable disk are restore sources as they are; nothing is
  converted.
- The store holds the off-site SSH key: a compromised store can delete the off-site copy's
  current state, not the off-site service's snapshots if the deployment enables them.
- Replication is outside v0; the format and layout it relies on are not.
