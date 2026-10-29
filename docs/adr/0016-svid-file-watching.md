# ADR 0016 — Picking up renewed SVIDs by watching their files

Status: Accepted — 2026-10-29. Supersedes ADR 0005's reload of the SVID material (Decision,
Files: "reads them at start and on `SIGHUP`").

## Context

ADR 0005 has `azukari-server` re-read its SVID, key and bundle on `SIGHUP`, sent by the internal
CA's reload of the unit. The internal CA has since decided (hanko ADR-0013) that it reloads no
unit for an SVID: it only writes the three files atomically (staged, synced, renamed, `key.pem`
last), and each consumer watches its files and swaps its identity in-process. With `SIGHUP`
alone, the store would keep an expired SVID after its 24 h lifetime and every backup would fail
at the handshake.

## Decision

- `azukari-server` checks its three files before each TLS handshake: when the inode or the
  modification time of any of them differs from the last material loaded, it loads the pair and
  the bundle under a mutex; a pair that does not load or match (caught between two renames)
  keeps the previous material, and the next handshake tries again. A successful swap logs
  `svid-reloaded`, a failed one `svid-reload-failed` once per change of the files.
- `SIGHUP` stays as a manual trigger of the same load; the unit keeps `ExecReload`.
- `azukari-backup` is unchanged: a oneshot reads its files at start.
- Open connections keep their handshake; a new bundle verifies new handshakes (ADR 0005).

## Consequences

- No coupling between the internal CA and the store's unit; a renewal is picked up by the next
  connection.
- A `stat` of three files per handshake: negligible next to the handshake itself.
