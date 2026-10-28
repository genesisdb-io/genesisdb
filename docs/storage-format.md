# Storage format

The event logs under `events/` are the authoritative data. Everything under `indexes/` is derived and can always be rebuilt.

## Data directory

```text
<GENESISDB_DATA_DIR>/
  format.gdb                     format marker, current format only
  events/
    events.gdb                   regular events
    refs.gdb                     reference data (erasable payloads)
    system.gdb                   system events (audit trail of erasures)
  indexes/
    index-state.gdb              state journal, the publication point
    subjects/index.gdb           subject and type catalog
    events-by-subject/00.gdb ... ff.gdb
    events-by-subject-recursive/00.gdb ... ff.gdb
    event-types/00.gdb ... ff.gdb
    latest-event-of-type/00.gdb ... ff.gdb
    events-by-id/00.gdb ... ff.gdb
  schemas/type-<sha256 of exact event type>.json
  wal/pending.wal                batch of the commit in flight
```

## Two formats

| | Legacy (1.1.6 and earlier) | Current |
|---|---|---|
| Extension | `.eventstore` | `.gdb` |
| First line of every log | `eventstore` | `genesisdb` |
| Marker file | none | `format.gdb`: `{"format":"genesisdb","version":4}` |

Readers accept records and header lines of both formats. New current-format records add `hashversion: 3`, a migrated directory is not meant to be opened by 1.x again. File names are defined by package `core/layout`.

The format is detected on start:

1. `format.gdb` with `"conversion":"in-progress"`: an interrupted conversion is finished first.
2. `format.gdb`: current format.
3. `events/events.eventstore`: legacy format.
4. No event log: a new directory in the current format.

Logs of both formats without a running conversion stop the start with an error.

A legacy directory is migrated to the current format on start (`GENESISDB_STORAGE_FORMAT=auto`, the default). A pending 1.x write-ahead log is replayed first, so the last 1.x commit is never lost. `GENESISDB_STORAGE_FORMAT=keep` opens a legacy directory in place without renaming anything, for inspection, new events are then written without `hashversion`. Existing current-format markers are upgraded to version 4. Future format versions are refused. See [compatibility](compatibility.md).

## Migration

The server migrates on start. The same migration is available offline, for example to migrate before the first start or to inspect what would change:

```sh
genesisdb convert --data-dir data --dry-run   # list the files
genesisdb convert --data-dir data             # legacy to current
```

The data directory must not be in use. The migration is designed to resume after interruption, subject to filesystem synchronization guarantees:

1. Replay the write-ahead log and repair an interrupted final record.
2. Write `format.gdb` with `"conversion":"in-progress"` and the target format.
3. For every log: write the new header and the unchanged body to `<name>.<ext>.tmp`, sync, compare the body checksum with the source, rename, sync the directory, remove the source.
4. Finalize `format.gdb`.
5. Rebuild the indexes, because the shorter header shifts every record offset by one byte.

## Logs

Every log starts with its header line followed by one JSON record per line.

- Event logs: `{"event":{...}}` with the fields `source`, `subject`, `type`, `specversion`, `id`, `time`, `datacontenttype`, `predecessorhash`, `data`, `hash`, `ref` (optional), `options`.
- Reference data log: `{"eventHash":"<ref>","refData":{...},"timestamp":"..."}`.
- System event log: like the event log, with its own hash chain and the source `tag:system.genesisdb.io`.

## Hash chain

New current-format events use hash version 3. Their SHA-256 digest binds a version tag, `predecessorhash` and the metadata digest. That metadata includes source, subject, type, specversion, ID, UTC timestamp, content type, data and reference key. The first predecessor is 64 zeros. Verification follows physical log order and checks cancellation. Records without `hashversion` are verified with the algorithm of their record and counted as `LegacyEvents`. Once a chain contains a version-3 record, every later record must be version 3 as well, and a version-3 record is verified with version 3 only.

Reference-backed events keep an immutable `{}` placeholder and reference key. Their mutable reference payloads are deliberately outside the immutable chain, so erasure does not change the event hash. Neither the event chain nor the WAL checksum authenticates a malicious disk writer. Use trusted external checkpoints to detect whole-chain rewriting or tail truncation.

## Commits

1. Prepare IDs, times and hashes.
2. Encode a single version-2 WAL batch with checksum as one line, write it in place into `wal/pending.wal` and sync. The file persists between commits, so no directory entry changes and one sync is enough. On Darwin this sync is `F_BARRIERFSYNC`: it orders the batch before every later write, the full flush of step 5 makes it durable.
3. Append and sync reference data, then the events.
4. Append the postings of every touched shard and sync them concurrently. On Darwin the event logs and shards use plain `fsync` rather than `F_FULLFSYNC`, because the device flush of the journal record in step 5 is the durability barrier that covers them.
5. Append and sync a commit record to the state journal. Only this record publishes the batch.
6. Truncate the write-ahead log to zero bytes, without a sync. A stale batch that survives a crash is replayed idempotently.

A complete WAL batch is recoverable even if the event append did not start. A WAL whose single line is unterminated is a torn write of a commit that was never acknowledged (nothing is appended to the logs before the WAL sync returns), it is discarded on start. Recovery rejects malformed or checksum-invalid complete batches rather than replaying a prefix. A pending WAL of GenesisDB 1.x (one entry per line) is replayed on the first start, exactly as 1.x does, every entry must carry a valid hash, a torn 1.x WAL is discarded. A failed commit may therefore be durable despite its error response. Restart recovery publishes a complete batch or refuses to serve the directory. Do not retry blindly.

Logical backups hold the writer lock while collecting events, reference/system logs and schema files. Auxiliary read errors fail the backup. Backups export events in commit order. Restore uses a durable maintenance marker, keeps the order of the backup (it does not re-sort by timestamp) and recomputes event chains. An interrupted restore can leave the directory deliberately unavailable, preserve it for diagnosis and restore into a separate empty directory from a verified backup. Backups are materialized, not a streaming large-store export API.

Schema files use collision-resistant filenames and contain exact event type identity. Erasure rewrites the reference log through a synced temporary file and records requested/completed system events. Replaced files, filesystem snapshots and old backups may still contain the erased payload bytes.

## Indexes

Five categories are sharded by `SHA-256(key)[0]` into 256 files each. Subject and type pairs use a length-prefixed key. Every posting stores the exact key and the byte location of the event, so hash collisions never return unrelated events and payloads are read by offset.

Every index file starts with a JSON header that names the record version:

```json
{"format":"genesisdb-sharded-index","version":3,"category":"event-types","shard":"0a","shardCount":256}
```

Version 3 is written in the current storage format. Each record is one line of eight tab separated fields: key, subject, type, event ID, log file, offset, length and the first 16 hex digits of the event hash. Subject, type and ID are left empty when they equal the key. Tab, newline, carriage return and backslash are escaped with a backslash. A record is about half the size of a version 2 record and decodes without JSON.

Version 2 is the JSON record of GenesisDB 1.1.6. Directories kept in the legacy storage format (`GENESISDB_STORAGE_FORMAT=keep`) still write it, so 1.x can read them:

```json
{"k":"/user/1","s":"/user/1","t":"registered","id":"...","ts":"...","src":"events.gdb","off":10,"len":321,"h":"<hash>","g":"<commit group>"}
```

Index files of another known record version are derived data. They are rebuilt from the event logs on start, a newer unknown version stops the start without changing anything.

Startup checks index files by size and state journal, which catches torn and missing writes. Bytes changed in place (bit rot, a stray write, a damaged copy) surface while reading. A read that finds an invalid record or a record that does not match its event rebuilds the indexes from the event logs and runs once more, so the request succeeds after the rebuild (about 15 seconds for 500,000 events) and the warning `index corruption detected` is logged. Reads and writes wait during the rebuild. A commit or erase that finds the damage while holding the writer lock fails with `500` without writing and the rebuild follows directly after it. Events are never changed by a rebuild. If the rebuild itself fails, for example because an event log is damaged, the store answers `503` until a restart.

At most 1282 index files exist (5 x 256 shards, the catalog and the state journal), regardless of how many events, subjects or types the store holds. Sharding bounds the number of files, not the work per query: an exact lookup scans its whole shard.

The recursive category posts every event under each ancestor (`/`, `/org`, `/org/acme`, ...), so fetching a subject with its descendants reads one shard.

## Migration of older indexes

Releases before 1.1.6 wrote one index file per subject and type. On the first start those files are removed and the shards are built in place. Index files of the other storage format, left by a conversion, are removed the same way. Unknown files, symlinks or a newer index format stop the start before anything is deleted.
