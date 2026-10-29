# ADR 0010 — Testing

Status: Accepted — 2026-10-29

## Context

azukari's failures lose data silently: a chunk pruned too early, a restore that looks
complete, a bucket created on the wrong disk. The sibling products test their behaviour in
`nixosTest`s, their formats with fixed vectors, and their output against masu's and nisshi's
validators. azukari depends on the internal CA only through its file contract (ADR 0005).

## Decision

- **Unit tests** (`go test`, run by the package's check phase and `nix flake check`):
  - FastCDC boundaries, chunk identifiers, nonces and sealed bytes against **fixed vectors**
    under a fixed test key (`testdata/`): a change in their output fails the build and is a
    format version (ADR 0006);
  - object header round trip and refusal of a corrupt one; key syntax; configuration
    refusals (unknown key, duplicate name, unknown class or replica, bad duration);
  - SPIFFE verification against certificates generated in the test: wrong trust domain, two
    URI SANs, a CA leaf, an expired SVID, a server ID mismatch;
  - pruning on a fake bucket: the newest snapshot kept, `refs` without a root after the
    grace, a chunk referenced only by an unfinished run kept, `refs-incomplete` when a chunk
    is gone.
- **Build check**: `go list -deps ./cmd/azukari-server ./cmd/azukari` must not contain the
  encryption package nor `chacha20poly1305` (ADR 0001, 0002).
- **nixosTests** (`nix/tests/`, Linux): SVIDs issued in the test by `openssl` from a test CA,
  with the internal CA's profile (URI SAN only, P-256, both EKUs). Each bucket sits on an
  extra virtual disk mounted at its `mount_point`.
  - `backup-restore` (v0 acceptance): a node backs up a tree with empty files, a large random
    file, compressible text, nested directories, symbolic links, modes, owners and mtimes;
    a **second, fresh VM** holding only the node's key and SVID restores it; the trees
    compare equal (`diff -r` and a stat listing of mode, owner, mtime, link target). Then
    one file changes, a second backup uploads only a few chunks, and the first snapshot
    still restores to the original tree. `restore --verify` passes on both.
  - `rights`: an overwrite with other bytes is 409; another node's namespace is 403
    `namespace-forbidden`; an ID not in `nodes` is 403; a certificate from another CA fails
    the handshake; no method deletes; a node with a wrong key gets 409 on `config`; a held
    HTTP/2 connection after its preface is closed (ADR 0003).
  - `bucket-mount`: with its disk unmounted a bucket is unavailable (503) and nothing is
    created on the root filesystem, the other bucket serves; mounted and restarted, it is
    created.
  - `svid-reload`: the test CA reissues the server's SVID, `SIGHUP`, a new handshake sees the
    new certificate.
  - Later milestones: `retention` (durations of minutes, ADR 0007), `replication` (an SSH
    target VM and a `dir` target, deletions in order, an unmounted target skipped),
    `restore-from-dir`.
  - Every test pipes the journals into `nisshi validate --logs` and the store's `/metrics`
    into `masu validate --metrics` (test-only flake inputs).

## Consequences

- The v0 is done when `backup-restore`, `rights`, `bucket-mount` and `svid-reload` pass.
- A real backup moving to azukari keeps its old backups until a full restore from azukari
  has been compared, outside this repository's tests.
