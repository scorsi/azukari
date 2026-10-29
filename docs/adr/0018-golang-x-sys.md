# ADR 0018 — `golang.org/x/sys` for restore

Status: Accepted — 2026-10-29. Adds to ADR 0002 (external modules).

## Context

Restore (ADR 0006) sets the modification time of restored symbolic links without following them
(`utimensat` with `AT_SYMLINK_NOFOLLOW`) and walks the target without following links (`openat`
with `O_NOFOLLOW`). The standard library's `syscall` package does not expose the first and makes
the second awkward. `golang.org/x/sys` is already in the module graph as a dependency of
`golang.org/x/crypto` (ADR 0002).

## Decision

`golang.org/x/sys/unix` joins ADR 0002's allowed modules (owner, 2026-10-29), maintained by the Go
team, linked only into `azukari-backup`.

## Consequences

- No new module in the graph; one more direct requirement in `go.mod`.
