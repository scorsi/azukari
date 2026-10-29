# ADR 0006 — Backup format

Status: Accepted — 2026-10-29. Key file name superseded by ADR 0011 (`data_key_file`); erratum on the chunk overhead (42 bytes).

## Context

`azukari-backup` chunks, deduplicates and encrypts a node's files before they leave it, and
restores them (ADR 0001). Decided already: one object per chunk, content-defined chunking
(FastCDC, written here), zstd before encryption, XChaCha20-Poly1305 from `x/crypto`, chunk
identifiers keyed with HMAC-SHA-256 so the store cannot test for a known content (ADR 0002),
one data key per node kept in the deployment's secrets (owner, 2026-10-29). The store must be
able to prune without reading anything (retention ADR). A node may die: a restore needs only
the key and a copy of the bucket, the store itself or a replica of its directory.

## Options considered

Metadata (the file list):

- **One encrypted tree object per directory** (restic): an unchanged directory is one shared
  object, but many small objects and a walk of requests at restore.
- **One metadata stream per snapshot** (owner, 2026-10-29), entries in path order, chunked and stored like file
  data: unchanged runs of entries deduplicate as chunks do; one code path for all bytes.

Unchanged files:

- **Read and hash every file on every run**: the simplest; hashing runs at disk speed
  (owner, 2026-10-29: v0).
- **Skip files whose size, mtime and inode match the previous snapshot**: much less reading,
  needs the previous snapshot's metadata first.

Chunker table:

- **Fixed gear table**: identical boundaries on every node.
- **Gear table derived from the node's key** (owner, 2026-10-29): chunk sizes, which the
  store sees, no longer match those of a known file chunked elsewhere.

## Decision

### Key

`key_file` holds 32 random bytes as 64 hex characters (a newline allowed). Subkeys come from
HKDF-SHA-256 (`crypto/hkdf`) with distinct labels: `azukari id` (chunk identifiers),
`azukari enc` (encryption), `azukari nonce` (chunk nonces), `azukari gear` (chunker table),
`azukari check` (key check).
The key never leaves the node except in the deployment's secrets, which are the recovery
path: losing every copy of it loses the backups, by design.

### Objects, all under the node's namespace (ADR 0003)

| Key | Content |
|---|---|
| `<node>/config` | plaintext JSON `{"format":1,"key_check":"<hex>"}`, `key_check` = HMAC-SHA-256 under the check subkey of the string `azukari`; written once (a second `PUT` of the same bytes is a 200, different bytes a 409: a wrong key is refused before any upload) |
| `<node>/chunks/<2 hex>/<64 hex>` | one chunk; the identifier is HMAC-SHA-256 of the chunk's plaintext under the id subkey; the first byte pair fans out the directory |
| `<node>/snapshots/<UTC yyyymmddThhmmssZ>-<8 hex random>` | one snapshot's root |
| `<node>/refs/<same name as the snapshot>` | the plaintext list of every chunk the snapshot references, for the store's retention (retention ADR) |

### Sealed objects

Chunks and snapshot roots are sealed the same way:

```
byte 0      version (1)
bytes 1–24  nonce
rest        XChaCha20-Poly1305(enc subkey, nonce, plaintext, aad = object key without the node segment)
plaintext   byte 0: compression (0 none, 1 zstd), then the payload
```

A chunk's nonce is the first 24 bytes of HMAC-SHA-256 of its identifier under the nonce
subkey: the same chunk always seals to the same bytes, so a `PUT` retried after a cut
connection is a 200, never a 409, and every replica holds identical objects. A nonce repeats
only for an identical plaintext under the same key, which reveals nothing the identifier does
not already reveal. A snapshot root, written once under a fresh name, takes a random nonce;
the client keeps its sealed bytes for retries.

The associated data binds every sealed object to its key: a store, replica or attacker that
swaps two objects is detected at decryption. After decryption a chunk's plaintext is hashed
again and must equal its identifier. zstd is kept only when it saves space.

### Chunking

FastCDC, minimum 256 KiB, target 1 MiB, maximum 4 MiB; its gear table is derived from the
gear subkey. A chunk object is at most 4 MiB plus 42 bytes (erratum 2026-10-29: version 1 + nonce 24 + compression 1 + tag 16; the text said 41).

### Snapshot

The metadata stream is a sequence of length-delimited protobuf `Entry` records in path order
(`proto/azukari/backup/v1/`): relative path, type (file, directory, symlink), mode, uid, gid,
mtime in nanoseconds, size, symlink target, and for a file the list of its chunk identifiers.
The stream is chunked and stored exactly like file data.

The snapshot root (sealed, aad = `snapshots/<name>`) holds: format version, start and end
times, the configured paths, the metadata stream's chunk identifiers, and totals (entries,
bytes, chunks, new chunks).

A snapshot is complete once its root exists: chunks first, then `refs/<name>`, then the root.
An interrupted run leaves chunks without a root, which retention removes after its grace
period.

### Scope of what is saved

Regular files, directories, symbolic links, with mode, numeric owner and group, and mtime.
Hard links are saved as separate files. Extended attributes, ACLs, devices, sockets and FIFOs
are skipped with a `warn` event per path. A file that changes while it is read is saved as
read and logged (`file-changed`); consistency of a service's data is the deployment's job (a
dump or a stopped service before the run).

### Restore

`azukari-backup restore` reads a snapshot from the store, or **from a directory holding a copy
of the bucket** (a replica, ADR on replication) when the store is gone (owner, 2026-10-29: in a
late milestone, after v0): the object header of
ADR 0004 is read the same way. It writes into an empty target directory, decrypts and checks
every object (tag, identifier, aad), restores mode, mtime and, when run as root, owner, and
ends with a summary that must match the root's totals. `restore --verify` does the same and
writes nothing.

## Consequences

- Unchanged files are read in full on every run (recommended for v0): the time of a run grows
  with the data size, not with the change. Skipping them by size, mtime and inode, using the
  previous snapshot's metadata, is a later change that leaves this format unchanged.
- Nodes never share chunks: cross-node deduplication is impossible by construction, the price
  of a key per node.
- The store sees per node: object sizes (chunks of 256 KiB–4 MiB after compression), counts,
  times, and which chunks a snapshot references. It sees no content, no name, no file size.
- FastCDC and the identifier get test vectors in the repository: a change in their output is a
  format version.
