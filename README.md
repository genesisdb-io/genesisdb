<div align="center">
  <a href="https://www.genesisdb.io">
    <img src="assets/genesisdb.png" alt="GenesisDB" width="130" height="130" />
  </a>
</div>
<br>
<p align="center">
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-blue"></a>
  <img alt="Go 1.26+" src="https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white">
  <img alt="CloudEvents native" src="https://img.shields.io/badge/CloudEvents-native-00B7D5">
  <img alt="GDPR ready" src="https://img.shields.io/badge/GDPR-ready-00B7D5">
</p>
<p align="center">
  <a href="https://www.genesisdb.io">genesisdb.io</a>
</p>

## What is it?

An event sourcing database for building event-driven apps, single node, written in Go, with a hash chain and GDPR erasure built in.

- one static binary, no runtime dependencies, standard library only in the engines.
- CloudEvents native: every event is a CloudEvents 1.0 record in a hierarchical subject tree (`/customer/42/address`).
- atomic batch commits with preconditions (`isSubjectNew`, `isSubjectExisting`, `isQueryResultTrue`).
- fetch a subject with all descendants, in commit order, bounded by event IDs.
- live subscriptions over Server-Sent Events.
- GDBQL, a small query language with filters, ordering, grouping, aggregates, projections and subqueries, see [docs/gdbql.md](docs/gdbql.md).
- optional JSON schemas per event type, enforced on commit.
- every event is SHA-256 chained to its predecessor, `GET /api/v2/admin/audit` verifies the whole store.
- personal data as reference payloads: erase it later with one request, the hash chain stays valid, an audit trail is written.
- logical backup and restore, index rebuild, Prometheus and OpenTelemetry metrics.
- built-in MCP endpoint: AI agents connect to `/mcp` with the usual tokens, see [docs/mcp.md](docs/mcp.md).
- embeddable: `store.Open(dir)` gives you the database inside your own Go program, see [docs/engines.md](docs/engines.md).
- drop-in replacement for the closed source 1.x line, see [GenesisDB 2.0 and 1.x](#genesisdb-20-and-1x).

## GenesisDB 2.0 and 1.x

GenesisDB 2.0 is the first open source release and the successor of the closed source 1.x line. It is a drop-in replacement for 1.1.6 and earlier.

| | Compatible with 2.0 |
|---|---|
| 1.x data directories | Yes, migrated automatically on first start |
| 1.x backups | Yes, both backup forms restore unchanged |
| 1.x clients and SDKs | Yes, the 1.x API stays under `/api/v1`, unchanged and frozen |
| 2.0 SDKs against a 1.x server | No, they use `/api/v2` |
| 2.0 data directories or backups in 1.x | No, the migration is one-way |

**A backup is mandatory before upgrading.** As with every migration, stop 1.x and copy the complete data directory before 2.0 starts on it for the first time. Without that copy there is no way back to 1.x.

Four things change for an existing installation:

- `GENESISDB_AUTH_TOKEN` must have at least 32 bytes. Generate one with `openssl rand -hex 32`.
- The server binds to `127.0.0.1` by default. Set `GENESISDB_HOST=0.0.0.0` for remote clients. The container image already does.
- In the container image, the data directory is `/app/data`. Mount volumes there.
- The container runs as user 10001, 1.x ran as root. Before the first start of 2.0 on a volume written by 1.x, hand the files over once, otherwise the migration stops with `permission denied`:
  ```bash
  docker run --rm -v <volume>:/app/data alpine chown -R 10001:10001 /app/data
  ```

Upgrade the server first and move clients to the 2.0 SDKs at your own pace. New features such as stored events returned by commit, upper fetch bounds and typed errors need a 2.0 SDK. Every v1 response carries `Deprecation: true` and a `Link` to its v2 successor. Steps, migration details and the route mapping are in [docs/compatibility.md](docs/compatibility.md).

## Install

### Container

Every release publishes a multi-arch image (linux/amd64, linux/arm64) on Docker Hub and GitHub Packages, the release notes list its digest:

```bash
docker run --rm --name genesisdb \
  -e GENESISDB_AUTH_TOKEN="$(openssl rand -hex 32)" \
  -p 127.0.0.1:8080:8080 \
  -v genesisdb-data:/app/data \
  genesisdb/genesisdb:latest
```

The same image is available as `ghcr.io/genesisdb-io/genesisdb:latest`.

The image runs as UID/GID 10001 and binds to `0.0.0.0` inside the container. A bind-mounted data directory must be writable by that user.

### go install

```bash
go install github.com/genesisdb-io/genesisdb/v2/cmd/genesisdb@latest
```

The installed binary reports the tagged module version.

### From source

```bash
git clone https://github.com/genesisdb-io/genesisdb
cd genesisdb
make build        # produces ./bin/genesisdb
```

Needs Go 1.26.10 or a newer patched release.

### Prebuilt binaries

Every release on the [releases page](https://github.com/genesisdb-io/genesisdb/releases) ships static binaries for Linux and macOS on amd64 and arm64, plus `SHA256SUMS`. Download, verify, `chmod +x`, and drop on your `$PATH`.

## Run

```bash
export GENESISDB_AUTH_TOKEN="$(openssl rand -hex 32)"
genesisdb
```

The server binds to `127.0.0.1:8080`, creates `./data` and prints a welcome box when it is up. The token is required and must have at least 32 bytes. For remote access configure TLS or put a TLS reverse proxy in front, and use separate read and write tokens, see [SECURITY.md](SECURITY.md).

```bash
genesisdb                  # start the server
genesisdb convert          # migrate a stopped 1.x data directory offline, back it up first
genesisdb version
```

## First events

```bash
curl localhost:8080/api/v2/commit \
  -H "Authorization: Bearer $GENESISDB_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"events":[{"source":"app","subject":"/customer/1","type":"customer.created","data":{"name":"Bruce"}}]}'

curl localhost:8080/api/v2/fetch \
  -H "Authorization: Bearer $GENESISDB_AUTH_TOKEN" \
  -d '{"subject":"/customer"}'

curl localhost:8080/api/v2/query \
  -H "Authorization: Bearer $GENESISDB_AUTH_TOKEN" \
  -d '{"query":"STREAM e FROM events WHERE e.subject UNDER '"'"'/customer'"'"' MAP { id: e.id, name: e.data.name }"}'
```

List responses are NDJSON, one event per line. The full API with every route, request body and status code is in [docs/http-api.md](docs/http-api.md). The API of GenesisDB 1.x stays available unchanged under `/api/v1`, so 1.x clients keep working. A [Postman collection](docs/postman/README.md) with a request for every route is in `docs/postman`.

For a store full of realistic data (customers with erasable personal data, orders, payments, shipments) run the seeder against a running instance:

```bash
GENESISDB_AUTH_TOKEN=... python3 scripts/seed.py 250000
```

## Configuration

Everything is configured with environment variables, a `.env` file in the working directory is read if present.

| Variable | Default | Meaning |
|---|---|---|
| `GENESISDB_AUTH_TOKEN` | required | Administrator bearer token, all protected routes |
| `GENESISDB_READ_TOKEN` | disabled | Read operations, not backup or hash audit |
| `GENESISDB_WRITE_TOKEN` | disabled | Read operations plus commit, not administration |
| `GENESISDB_HOST` | `127.0.0.1` | API bind address, container overrides to `0.0.0.0` |
| `GENESISDB_PORT` | `8080` | API port |
| `GENESISDB_TLS_CERT_FILE`, `GENESISDB_TLS_KEY_FILE` | disabled | TLS certificate/key, supply both |
| `GENESISDB_DATA_DIR` | `data` | Exclusively owned data directory |
| `GENESISDB_MAX_REQUEST_BYTES` | `0` (unbounded) | Optional request body cap in bytes |
| `GENESISDB_MAX_RESTORE_BYTES` | `0` (unbounded) | Optional restore body cap in bytes |
| `GENESISDB_MAX_READ_EVENTS` | `0` (unbounded) | Optional cap on selected events per request |
| `GENESISDB_MAX_READ_BYTES` | `0` (unbounded) | Optional cap on selected bytes per request |
| `GENESISDB_MAX_CONCURRENT_REQUESTS` | `0` (unbounded) | Optional cap on active requests including subscriptions |
| `GENESISDB_REQUEST_TIMEOUT_SECONDS` | `0` (none) | Optional deadline for non-streaming requests |
| `GENESISDB_TZ` | system | Event timestamp time zone |
| `GENESISDB_MEMCACHE` | `true` | Set to `false` to disable event caching |
| `GENESISDB_MEMCACHE_SIZE` | `10000` | Cache entry count, not a byte limit |
| `GENESISDB_STORAGE_FORMAT` | `auto` | `auto` migrates a 1.x directory on start (back it up first, the backup is mandatory), `keep` leaves it in the 1.x format for inspection |
| `GENESISDB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `GENESISDB_METRICS` or `GENESISDB_PROMETHEUS_METRICS` | `false` | Enable Prometheus and optional OTLP metrics |
| `GENESISDB_METRICS_PORT` | `2112` | Separate Prometheus listener |
| `GENESISDB_LEGACY_METRICS` | `true` | Also expose historical metric names |
| `GENESISDB_MCP` | `false` | Serve the MCP endpoint for AI agents at `/mcp` |

OTLP export additionally requires `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. Standard `OTEL_*` settings apply. Metrics endpoints are unauthenticated and must remain on a trusted network. Metric cardinality is bounded: subjects are never exported as labels, and at most 256 distinct event types get their own label value.

## Behavior worth knowing

- Requests accept exactly one JSON value in valid UTF-8. Nothing is capped by default: no limit on events per commit, events per store, selection size, backup size or request duration. Fetches, queries and backups always return the complete result. The `GENESISDB_MAX_*` caps are opt-in, a hit cap fails the request explicitly (`413`, `422`, `429`), never with a truncated result.
- Query `LIMIT` and fetch bounds are applied after loading, not as storage-level pagination.
- Backups, hash audits and erasures hold the writer lock: commits wait until they finish, reads continue. Schedule them outside peak write load.
- A failed commit can be durable already. Check state before retrying. Once an ambiguous storage error occurs, the store rejects writes until a restart has replayed the write-ahead log.
- Reads and subscriptions return events in commit order, independent of event timestamps.
- Durability depends on a filesystem and hardware that honor file and directory synchronization. This is not a distributed database and does not provide replication or consensus.

## Embedded use

```go
s, err := store.Open("data", store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition})
if err != nil {
    return err
}
defer s.Close()
err = s.Commit(ctx, []*core.Event{{Source: "app", Subject: "/user/1", Type: "registered", Data: json.RawMessage(`{"name":"Bruce"}`)}})
events, err := s.Fetch(ctx, "/user", store.FetchOptions{})
```

GenesisDB is one Go module with each engine as its own package under `packages/`. Importing one engine compiles only that package and its GenesisDB dependencies. See [docs/engines.md](docs/engines.md).

## Integrity and privacy boundaries

Every event carries a SHA-256 hash that binds its predecessor. Hash chains are not signatures: an attacker with write access to the data directory can rewrite a whole chain or truncate its tail. Keep trusted external checkpoints and independent backups if that threat matters.

Reference-payload erasure replaces the entire reference object with `{}` and records requested/completed audit events. It does not erase immutable metadata, inline payloads, old backups, filesystem snapshots or disk remnants. Do not put personal data in immutable event subjects, types or sources. The reference key that remains is a SHA-256 digest of metadata and original payload, irreversible but confirmable for guessable payloads, see [SECURITY.md](SECURITY.md). Erasure is a building block, not a GDPR compliance guarantee.

## Documentation

- [HTTP API](docs/http-api.md)
- [GDBQL](docs/gdbql.md)
- [MCP endpoint for AI agents](docs/mcp.md)
- [Postman collection](docs/postman/README.md)
- [Embedded engines](docs/engines.md)
- [Storage format and recovery](docs/storage-format.md)
- [Compatibility and upgrade from 1.x](docs/compatibility.md)
- [Security policy](SECURITY.md)
- [Design notes](docs/design-notes.md)
- [Changelog](CHANGELOG.md)
- [Roadmap](ROADMAP.md)

## Development

```sh
make install-tools   # pinned staticcheck, govulncheck and gitleaks
make lint
make test            # all packages, race detector
make fuzz            # bounded fuzz runs: query parser, backup decoder, commit round trip
make vuln            # reachable dependency and standard-library vulnerabilities
make bench           # benchmark smoke runs, not performance guarantees
make compatibility   # replay the recorded 1.1.6 exchanges
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow and the rules. Short fuzz runs, synthetic benchmarks and fault injection do not establish production capacity or exhaustive crash safety. Test your workload and practice backup restoration before production use.

## License

MIT
