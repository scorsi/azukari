# ADR 0001 — Name and scope

Status: Accepted — 2026-10-29

## Context

The homelab's primary data (the forge's repositories and database, the services' state) needs
backups that do not go through a workstation, survive the loss of the node that produced them
and of the local storage, and that a compromised node cannot erase. Every other piece of the
stack is built in house; the backups are the one place still served by a third-party tool.

[oroshi](../../../oroshi) (0001) keeps a generic artifact store and S3 compatibility out of its
scope on purpose: it serves package ecosystems to their stock clients and holds mostly
rebuildable cache. The data here has the opposite profile: the owner is its only source, it
must never be lost, and it is opaque to whoever keeps it.

## Options considered

- **restic or borg against an append-only server** (rest-server `--append-only`, borg
  `append_only`): proven formats, but pruning needs the repository key on the machine that
  prunes, and the server is a black box to the house observability stack.
- **An S3-compatible store (MinIO, Garage) with any backup client**: a large protocol surface
  for put, get and list; append-only needs bucket policies or object lock, and the store
  still cannot prune what it cannot read.
- **An own product**: a small store with its own protocol, typed buckets, and an own backup
  component that chunks, deduplicates and encrypts before sending.

## Decision

- Name: **azukari** (預かり, "what is kept in custody for someone"). The store keeps what nodes
  deposit, does not read it, and gives it back only to its depositor.
- One repository. Every component, package and module is named `azukari-<subproduct>`, the
  subproduct in English; the operator command alone is plain `azukari`:
  - `azukari-server`: the store;
  - `azukari-backup`: the backup component of a node (back up, list snapshots, restore);
  - `azukari`: the operator command line, run on the store's host; it talks only to the
    store's local administration socket, never over the network;
  - a library shared by them (configuration, transport, protocol).
  Each node-side subproduct is a dedicated component; there is no general-purpose client for
  nodes.
- **Push**: nodes send their own data; the store never reaches out to a node.
- **Typed buckets**: every bucket has a type that fixes its contract; the configuration maps
  each bucket to where it is written. Moving storage elsewhere changes that mapping and the
  endpoint, nothing in the format or the protocol.
- **First type, `backup`**:
  - a node identity may only add: it cannot delete nor overwrite;
  - each node writes, reads and lists only its own namespace in the bucket;
  - data is encrypted by `azukari-backup` before it leaves the node; the store never holds a
    key and has no decryption code;
  - retention and deletion are done by the store alone, from the bucket configuration, or
    by the operator through the local administration socket;
  - the store replicates the bucket to off-site and local targets itself.
- Other types (for example a shared storage bucket, not tied to nodes) come later, each
  with its own ADR.

### Out of scope

- S3 or any other standard object-storage protocol.
- The live data of services (repositories, databases, caches): they need a local POSIX
  filesystem and stay on the node that runs them. azukari keeps their backups.
- A general-purpose client for nodes, a web UI, mounting a bucket as a filesystem.
- Multi-tenancy, exposure to the public Internet.

### Boundary with oroshi

oroshi serves package ecosystems through their standard protocols and stores its blobs on
its own local filesystem (oroshi 0004); it never stores them in azukari. Its rebuildable
cache is never backed up. Its primary data, once it has some, is dumped by oroshi and
backed up by `azukari-backup` like any other service's data. Neither product depends on the
other.

## Consequences

- The backup format, retention and replication are this repository's contracts, not a
  third party's.
- A restore depends only on `azukari-backup`, the node's key and a reachable copy of the
  bucket (store, local copy or off-site copy).
- Adding a bucket type is an ADR plus code in the store; the `backup` contract does not
  change with it.
