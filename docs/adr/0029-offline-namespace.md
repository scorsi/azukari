# ADR 0029 — The namespace of a restore from a copy

Status: Accepted — 2026-10-30. Completes ADR 0006 (restore from a copy) and ADR 0011 (the
namespace is the host part of the node's SVID).

## Context

A restore from the store knows its namespace from the node's SVID (ADR 0005, 0011). A restore from
a copy (ADR 0006) runs where the store is gone, often on a fresh host that has no SVID, or on another
host than the dead node; it has the node's backup configuration and data key only.

## Options considered

- **The namespace whose key check matches the data key**: each namespace holds its key check
  (ADR 0006); only the node's own key matches its own.
- **The SVID's host part when an SVID is present, else an option**: a fresh host needs the option,
  and a wrong name fails late.
- **A required `--namespace` option**: one more thing to know during a recovery.

## Decision

The key decides (owner, 2026-10-30). `restore --from-dir` and `snapshots --from-dir` read every
namespace's key check in the copy and use the one that the data key opens; none is an error, more
than one is refused. No SVID, `[tls]` or `server` is read in this mode.

## Consequences

- A dead node's data restores on any host holding its configuration and key, without a CA change.
- Reading the copy's key checks costs one small object per namespace.
