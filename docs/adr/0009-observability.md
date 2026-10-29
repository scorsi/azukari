# ADR 0009 — Observability

Status: Accepted — 2026-10-29

## Context

The house stack scrapes each service's loopback `/metrics` and reads its journal (hikyaku),
under masu's metric convention and nisshi's log schema v1 (nisshi ADR 0009). `azukari-backup`
is a oneshot: nothing stays alive to be scraped, as with hanko (hanko ADR-0007). The store
sees every backup arrive and is the one place that can tell a node stopped backing up
(owner, 2026-10-29: metrics on the store only). A backup run moves up to millions of chunks:
one log line per object would flood the journal.

## Options considered

- **Metrics from `azukari-backup` too** (a textfile, a push): no collector for either in the
  stack (hanko ADR-0007 rejected the textfile).
- **Store metrics, and logs from every binary**: the store measures freshness and volumes,
  each run of `azukari-backup` ends with one summary line.

Logging of requests:

- **One line per request** (owner, 2026-10-29): complete; a run of a million chunks writes a
  million lines, within the journal's rate limit at the pace of uploads.
- **One line per snapshot and per refusal**: what an operator reads; volumes in metrics only.

## Decision

- **Logs**: `azukari-server`, `azukari-backup` and `azukari` write schema v1 lines on stderr,
  through a copy of nisshi's Go reference handler (as hanko and genkan). Never key material, a
  chunk's content or a file name of a backed-up tree in the store's logs; `azukari-backup`
  logs paths only for skipped or changed files.
- **Events, `azukari-server`**:

  | Event | Level | Fields |
  |---|---|---|
  | `server-started` | info | `buckets`, `listen` |
  | `svid-reloaded` / `svid-reload-failed` | info / error | `not_after` / `err` |
  | `bucket-created` | info | `bucket` |
  | `bucket-unavailable` | error | `bucket`, `err` |
  | `request` (every request answered under 400, and 404) | info | `bucket`, `peer_id`, `call`, `key` or `count`, `code`, `bytes`, `dur_us` |
  | `request-refused` (400, 403, 409, 411, 413, 503, 507) | warn | the same, and `reason` |
  | `snapshot-received` | info | `bucket`, `namespace`, `snapshot` |
  | `prune-completed` / `prune-failed` | info / error | `bucket`, counts and bytes, `dry_run` / `err` |
  | `replication-completed` / `replication-failed` | info / error | `bucket`, `replica`, counts, bytes, `dur_us` / `err`, `exit_code` |
  | `admin-call` | info | `uid`, `call`, arguments |

- **Events, `azukari-backup`**: `backup-completed` (`bucket`, `snapshot`, entries, bytes read,
  bytes and chunks new, `dur_us`), `backup-failed` (`err`), `file-skipped` and
  `file-changed` (warn, `path`), `restore-completed`, `restore-failed`.
- **Rate limit**: the NixOS module sets nisshi ADR 0009's per-request values
  (`LogRateLimitIntervalSec=30s`, `LogRateLimitBurst=30000`) as defaults.
- **Metrics, `azukari-server` only**, on the loopback listener of masu's convention; `node`
  is a reserved label there, so a node namespace is labelled `namespace` (a configured
  value):

  ```
  azukari_build_info{version}
  azukari_requests_total{bucket,call,code}
  azukari_received_bytes_total{bucket}
  azukari_bucket_available{bucket}
  azukari_bucket_free_bytes{bucket}
  azukari_last_snapshot_timestamp_seconds{bucket,namespace}
  azukari_prune_last_success_timestamp_seconds{bucket}
  azukari_pruned_bytes_total{bucket}
  azukari_replication_last_success_timestamp_seconds{bucket,replica}
  azukari_replication_runs_total{bucket,replica,result}
  azukari_svid_expiry_timestamp_seconds
  azukari_tls_handshake_errors_total
  ```

  `call` is the closed set of ADR 0003's calls, `code` the status class. The store reads the
  last snapshot time per namespace from the disk at start, so the freshness series survives a
  restart.
- The store's tests run `masu validate --metrics` and `nisshi validate --logs` (test-only
  flake inputs).

## Consequences

- "Node X has not backed up for a day" and "replica Y is a week old" are each one series.
- A backup that fails before reaching the store shows as missing freshness on the store and
  as `backup-failed` in that node's journal.
