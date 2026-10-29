# ADR 0011 — Configuration of the TLS files and of azukari-backup

Status: Accepted — 2026-10-29. Supersedes the file key names of ADR 0005 (Decision, Files) and the name `key_file` of ADR 0006 (Decision, Key). `paths` restricted by ADR 0017.

## Context

ADR 0005 names the identity files `svid_file`, `key_file`, `bundle_file` and `trust_domain` on
both sides; ADR 0006 names the node's data key `key_file` too. `azukari-backup` takes both, so
one flat configuration cannot hold them. The rest of `azukari-backup`'s configuration (the
store, the bucket, the paths) is fixed by no ADR.

## Options considered

- **Rename the data key** (`data_key_file`): ADR 0006 changes, ADR 0005 stays.
- **Put the identity files in a `[tls]` table on both sides** and rename the data key: each file key says what it
  belongs to.

## Decision

`[tls]` table on both sides, and the data key of ADR 0006 is named `data_key_file` so no two keys share a name (owner, 2026-10-29). Unknown keys stay refused.

`azukari-server`:

```toml
[tls]
trust_domain = "example.org"
svid_file = "/absolute/svid.pem"
key_file = "/absolute/key.pem"
bundle_file = "/absolute/bundle.pem"
```

`azukari-backup`:

```toml
server = "https://store.example.org:8443"  # https only, host and port, no path
bucket = "backup"                           # [a-z0-9-] (ADR 0004)
data_key_file = "/absolute/data.key"        # the node's data key (ADR 0006)
paths = ["/var/lib/a", "/var/lib/b"]        # absolute, not empty, no duplicate, none inside another

[tls]
trust_domain = "example.org"
server_id = "spiffe://example.org/store-host/azukari-server"  # in the trust domain (ADR 0005)
svid_file = "/absolute/svid.pem"
key_file = "/absolute/key.pem"
bundle_file = "/absolute/bundle.pem"
```

The node's namespace is not configured: it is the `<host>` segment of the node's own SPIFFE
ID (ADR 0003, 0005), read from its SVID.

## Consequences

- ADR 0005's semantics (files, verification, reload, grants) are unchanged; only where the
  keys sit moves.
- The store's configuration parser, already written, moves its four identity keys into `[tls]`.
