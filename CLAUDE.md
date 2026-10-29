# azukari — notes for AI assistants

Read [`docs/adr/`](docs/adr/) before making an architectural choice. Decisions are the owner's:
an ADR where a real choice exists is proposed with a recommendation, never accepted alone.

## Conventions

- One theme per ADR; never bundle unrelated decisions.
- Every component, package and module is named `azukari-<subproduct>`, subproduct in English;
  the operator command line alone is plain `azukari` (administration through the local socket only).
- `azukari-server` and `azukari` never contain decryption code or keys (ADR 0001, 0002).
- Public repository: no host names, domains, addresses or keys of any deployment; those live
  in the deployment repository.
- Code, comments, docs and commits in English.
