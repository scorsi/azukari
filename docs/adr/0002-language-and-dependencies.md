# ADR 0002 — Language and dependencies

Status: Accepted — 2026-10-29

## Context

azukari is one store and a backup component run as a oneshot on every node. It needs mutual
TLS with X.509-SVIDs, the peer's identity read from the certificate's URI SAN instead of a
DNS name, and certificate and trust-bundle files reloaded when the internal CA renews them;
an HTTPS server streaming large request bodies under strict deadlines; protobuf; zstd;
authenticated encryption and keyed hashing from proven libraries, never a home-made scheme;
content-defined chunking; careful file I/O (`fsync`, atomic rename). A bug loses data:
robust beats clever. Memory is not a criterion: there is one store, and the node side does
not stay resident.

## Options considered

| | For | Against |
|---|---|---|
| Zig | small binaries | no TLS server in std, its client cannot present a certificate; no zstd compressor, no protobuf |
| Nim | small binaries | TLS through OpenSSL bindings, immature crypto and protobuf libraries |
| Elixir | `:ssl` does full mTLS with a SAN check in `verify_fun`; concurrency | no XChaCha20 in OTP `:crypto` (libsodium NIF); a BEAM runtime and a slow start for a oneshot on every node |
| Rust | best crypto crates (`blake3`, RustCrypto `chacha20poly1305`, `fastcdc`); the sibling stores' hardened hyper server to copy | rustls has no URI SAN verification: custom client and server certificate verifiers to write; tokio and hyper for a few clients |
| Go | `crypto/tls` presents and verifies client certificates, a URI SAN check is one `VerifyPeerCertificate`, reload through `GetCertificate`; `net/http` bounds headers and bodies natively; `x/crypto` has XChaCha20-Poly1305; the internal CA and the log schema reference are Go | GC, irrelevant here; BLAKE3 and FastCDC are not in the standard library |

## Decision

Go, `CGO_ENABLED=0`, `buildGoModule`. One Go module, three commands (`cmd/azukari-server`,
`cmd/azukari-backup`, `cmd/azukari`) and shared packages under `internal/` (ADR 0001).

External modules allowed:

| Need | Module | Why not stdlib |
|---|---|---|
| XChaCha20-Poly1305 | `golang.org/x/crypto/chacha20poly1305` (`NewX`) | not in stdlib; maintained by the Go team. Linked only into `azukari-backup` |
| protobuf | `google.golang.org/protobuf` | the protocol's control messages; generated with `buf`, checked in as nisshi does |
| zstd | `github.com/klauspost/compress/zstd` | no zstd in stdlib; pure Go, standard frames |
| TOML | `github.com/pelletier/go-toml/v2` | configuration, `DisallowUnknownFields()`, as genkan, nisshi and hanko |

From the standard library, with no module added:

- **Keyed chunk identifiers: HMAC-SHA-256** (`crypto/hmac`, `crypto/sha256`). BLAKE3 is
  faster, but Go uses the CPU's SHA instructions on amd64 and arm64, which keeps hashing
  faster than the disks being read; a keyed hash from the standard library beats a
  third-party one for the same property (the store cannot test whether a known content is
  present).
- **Content-defined chunking: FastCDC written here** (about 60 lines plus tests). The only
  Go library (`jotfs/fastcdc-go`) has not been released since 2020. Its gear table and
  parameters are part of the backup format (ADR on the format): changing them costs
  deduplication, never correctness.
- TLS, the HTTP server and client, the local administration socket, JSON logs (a copy of
  nisshi's schema v1 Go reference, as hanko and genkan), the `/metrics` text exposition.

`azukari-server` and `azukari` never import the encryption package; a test checks it in the build
(`go list -deps`), so the store's inability to decrypt holds by construction (ADR 0001).

Configuration is TOML, rendered by the NixOS module with `pkgs.formats.toml`. Any other
module needs a new ADR.

## Consequences

- One toolchain with the internal CA (hanko) and the log store (nisshi): the same certificate
  types, the same log handler.
- FastCDC and the chunk identifier are house code on the data path: they get test vectors
  fixed in the repository, since a change in their output is a format change.
