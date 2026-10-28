# Compatibility and upgrades

GenesisDB 2 is the open source successor of the 1.x releases and a drop-in replacement for GenesisDB 1.1.6 and earlier: start it on an existing data directory and it migrates that directory, feed it a 1.x backup and it restores it, keep the existing clients and they keep working. The migration is one-way, 1.x cannot open a migrated directory. The 1.x line receives no further updates.

**A backup is mandatory.** As with every migration, a backup before upgrading is not a recommendation but a requirement. Stop 1.x and copy the complete data directory before GenesisDB 2 starts on it for the first time. Without that copy there is no way back to 1.x.

## Upgrading a running installation

1. Stop the 1.x process and copy the complete data directory. This backup is mandatory: there is no conversion back to 1.x, and the copy is the only fallback if anything goes wrong.
2. Set `GENESISDB_AUTH_TOKEN` to a value of at least 32 bytes (`openssl rand -hex 32`). 2.0 enforces this minimum. This is the only configuration change that is required.
3. Set `GENESISDB_HOST=0.0.0.0` if clients connect from other hosts. 2.0 binds to loopback by default, 1.x bound to all interfaces. The container image already sets this.
4. Start 2.0 with the same `GENESISDB_DATA_DIR`. The first start migrates the directory and logs what it did. Nothing else has to be run.

## What the first start migrates

Everything happens before the listener opens, in this order, and every step is crash safe and resumable: a start that is interrupted anywhere finishes the migration on the next start.

- **Pending 1.x write-ahead log.** If 1.x was stopped between writing its WAL and appending the events, the batch is replayed into the event log first, exactly like 1.x itself does on its next start. A torn 1.x WAL (an interrupted write of a batch that was never acknowledged) is discarded, as 1.x does.
- **Event logs.** `events/*.eventstore` become `events/*.gdb` with the `genesisdb` header. The records are copied byte for byte and verified by checksum before a source file is removed. Event IDs, timestamps and hashes do not change, so the hash chain of 1.x remains verifiable and `GET /api/v1/status/audit` passes on migrated data.
- **Indexes.** The fixed-shard indexes of 1.1.6 and the per-key index layout of earlier releases are both replaced by a rebuild from the event logs, because the shorter log header shifts every record offset by one byte. The rebuilt indexes use the compact records of version 3, see [storage format](storage-format.md). Rebuild time is proportional to the store size, about 15 seconds for 500,000 events on a laptop.
- **Schema files.** `schemas/<sanitized type>.json` of 1.x becomes a `schemas/type-<sha256>.json` envelope that carries the exact event type. The type is recovered from the 1.x file name (`_` back to `.`), which is exact for reverse DNS type names such as `io.acme.user-registered`. Keywords outside the validated subset (for example `pattern`, `minimum`, `$schema`) are dropped, so the migrated schema accepts and rejects exactly the events 1.x accepted and rejected. If a schema file cannot be migrated, it stays in place, is still read, and a warning is logged.
- **Format marker.** `format.gdb` is written last. Its presence marks a migrated directory.

A directory in which 1.x and 2.0 log files coexist without a running migration is refused with a clear error, because it means files were copied by hand. Keep one set.

## Backups

`POST /api/v2/admin/restore` (and the 1.x route `/api/v1/backup/restore`) accepts both 1.x backup forms unchanged: the plain event array and the extended `{"events","refs","system"}` object. Restore requires an empty store, as in 1.x. Event IDs and timestamps are preserved, the hash chain is recomputed with the current algorithm.

Backups written by 2.0 add a `schemas` section to the extended form. 1.x restore ignores unknown sections, but 2.0 backups are not meant to be restored into 1.x.

## Behavior that stays the same

- Every 1.x route answers with the same status, content type and body. The compatibility suite replays requests recorded against 1.1.6 and compares byte for byte, except for the intentional differences listed below.
- Response format is NDJSON by default and a JSON array with `x-disable-ndjson`, as before.
- No limit on events per commit, events per store, selection size or request duration. 1.x had none and 2.0 has none by default. Caps are available as opt-in configuration, see the README.
- Subject, type and source handling, single-segment subjects getting a generated ID, the 64 KB event size check, preconditions and the GDBQL surface are unchanged.

## Behavior that changed

Security defaults:

- `GENESISDB_AUTH_TOKEN` must have at least 32 bytes. Optional `GENESISDB_READ_TOKEN` and `GENESISDB_WRITE_TOKEN` provide read-only and read-write roles. Backup, audit, restore, erase, schema registration and index rebuild require the administrator token.
- The CLI binds to `127.0.0.1` instead of all interfaces. Set `GENESISDB_HOST`.
- Optional TLS through `GENESISDB_TLS_CERT_FILE` and `GENESISDB_TLS_KEY_FILE`.
- Requests must contain exactly one JSON document.

Query engine, where 2.0 answers more precisely:

- GDBQL compares values typed and exactly. `1` and `"1"` are different, large integers keep their digits.
- `AND` binds stronger than `OR`, quoted strings may contain keywords, `IN` lists tolerate whitespace around entries.
- A `LIMIT` or `TOP` clause is honored in every position, including the end of the query.
- A query with words that belong to no clause is rejected with `400`. 1.1.6 ignored them or treated them as part of a value, so a typo such as `MAPP COUNT()` returned whole events and `LIMT 5` returned nothing. Valid queries answer exactly as before.
- `GROUP BY` results come in the order of their first event.
- Reads return events in commit order, independent of event timestamps.

Erasure and integrity:

- Erasure (`POST /api/v1/erase`, `POST /api/v2/admin/erase`) replaces whole reference payload objects with `{}`, including their keys. 1.x kept the keys and blanked the values. Both record audit system events, 2.0 records a requested and a completed event.
- New events in a migrated directory carry `hashversion: 3`, which binds the predecessor hash into the event hash. Existing events are unchanged, the audit verifies every event with the algorithm of its record and passes on migrated data.

Removed:

- gRPC and license keys. `GENESISDB_GRPC_ENABLED`, `GENESISDB_GRPC_PORT` and `GENESISDB_LICENSE` are ignored with a warning.

## API versions and clients

GenesisDB 2 serves two API versions side by side:

- `/api/v1` is the API of 1.1.6, frozen. Every route of 1.x exists with the same name, the same token permissions and byte identical answers. It gains no new routes.
- `/api/v2` is the current API with new route names and administration under `/api/v2/admin`. It contains no 1.x names.

| Client | GenesisDB 1.x | GenesisDB 2 |
|---|---|---|
| SDK 1.x (`/api/v1`) | yes | yes, unchanged |
| SDK 2.x (`/api/v2`) | no | yes |

Upgrade the server first, then the clients at any pace. Every `/api/v1` response carries `Deprecation: true` and a `Link` to its `/api/v2` successor, the mapping is in [http-api.md](http-api.md#api-v1). `/api/v1` stays available for the whole 2.x line. Historical `eventstore_*` metric names remain available through `GENESISDB_LEGACY_METRICS=true`.

## Inspecting a 1.x directory without migrating

`GENESISDB_STORAGE_FORMAT=keep` opens a 1.x directory in place and leaves the file names alone. Reads work normally. New events are written without `hashversion`, so the directory remains readable by 1.x. This mode exists for inspection and for the compatibility tests, not for operation.

`genesisdb convert --data-dir <dir> --dry-run` lists what a migration would change, `genesisdb convert --data-dir <dir>` runs it offline against a stopped directory. The server does the same on start. Back up the directory before either, the backup is mandatory.

## Tests

`go test -run Compatibility ./tests` replays the recorded 1.1.6 exchanges on `/api/v1` and on their `/api/v2` successors against a 1.1.6 fixture directory opened with default options, against the same directory kept in the legacy format, against a pre-1.1.6 index layout, and restores the 1.1.6 backup files. It also opens a fixture with a pending 1.1.6 WAL and a 1.1.6 schema file and verifies that the WAL is replayed, the schema is migrated and still enforced, and the hash chain passes.
