# Changelog

All notable changes to GenesisDB are documented in this file. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release notes with the commits of each version are on the [GitHub releases page](https://github.com/genesisdb-io/genesisdb/releases). This file records the notable changes of each line.

## [2.0.0]

The first open source release. GenesisDB 2 is the successor of the 1.x line and a drop-in replacement for it: an existing data directory is migrated automatically on first start, every 1.x backup restores, HTTP routes and responses stay compatible. The migration is one-way. See [docs/compatibility.md](docs/compatibility.md).

### Added
- **Open source under MIT.** Every feature is included, there are no editions, license keys or event limits.
- **Modular Go engines**: `core`, `storage`, `index`, `cache`, `query`, `store`, `schema`, `auth`, `metrics` and `server` are separate packages of one Go module. Each engine can be imported and used on its own, see [docs/engines.md](docs/engines.md).
- **MCP endpoint**: `GENESISDB_MCP=true` serves the Model Context Protocol at `/mcp`. AI agents list subjects and types, fetch events, run GDBQL queries and, with a write token, commit events. Tools follow the token role, administration stays out of reach. See [docs/mcp.md](docs/mcp.md).
- **Embeddable store**: `store.Open` returns a database without global state, usable inside any Go program.
- **Storage format 2**: data directories use `.gdb` files, the log header `genesisdb` and a `format.gdb` marker. A 1.x `.eventstore` directory is migrated on first start, crash safe and resumable, including a pending 1.x write-ahead log, earlier index layouts and 1.x schema files.
- **`genesisdb convert`**: the same migration as an offline command, with `--dry-run`.
- **`GENESISDB_STORAGE_FORMAT`**: `auto` (default) migrates on start, `keep` opens a 1.x directory in place for inspection.
- **API v2** under `/api/v2` with new route names (`fetch`, `subscribe`, `query`, `types`, `schemas`, `ping`) and administration under `/api/v2/admin` (`audit`, `backup`, `restore`, `erase`, `index`). The 2.x SDKs use it.
- **API v1 frozen**: `/api/v1` keeps every route of 1.1.6 with identical answers, so 1.x clients work unchanged against 2.0. It contains no new names and marks every response with `Deprecation` and a `Link` to its `/api/v2` successor. See [docs/http-api.md](docs/http-api.md#api-v1).
- **Read and write tokens**: `GENESISDB_READ_TOKEN` and `GENESISDB_WRITE_TOKEN` next to the administrator token, with least-privilege authorization on every route of both API versions.
- **TLS**: `GENESISDB_TLS_CERT_FILE` and `GENESISDB_TLS_KEY_FILE`, TLS 1.2 minimum.
- **Optional caps**: `GENESISDB_MAX_REQUEST_BYTES`, `GENESISDB_MAX_RESTORE_BYTES`, `GENESISDB_MAX_READ_EVENTS`, `GENESISDB_MAX_READ_BYTES`, `GENESISDB_MAX_CONCURRENT_REQUESTS` and `GENESISDB_REQUEST_TIMEOUT_SECONDS`. All off by default, a hit cap fails the request explicitly (`413`, `422`, `429`) instead of truncating the result.
- **`hashversion` field**: every new event records the version of its hash algorithm, which binds the predecessor hash into the event hash.
- **Compact index records**: indexes of the current storage format store tab separated records without JSON, less than half the size of the 1.1.6 records, and are read without reflection. Existing indexes are rebuilt on the first start.
- **Faster reads**: `COUNT()` without a payload filter is answered from the index, event records are read in 1 MiB windows instead of one system call each, and `fetch_events` of the MCP endpoint loads only the requested page.
- **Faster commits**: shard files stay open between commits and macOS issues one full device flush per commit instead of three. Batches of 500 events commit about twice as fast (about 12,000 events per second with four clients on an Apple M1 Pro), a batch of 1000 events in about 60 ms.
- **Strict GDBQL parsing**: a misspelled keyword, a repeated clause or text after a value fails with `400` and a message naming the problem. 1.1.6 ignored such words and returned a different result, which also affected `isQueryResultTrue` preconditions.
- **Index self-repair**: a read that finds index data damaged in place rebuilds the indexes from the event logs and retries, instead of answering `500` until a manual `POST /api/v1/index`.
- **Checksummed write-ahead log** with one batch per record and idempotent replay.
- **Erasure audit trail**: `POST /erase` writes a requested and a completed system event.
- **Subscriptions** catch up in bounded batches when a read budget is configured and end with an explicit `event: error` instead of stalling.
- **`genesisdb_*` metrics** next to the `eventstore_*` names (`GENESISDB_LEGACY_METRICS`), `GENESISDB_METRICS_PORT`, OpenTelemetry OTLP export, bounded label cardinality.
- **Status**: `GET /api/v1/status` reports engine version and channel, storage format and readiness.
- **Structured logging** with `log/slog` and `GENESISDB_LOG_LEVEL`.
- **Compatibility suite**: requests recorded against 1.1.6 are replayed and compared byte for byte.
- **Release pipeline**: every release runs all gates and publishes a tag, static binaries with checksums, and a multi-arch container image on Docker Hub and GitHub Packages.

### Changed
- Every Go identifier describes what it does (for example `ReferenceDataEntry`, `VerifyHashChain`, `FindLatestEvent`).
- GDBQL compares typed and exactly, with arbitrary-precision arithmetic. The LINQ-style spelling (`FROM e IN events ... PROJECT INTO`) stays accepted as a compatibility syntax.
- `GROUP BY` results are returned in the order of their first event.
- `POST /api/v1/erase` replaces the whole reference payload object, keys included.
- Every file and directory GenesisDB creates is owner-only (`0600`/`0700`).
- Request bodies, backups and new events must be valid UTF-8.
- The HTTP router is the standard library `http.ServeMux`.

### Removed
- gRPC.

## [1.1.6]

The last release of the closed source 1.x line. GenesisDB 2 opens its data directories and restores its backups.
