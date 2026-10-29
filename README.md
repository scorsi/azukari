# azukari (預かり)

> Store for the homelab's data, with typed buckets, starting with backups: nodes push data
> they encrypted themselves, the store only adds, keeps, prunes and replicates. Rust.

*預かり (azukari): what is kept in custody for someone, and given back only to them.*

**Status:** design (ADRs in progress); no code yet.

## Components

- `azukari-server`: the store and its local administration.
- `azukari-backup`: the backup component of a node: chunks, deduplicates, encrypts, pushes,
  restores.

## Not its job

The live data of services, S3 compatibility, package registries ([oroshi](../oroshi)),
a general-purpose client, a web UI.

## Documents

- [ADRs](docs/adr/README.md)

## Conventions

Nix flake, NixOS module, `nixosTest`. Docs in English. Public repository: no host names,
domains or addresses of any deployment.
