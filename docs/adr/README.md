# Architecture Decision Records

MADR-lite: one decision per file, `NNNN-slug.md`, sections Context / Options considered /
Decision / Consequences. A decision is changed by a new ADR that supersedes the old one.

| ADR | Title |
|---|---|
| [0001](0001-name-and-scope.md) | Name and scope |
| [0002](0002-language-and-dependencies.md) | Language and dependencies |
| [0003](0003-protocol.md) | Protocol |
| [0004](0004-buckets-and-placement.md) | Buckets, types and placement on disk |
| [0005](0005-identity-and-rights.md) | Identity and rights |
| [0006](0006-backup-format.md) | Backup format |
| [0007](0007-retention.md) | Retention and pruning |
| [0008](0008-replication.md) | Replication |
| [0009](0009-observability.md) | Observability |
| [0010](0010-testing.md) | Testing |
| [0011](0011-configuration-files.md) | Configuration of the TLS files and of azukari-backup |
| [0012](0012-connection-limit.md) | Beyond the connection limit |
| [0013](0013-internal-error.md) | Internal error answer |
| [0014](0014-additional-events.md) | Additional events of azukari-server |
| [0015](0015-key-alphabet.md) | Uppercase letters in object keys |
| [0016](0016-svid-file-watching.md) | Picking up renewed SVIDs by watching their files |
| [0017](0017-backup-paths-root.md) | No `/` among the backed-up paths |
| [0018](0018-golang-x-sys.md) | `golang.org/x/sys` for restore |
| [0019](0019-backup-events.md) | Events of azukari-backup |
| [0020](0020-backup-privileges.md) | azukari-backup runs as a static user and reads what services export |
| [0021](0021-exports.md) | Exports: how services hand their data to azukari-backup |
| [0022](0022-export-groups.md) | One group per export |
| [0023](0023-export-writable-paths.md) | Writable paths of an export |
| [0024](0024-admin-socket-protocol.md) | Protocol of the administration socket |
| [0025](0025-root-requires-refs.md) | A root is accepted only with its refs |
| [0026](0026-replication-quick-check.md) | Replication compares sizes and modification times |
| [0027](0027-replication-skipped.md) | A skipped replication run |
| [0028](0028-one-operator-command.md) | One operator command |
