# ADR 0022 — One group per export

Status: Accepted — 2026-10-29. Amends ADR 0021 (the export directory's group).

## Context

ADR 0021 gives every export directory the group `azukari-backup` with the setgid bit, so files an
export creates there are readable by the backup. Some services would rather hard-link files
they already hold than copy them (an append-only store's closed days): a hard link shares the
original's inode, owner, group and mode, and the setgid bit of a directory does not apply to a
link. The backup can read such a file only if the file itself carries a group it belongs to.

## Options considered

- **Copy instead of linking**: one more copy of the data on the node's disk.
- **The backup joins the service's own group**: it reads the live store and the service's SVID
  key (hanko ADR-0005).
- **One group per export**, shared only by the export's user and the backup: the service hands
  over exactly the files it chooses, by giving them that group.

## Decision

One group per export (owner, 2026-10-29). For `services.azukari-backup.exports.<name>` the
module creates the group `azukari-export-<name>`, whose members are the export's `user` and
`azukari-backup`, and the export directory becomes mode 2750, owner `<user>`, group
`azukari-export-<name>`. A file the export creates gets the group through the setgid bit; a
file it hard-links from the service's own data must be given the group (`chgrp`, then mode
`0640`) by the export, and only once it no longer changes.

## Consequences

- No copy for exports that link immutable files; the service's live and open files keep their
  own group and stay unreadable to the backup.
- `azukari-backup` is a member of one small group per export, each covering only what that
  service handed over; an export's user still cannot read another service's export.
- Changing a linked file's group changes it in the service's data too: an export links only
  files the service never writes again.
