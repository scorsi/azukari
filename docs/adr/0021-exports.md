# ADR 0021 — Exports: how services hand their data to azukari-backup

Status: Accepted — 2026-10-29. Completes ADR 0020.

## Context

ADR 0020 makes `azukari-backup` read only what services export. Each export needs the same plumbing:
a directory the backup's group can read, a unit that runs the service's dump as the service's
user, and an order so the backup starts after the dumps. Written by hand in the deployment for
every service, the plumbing is copied and its contract is written nowhere.

## Options considered

- **Everything in the deployment repository**: the state before this ADR; copied plumbing.
- **Everything in azukari, service commands included**: azukari would know third-party services.
- **The mechanism in azukari's module, the command next to the service**: azukari owns the spool
  contract; a house product's module plugs its own dump command; the deployment declares only the
  command of third-party services.

## Decision

The last option (owner, 2026-10-29). `services.azukari-backup.exports.<name>` declares:

```nix
services.azukari-backup.exports.<name> = {
  user = "<service user>";   # the export runs as this user
  command = "<command>";     # writes the export into $EXPORT_DIR
};
```

For each export the module creates:

- the directory `/persist/backups/<name>` (base configurable), mode 2750, owner `<user>`, group
  `azukari-backup`: files created there belong to the group (setgid) and are written group-readable;
- the oneshot `azukari-export-<name>.service`, run as `<user>` with `EXPORT_DIR` set, sandboxed,
  writable only in its directory; the command replaces the previous export (the history lives in
  azukari);
- `Requires=` and `After=` from `azukari-backup.service` on it, so a failed export fails the backup;
- the directory added to the backup's `paths`.

A house product may set its own `exports.<name>` in its module when the backup component is enabled;
the command and its knowledge stay in that product.

## Consequences

- The deployment declares a user and a command per third-party service, nothing else.
- An export that compresses or encrypts its whole output changes entirely each day and deduplicates
  poorly; that is the export's format to choose.
- Exports cost the disk space of one copy of what they export.
