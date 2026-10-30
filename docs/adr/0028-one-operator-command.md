# ADR 0028 — One operator command

Status: Accepted — 2026-10-30. Amends ADR 0001 and 0024 (the operator command), ADR 0006 and 0011
(how the operator reaches `azukari-backup`'s calls).

## Context

The operator runs two kinds of calls: the store's administration (`azukari`, ADR 0024) and a node's
own data (`azukari-backup snapshots`, `restore`, `restore --from-dir`). Two command names for one
operator is one too many (owner, 2026-10-30). `azukari` must still never hold decryption code or a
key (ADR 0001, 0002): a restore decrypts with the node's data key, which only `azukari-backup` has.

## Options considered

- **`azukari` delegates node calls to `azukari-backup`**: `azukari backup <call> …` replaces its own
  process with `azukari-backup <call> …`; the operator types one command, the decrypting code stays
  in one binary.
- **One binary for everything**: the store's operator command then holds the sealing code.
- **A third binary for restores**: still several command names.

## Decision

`azukari` delegates (owner, 2026-10-30).

- `azukari backup <call> [args…]` executes (`execve`, the same process, arguments and environment
  unchanged) `azukari-backup <call> [args…]`, found in the same directory as the running `azukari`
  (one package installs both), never through `PATH`. `<call>` is `snapshots` or `restore`; any other
  is refused by `azukari` with its usage.
- `azukari` imports nothing of `azukari-backup`: the delegation is a process exec, and the
  `no-encryption` check still covers `azukari`.
- `azukari-backup` keeps its calls and arguments; its `run` is for the timer, `check` for the
  module. Documentation and operator usage name `azukari backup …` only.
- Privileges are the caller's: an operator restores as `azukari-backup` (its user can read the data
  key) or as root (to restore owners), exactly as with `azukari-backup` directly.

## Consequences

- One command name on every host; the package is installed on backup nodes as well as on the store.
- A node without the store still runs `azukari backup restore --from-dir …`: `azukari`'s socket
  calls are unaffected and not needed there.
