# Contributing to GenesisDB

These instructions apply to contributors and coding agents throughout this repository. Read [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow and [README.md](README.md) for supported behavior. Keep changes focused on the requested task.

## Before changing code

- Open an issue before changing stored data formats, the HTTP API, public Go APIs, or behavior spanning several packages.
- Preserve compatibility with existing data directories, backups and 1.x clients. Never rename persisted JSON fields or make existing data unreadable.
- Read the affected package's README, implementation and tests before editing. Use existing patterns rather than introducing new abstractions without a concrete need.
- Do not include unrelated refactors, dependency upgrades or generated artifacts in a patch.

## Project map

This is one Go module: `github.com/genesisdb-io/genesisdb/v2`. All internal imports include `/v2`. Do not introduce `go.work`, nested modules or `replace` directives.

| Path | Responsibility |
| --- | --- |
| `cmd/genesisdb/` | Configuration, executable wiring and offline conversion |
| `packages/core/` | Events, hashes, interfaces, validation and shared durable file helpers |
| `packages/core/layout/` | Data file names, log headers and format markers |
| `packages/storage/` | Event logs, reference payloads, system events and write-ahead log |
| `packages/index/` | Derived sharded indexes, snapshots and publication journal |
| `packages/cache/` | Bounded generation-aware LRU cache |
| `packages/query/` | GDBQL parsing, planning and execution |
| `packages/store/` | Embedded database, commits, recovery, subscriptions and maintenance |
| `packages/schema/`, `packages/auth/` | Schema validation and bearer token verification |
| `packages/server/httpserver/` | HTTP routes, streaming and MCP endpoint |
| `packages/metrics/`, `packages/telemetry/` | Prometheus and OpenTelemetry adapters |
| `tests/` | Architecture rules and recorded 1.1.6 compatibility tests |
| `docs/` | API, query language, storage, compatibility and design documentation |

## Architecture and style

- Treat `tests/architecture_test.go` as the authoritative dependency rules. Do not weaken those tests to accommodate a change.
- `storage`, `index`, `cache` and `query` never import one another. Shared contracts belong in `core`.
- `store` composes `core`, `storage`, `index` and `cache`. Query integration uses interfaces and injected callbacks, not a `store` import of `query`.
- Engines use only the standard library and permitted GenesisDB packages. Keep third-party imports in `metrics`, `telemetry` and `cmd/genesisdb`.
- Engines receive configuration through `Options`, not environment variables or global state. Environment handling belongs in the executable.
- Define data file names and log headers only in `core/layout`.
- Use descriptive names without abbreviations. Functions requiring a held lock end in `WhileLocked`.
- Follow existing lock ownership, cancellation, error handling and observer patterns. Preserve race safety and commit-order reads and subscriptions.
- Format changed Go files with `gofmt`. Keep files below about 600 lines.
- Use the query language name GDBQL. Architecture checks also enforce retired terminology rules in code and documentation.

## Data integrity and security

Read [docs/storage-format.md](docs/storage-format.md) before modifying writes, recovery, indexes, migration, backup or erasure. Read [SECURITY.md](SECURITY.md) before changing authentication or handling sensitive data.

- Event logs are authoritative. Indexes are derived and rebuildable. Never repair an index by changing immutable events.
- Preserve WAL ordering, synchronization barriers, atomic batch publication and idempotent recovery. Do not acknowledge a write before its durability requirements are met.
- A failed commit may already be durable. Preserve the rejection of further writes after ambiguous storage failures until restart recovery.
- Preserve hash verification, reference-payload erasure semantics and the erasure audit trail. Do not describe hash chains as signatures or erasure as a complete compliance guarantee.
- Request limits are opt-in. A limit must fail explicitly, never silently truncate fetches, queries or backups.
- Never use the repository's `data/` directory for tests or experiments. Use `t.TempDir()` in Go tests and a dedicated disposable directory for manual runs.
- Do not read, print or commit local `.env` secrets, tokens or private event data. Redact sensitive values from logs and examples.
- Do not run conversion, restore, erasure, seeding or destructive filesystem tests against a user's existing database.

## Setup and checks

Use Go 1.26.10 or a newer patched release consistent with `go.mod`. The Makefile pins development tool versions.

```sh
make install-tools   # install pinned lint, vulnerability, secret and license tools
make lint            # gofmt check, go vet and staticcheck
make test            # all packages with the race detector
make compatibility   # recorded 1.1.6 exchanges and fixtures
make fuzz            # bounded query, backup, index codec and commit fuzzing
make vuln            # dependency and standard-library vulnerability scan
make bench           # benchmark smoke tests
make build           # static binary in bin/genesisdb, default version 0.0.0-dev
```

`make build` regenerates `THIRD_PARTY_LICENSES` and copies license files into `bin/`. Do not hand-edit generated license output. Review generated changes before including them in a patch. Avoid `make clean` unless intended, since it also removes `THIRD_PARTY_LICENSES`.

During development, run focused tests first:

```sh
go test -race ./packages/store
go test -count=1 -run TestArchitecture ./tests
```

Before submitting code, run `make lint` and `make test`, plus compatibility tests when touching externally visible or persisted behavior. Run relevant fuzzing and security checks for parser, codec, storage and authentication changes. CI runs additional checks on Linux and macOS, including secret scanning and Linux disk-full tests. Report exactly what ran and any failures or skipped checks. Do not claim checks passed when they were not run.

### Test requirements

- Every bug fix includes a regression test that reproduces the bug.
- Storage and index changes include fault injection coverage. Exercise failure and recovery paths, not just successful writes.
- Keep compatibility fixtures meaningful. Do not rewrite golden responses merely to make a regression pass.
- Use existing durability, repair, resource-security and permission tests as patterns for related changes.
- Disk-full tests skip unless `GENESISDB_FULL_VOLUME` names a dedicated disposable volume. They delete `data`, `filler` and `reserve` inside that volume. Follow the setup in CONTRIBUTING.md and never point them at an ordinary directory or existing database.

## Documentation and pull requests

- Update affected package READMEs and user documentation when behavior changes. HTTP changes belong in `docs/http-api.md`, query changes in `docs/gdbql.md`, and storage changes in `docs/storage-format.md`.
- Keep compatibility and security documentation accurate when changing those guarantees. Update Postman examples when affected by API changes.
- Branch from `main` and submit a focused pull request using the repository template. Explain the change, its reason, compatibility implications and validation performed.
- Commit subjects use the imperative mood and stay within 72 characters. Explain the reason in the body when it is not obvious.
- Do not commit secrets, local databases, test binaries or unrelated build output. Leave release tags and release publishing to the maintainer.
