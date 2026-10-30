# azukari (預かり)

> Store for the homelab's data, with typed buckets, starting with backups: nodes push data
> they encrypted themselves, the store only adds, keeps, prunes and replicates. Go.

*預かり (azukari): what is kept in custody for someone, and given back only to them.*

**Status:** M0 in progress.

## Components

- `azukari-server`: the store.
- `azukari-backup`: the backup component of a node: chunks, deduplicates, encrypts, pushes,
  restores.
- `azukari`: the operator command line (administration through the store's local socket);
  `azukari backup snapshots|restore` runs the node's `azukari-backup` installed next to it.

## Backups on a node

The NixOS module `azukari-backup` gives each export `<name>` (ADR 0021, 0030) the units
`azukari-export-<name>.service` (writes the export) and `azukari-backup-<name>.service` (reruns the
export, then backs it up as its own snapshots), started by `azukari-backup-<name>.timer` on
`exports.<name>.every` (default daily). To back up one export now, then list and restore it:

```
systemctl start azukari-backup-<name>
azukari backup snapshots /etc/azukari/azukari-backup.toml <name>
azukari backup restore [--verify] /etc/azukari/azukari-backup.toml <name> <snapshot> <empty target>
```

A node's full restore is one restore per export.

## Restore without the store

When the store is gone, a node restores from a copy of the bucket's directory (a replica,
or the bucket's disk mounted elsewhere) with only its configuration file and data key:

```
azukari backup snapshots --from-dir /mnt/replica/backup /etc/azukari/azukari-backup.toml <export>
azukari backup restore [--verify] --from-dir /mnt/replica/backup /etc/azukari/azukari-backup.toml <export> <snapshot> <empty target>
```

`--from-dir` names the directory holding `FORMAT` and `objects/`. No network, no SVID:
`server` and `[tls]` are not read and may be absent. The node's namespace is the one whose
`config` matches the data key. Every object is checked against its header and decrypted as
from the store; symlinks in the copy are refused, rsync's temporary `.name.XXXXXX` files and
`tmp/` are ignored.

## Not its job

The live data of services, S3 compatibility, package registries ([oroshi](../oroshi)),
a general-purpose client, a web UI.

## Documents

- [ADRs](docs/adr/README.md), [milestones](docs/milestones.md)

## Conventions

Go. Nix flake, NixOS module, `nixosTest`. Docs in English. Public repository: no host names,
domains or addresses of any deployment.
