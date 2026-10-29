# ADR 0020 — azukari-backup runs as a static user and reads what services export

Status: Accepted — 2026-10-29. Supersedes the unit of milestone M2 (root, `CAP_DAC_READ_SEARCH`,
`preCommand`); amends ADR 0019 (`restore-completed` gains `owners`).

## Context

The M2 module runs `azukari-backup` as root with `CAP_DAC_READ_SEARCH`, so one unit can read every
service's state whatever its owner and mode. Root in a oneshot that parses network input and holds
the node's data key reads every secret of the host (deployment secrets, every SVID key, the CA key
on the CA's host). Live service state is not a consistent backup anyway (open databases, SQLite
WAL): services need to export a consistent copy before a backup.

## Options considered

- **Static user with `CAP_DAC_READ_SEARCH`**: root's read power under another name;
  `open_by_handle_at` reaches every inode.
- **Read ACLs set by the module**: fragile across services that recreate or `chmod` files, and every
  file with an ACL is skipped (ADR 0006, `file-skipped`).
- **One unit per service, as that service's user**: every service user reads the node's single data
  key (ADR 0006) and shares the node's identity (ADR 0005).
- **Membership in the services' groups**: a service's group also reads that service's SVID or TLS
  key (hanko ADR-0005), and files in mode 0600 stay unreadable.
- **A privileged opener passing descriptors**: keeps the capability in one process, for about 200
  lines and a second unit.
- **Services export, the backup reads the exports through its group, with no capability.**

## Decision

The last option (owner, 2026-10-29). The module creates the static user and group
`azukari-backup` and runs the unit as them, with an empty `CapabilityBoundingSet=`,
`ProtectHome=true`, and never `PrivateUsers=` (owners would read as 65534). `data_key_file` is
owned by `azukari-backup`, mode 0400; the SVID directory has group `azukari-backup` (hanko
ADR-0005). `paths` name directories the group may read; a path the run cannot read fails the run
(no silent gap). The module has no `preCommand`: exports are ADR 0021. `restore` is a manual
command: run as root it restores owners, otherwise it restores mode and mtime and says so
(`restore-completed` field `owners`, boolean). `restore --verify` and `snapshots` need no privilege.

## Consequences

- What is backed up is what services export; a service with no export has no backup, visible in
  the store's per-node freshness only when nothing at all is exported.
- uid and gid are still saved (ADR 0006) and restored by root.
- The nixosTest `backup-restore` backs up an export, proves that an unreadable file fails the run,
  and runs `--verify` unprivileged.
- The deployment gives `azukari-backup` its SVID group and the key's ownership.
