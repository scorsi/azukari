# azukari — milestones

Each milestone ends with its tests passing (ADR 0010). Technical milestones, no calendar.
v0 = M0–M2: a store, a backup component, a full backup and restore verified end to end.
The deployment's own steps live in the deployment repository.

## M0 — Foundations

- Repository on Forgejo with GitHub mirror; flake (flake-parts, treefmt, `buildGoModule`,
  devShell with `go` and `buf`); `go.mod` limited to ADR 0002.
- `proto/azukari/v1/` (protocol, ADR 0003) and `proto/azukari/backup/v1/` (metadata entries,
  ADR 0006), generated with `buf`, checked in; a check fails when the generated code is
  stale.
- Configuration of `azukari-server` and `azukari-backup` (TOML, unknown keys refused, every
  refusal of ADR 0004, 0005, 0007, 0008 that needs no disk), `check <file>` on both.
- Schema v1 log handler (copy of nisshi's Go reference).
- Build check of ADR 0010: no encryption code in `azukari-server` nor `azukari`.
- **Done when**: `nix flake check` is green on darwin and Linux, with the configuration
  fixtures accepted and refused as expected.

## M1 — Store

- Shared SPIFFE verification (ADR 0005), SVID and bundle files, reload on `SIGHUP`.
- Buckets (ADR 0004): mount check, creation, `unavailable`, object header, write path with
  `link`, disk reserve.
- `PUT`, `GET`, `missing`, `list` (ADR 0003), HTTP/1.1 and HTTP/2 with the limits; grants and
  namespaces (ADR 0005); `refs` validation and the existence check under the namespace lock
  (ADR 0007).
- Events and metrics of ADR 0009, freshness rebuilt from disk at start.
- NixOS module `services.azukari-server`: static user, config rendered with
  `pkgs.formats.toml` and checked at build time, SVID files, hardening as the sibling
  services, journald rate limit defaults; no firewall opening.
- **Done when**: nixosTests `rights`, `bucket-mount` and `svid-reload` pass, with
  `masu validate --metrics` and `nisshi validate --logs`.

## M2 — Backup and restore (v0)

- Key file and HKDF subkeys; FastCDC with its keyed gear table; identifiers, nonces, sealing;
  fixed test vectors (ADR 0006, 0010).
- `azukari-backup run`: walk the configured paths, metadata stream, `missing` in batches,
  parallel uploads, `config`, `refs` (retry on `refs-incomplete`), root.
- `azukari-backup snapshots`, `restore`, `restore --verify`, from the store.
- NixOS module `services.azukari-backup`: oneshot with a timer, key file and SVID files,
  paths, exports declared through the module (ADR 0020, 0021).
- **Done when**: nixosTest `backup-restore` passes (restore on a fresh VM, trees equal,
  second run uploads only the change, first snapshot still restores).

## M3 — Retention and the operator command

- Pruning of ADR 0007 on its schedule, `enabled = false` by default, dry runs logged.
- Local administration socket (ADR 0005) and `azukari`: run retention (`--dry-run`),
  delete a snapshot, list a namespace's snapshots.
- **Done when**: nixosTest `retention` passes with durations of minutes: old snapshots
  pruned, the newest kept, an interrupted run's chunks pruned after the grace, a backup
  running during a prune completes with every chunk present.

## M4 — Replication

- Bucket classes and replicas (ADR 0008): `rsync` over SSH and to a directory, guards, copy
  order, mirrored deletions, events and metrics.
- **Done when**: nixosTest `replication` passes: an SSH target VM and a `dir` target hold
  byte-identical buckets after backups and a prune; an unmounted target and an unavailable
  bucket are skipped and nothing is deleted.

## M5 — Restore from a copy

- `azukari-backup restore --from-dir <bucket copy>` (ADR 0006).
- `azukari backup <snapshots|restore>` runs `azukari-backup` (ADR 0028).
- **Done when**: nixosTest `restore-from-dir` passes: the store stopped, a fresh VM restores
  from a replica directory and the trees compare equal.

## M6 — One snapshot per export

- Format version 2, `[exports.<name>]`, commands and module per export, retention and operator
  calls per export (ADR 0030).
- **Done when**: every nixosTest passes on format 2, and a nixosTest shows two exports of one node on
  different cadences: each lists and restores alone, a timer that does not fire sends nothing, and
  retention keeps the newest snapshot of each export.
