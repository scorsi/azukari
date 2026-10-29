# ADR 0004 — Buckets, types and placement on disk

Status: Accepted — 2026-10-29

## Context

Buckets have a type that fixes their contract (ADR 0001); the configuration says where each
one is written. The store runs on a host whose disks are pooled without parity: a union
filesystem over several disks, or each disk mounted on its own, mounts allowed to fail at boot
(`nofail`) so the host boots without them. When dedicated storage nodes exist, only the
mapping and the endpoint change. An object must be durable before it is acknowledged, must
never be overwritten (the `PUT` of ADR 0003 creates only if absent), and a half-written object
must never be visible.

## Options considered

Where a bucket lives:

- **On the union filesystem**: one big space; but a bucket spread over several disks without
  parity is lost, in part, as soon as **any** of them dies: for a backup bucket that means
  every snapshot missing chunks. Union filesystems also weaken `fsync` of a directory and
  atomic link and rename (oroshi 0004 refuses them for the same reason).
- **Each bucket in a directory of one disk**: a disk loss takes the buckets on that disk and
  nothing else; plain ext4 semantics. The off-site and local replicas (ADR on replication)
  cover that loss.
- **The store spreads objects over several disks itself**: capacity of all disks, but the
  same exposure as the union filesystem for a backup bucket, and placement code.

How an object's hash and size are kept (the `GET` and `list` of ADR 0003 return them):

- **A header in the object file** (magic, format version, SHA-256, size, then the bytes):
  self-describing, copied by any replication as is, verifiable offline.
- **Extended attributes**: lost by a copy that does not carry them.
- **A sidecar file per object**: two files to keep consistent per object.
- **A database**: a second source of truth for a few fields.

## Decision

- **Configuration**, one table per bucket:

  ```toml
  [[buckets]]
  name = "backup"            # [a-z0-9-], unique
  type = "backup"            # only type in v0
  class = "<name>"          # replication and other per-class settings (ADR 0008)
  path = "/absolute/dir"     # a directory on one local filesystem
  max_object_bytes = 16777216
  disk_reserve = "20GiB"     # free space kept on that filesystem; below it, 507
  mount_point = "/absolute"  # the mount that must hold `path`; "/" only on purpose
  ```

  Retention and replication settings join this table in their own ADRs. Several buckets may
  share a filesystem; a bucket never spans two.
- **Requirements on `path`**: a local filesystem with `link`, `rename` and directory `fsync`
  (ext4, xfs, btrfs); never a union or network filesystem. The deployment pins each bucket to
  one disk.
- **Layout** under `path`: `FORMAT` (layout version, `1`), `objects/<key>` (the key's segments
  are directories; key syntax in ADR 0003 makes this safe), `tmp/`. Fan-out is the key's
  design (the backup format's keys carry it), not the store's.
- **Object file**: a 64-byte header (magic `AZKO`, version, flags, size, SHA-256 of the body,
  the store's receive time, ADR 0007)
  then the body. `GET` and `list` read the header; every object can be checked
  offline against it.
- **Write path**: stream the body into `tmp/<random>` hashing it, refuse on size or hash
  mismatch, `fsync` the file, `link` it to `objects/<key>` (creating missing parent
  directories, each `fsync`ed), `fsync` the parent, unlink the temporary file. `link` fails if
  the key exists: the store then compares hashes and answers 200 or 409 (ADR 0003). Nothing is
  ever renamed over an existing object. `tmp/` is emptied at start.
- **A bucket is created only on its own mount** (owner, 2026-10-29: no manual step). On a
  disk that did not mount, creating `path` would put it on the host's root filesystem, where
  it would swallow the backups. At start, for each bucket:
  - `path/FORMAT` present and valid: available;
  - otherwise, if `mount_point` is not a mount point (same device as its parent directory) or
    `path` is not under it: unavailable;
  - otherwise, `path` absent or empty: the store creates it with `objects/`, `tmp/` and
    `FORMAT` (each `fsync`ed), logs `bucket-created`, and serves it;
  - otherwise (`path` holds files but no `FORMAT`): unavailable, never adopted.

  An **unavailable** bucket answers 503 `bucket-unavailable`, is logged and counted, and the
  other buckets keep serving (owner, 2026-10-29). A bucket whose filesystem fails later
  (write or `fsync` errors) becomes unavailable the same way until a restart.
- **Moving a bucket** (to another disk, later to a storage node): stop the store, copy the
  directory, change `path`, start. The format and the keys do not change.

## Consequences

- A disk failure loses only the buckets pinned to it; the deployment chooses which bucket
  goes where, and never puts a service's live data on the disk holding its backups.
- The store keeps no database: the bucket directory is the whole truth, and a plain copy of
  it is a complete replica.
- Each object costs one inode and its parent directories; the backup format keeps objects
  around a MiB (ADR on the format).
