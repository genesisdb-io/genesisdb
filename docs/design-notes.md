# Design notes

How GenesisDB 2 protects data, where its limits are, and what the test suite proves. [storage-format.md](storage-format.md) describes the on-disk layout, [SECURITY.md](../SECURITY.md) the deployment boundaries.

## Security and integrity

- The CLI binds to loopback by default, supports TLS (1.2 minimum) and distinguishes administrator, writer and reader tokens. Tokens must be distinct and at least 32 bytes.
- Authorization is identical in API v1 and API v2: every route carries its required role in the route table. Backup, hash audit, restore, erase, schema registration and index rebuild are administrator-only.
- Request and restore body caps, selection budgets, concurrency limits and request deadlines are opt-in (`GENESISDB_MAX_*`, `GENESISDB_REQUEST_TIMEOUT_SECONDS`). By default no commit batch, store, selection, backup or request is limited in size or duration.
- One validation path for every new event, whether it arrives over HTTP, through the embedded store or through the storage engine: subject, type, source and payload shape, size, and valid UTF-8. Request bodies are exactly one JSON value.
- Every new event carries a version-3 hash that binds the predecessor hash, so a chain cannot be re-linked without recomputing every later hash. Events without a `hashversion` field are verified with the algorithm of their record and counted separately by the audit.
- The write-ahead log holds one checksummed batch per record. A torn record is a commit that was never acknowledged and is discarded on start, a complete record is replayed idempotently, a complete but corrupt record stops the start. A pending 1.x write-ahead log is replayed on the first start.
- Indexes are derived data. A read that finds damaged index data rebuilds the indexes from the event logs and retries once, instead of failing every following read until an administrator intervenes. Damaged index data never produces a wrong result: every posting is checked against the event record it points to.
- A storage failure after the write-ahead log was published is sticky: rebuild, restore, erase, reference replacement and system events refuse until a restart has replayed the log. This keeps the in-memory log position and predecessor hash trustworthy.
- Every file and directory GenesisDB creates is owner-only (`0600`/`0700`), independent of the umask.
- 1.x data directories are migrated automatically on first start, including pending write-ahead logs, earlier index layouts and schema files. The migration is one-way.

## Correctness

- Reads, subscriptions, latest-event lookups and restores order events by physical log position. Event timestamps never influence the order, so a clock correction cannot reorder a subject's history.
- GDBQL parses quote-aware, applies `AND` before `OR`, and compares typed and exact: strings, numbers, booleans and null are distinct, numbers use arbitrary precision, sums and averages accumulate exactly. Preconditions use the same code. Query text is capped at 64 KiB and 32 nesting levels.
- Schemas are a strict, documented JSON Schema subset with exact type identities in collision-resistant file names. Registration is exclusive and atomic, validation on commit is serialized with registration.
- Erasure replaces whole reference objects, keys included, writes requested and completed audit events under a neutral subject, and rewrites the reference log once in a streaming pass. It is not bounded by the read budget: a subject larger than one fetch is still erased completely.
- Subscriptions track physical high-water marks instead of delivered IDs. With a read budget they catch up in bounded batches and end the stream with `event: error` after repeated failures instead of stalling.
- Prometheus and OpenTelemetry never use subjects as labels. Event type labels are capped at 256 distinct values, further types count under `other`.

## Commit latency

A durable commit touches the write-ahead log, the event log, up to six index shards and the state journal. The flush strategy keeps the number and cost of device flushes low without changing what is durable when:

- The write-ahead log is written in place into a persistent `pending.wal` with one sync. Clearing it after a durable commit does not sync, a stale batch replays idempotently.
- Index shards are synced concurrently, so the device overlaps their flushes. Shard files stay open between commits (up to one handle per shard, bounded by a quarter of the descriptor limit), so a batch of 500 events does not open and close about a thousand files.
- On Darwin one full device flush (`F_FULLFSYNC`) per commit is enough. The write-ahead log is ordered with `F_BARRIERFSYNC`, the event logs and shards use plain `fsync`, and the journal record that publishes the commit is written with `F_FULLFSYNC`, which drains the device cache including everything handed over before it. A commit is acknowledged only after that flush. A crash before it loses only an unacknowledged commit, the barrier guarantees that the write-ahead log reaches stable storage before the event log, so a partly written batch is always completed by replay. On Linux `fsync` already is the strongest flush and every step uses it.

Measured on Apple M1 Pro over HTTP, one client, durable, median of 20 commits (2 for 1000 events), events of about 300 bytes:

| Batch | Latency |
|---|---|
| 1 event | 11 ms |
| 10 events | 15 ms |
| 100 events | 22 ms |
| 1000 events | 62 ms |

One `F_FULLFSYNC` costs about 4 ms on this machine, so a single event commit is bound by the device, not by GenesisDB. Linux with a server SSD is faster. Batches of 500 with four parallel clients sustain about 12,000 events per second, the seeder in `scripts/seed.py` writes 500,000 realistic events in 47 seconds (10,700 events per second). These are local observations, not guarantees.

### Concurrent commits

Commits that wait at the same moment are written as one group. They share the write-ahead log record, the log appends, the shard syncs and the journal flush, so the cost of the device flush is paid once per group instead of once per commit. A group is written exactly like one client batch with the same events, which is why the storage engine, the index engine, the on-disk formats and recovery are untouched.

- There is no background goroutine and no timer. Commits queue in arrival order, one of the waiting callers writes the group, and the commits that arrive meanwhile form the next group. A single writer takes the same path with a group of one and is not delayed.
- Preconditions are only evaluated against published, durable state. A commit joins a group only if none of its preconditions can observe the unpublished events of the commits before it. A commit with a query precondition, or with a subject precondition on a subject that an earlier commit of the group creates, starts the next group. The outcome equals executing the commits one after another in arrival order.
- A group holds at most 256 commits (`GENESISDB_MAX_COMMITS_PER_GROUP`), 1,024 events (`GENESISDB_MAX_EVENTS_PER_GROUP`) and 8 MiB of payload (`GENESISDB_MAX_BYTES_PER_GROUP`). The limits only split groups: a single commit of any size is written as before, nothing is rejected or truncated.
- Each commit stays atomic and gets its own result. A storage failure reaches every commit of the group it hits, see the README.
- Backups, audits, erasures, rebuilds and schema registrations announce themselves, and group leaders let them take the writer lock first. Otherwise the next group would retake the lock before a waiting administrator request sees it free. The priority is bounded: an administrator request that releases the lock while commits are queued hands the next turn to them, so the following administrator request waits for one commit group. Under load both sides alternate and neither can starve the other. Releases of the writer lock wake its waiters directly, a handover does not wait for a poll.
- The caller that writes a group also evaluates the validation and the preconditions of the other commits in it, and may write a group ahead of its own commit. It only does work that is in front of its own commit in the queue, so this is time it would otherwise spend waiting.

Measured in process on Apple M1 Pro, single-event commits of about 150 bytes on distinct subjects, 1,000 commits per case. The grouped columns are the median of three runs, the columns for one commit per write are one run of the release before group commit. `p99` is the latency of the slowest percent of commits, measured from the call to the acknowledgement:

| Concurrent writers | One commit per write | p99 | Grouped | p99 |
|---|---|---|---|---|
| 1 | 127 commits/s | 13 ms | 131 commits/s | 13 ms |
| 4 | 126 commits/s | 14 ms | 296 commits/s | 26 ms |
| 16 | 123 commits/s | 8.0 s | 826 commits/s | 29 ms |
| 64 | 123 commits/s | 8.0 s | 2,254 commits/s | 45 ms |
| 256 | 112 commits/s | 8.8 s | 4,370 commits/s | 82 ms |

The left columns are the release before group commit, where waiting commits polled for the writer lock and some waited for seconds. Grouping does not make a single commit faster, it lets concurrent commits share the flush and serves them in arrival order. Workloads in which every commit carries a query precondition are still written one commit at a time. `BenchmarkCommitParallel` in `packages/store` repeats the measurement, also with `MaxCommitsPerGroup: 1`. That setting keeps the arrival order of this release, so it is not the left column: with 256 writers it reached about 124 commits per second with a p99 of 2.5 s. Single runs vary, one of three runs with one writer measured 107 commits per second and a p99 of 30 ms.

Over HTTP on the same machine, 600 single-event commits per case: one client 112 commits per second with and without grouping, 16 clients 661 instead of 107 (p99 33 ms instead of 2.4 s), 64 clients 1,770 instead of 99 (p99 58 ms instead of 5.5 s).

In a Linux container on the same machine (Go 1.26.10, overlay filesystem of the container runtime, not a server SSD), grouped against groups of one commit: one writer 498 against 484 commits per second, 16 writers 1,749 against 475, 64 writers 2,533 against 463, 256 writers 3,806 against 471.

The group size of 256 commits was chosen by measurement: with 256 writers it reached about 6,500 commits per second, against about 3,500 with groups of 64 and about 5,800 with groups of 1,024. With 64 writers, groups of 64, 256 and 1,024 were equal within noise. These are local observations, not guarantees.

### Small store

The same machine, a fresh store, events of about 300 bytes on 5,000 subjects below `/order`, one event type. Writes first, then reads on the resulting 24,220 events. Median over HTTP, warm process:

| Operation | Time |
|---|---|
| Commit 1 event | 11.1 ms |
| Commit 10 events | 14.9 ms |
| Commit 100 events | 21.7 ms |
| Commit 1,000 events | 62 ms |
| Commit 20,000 events, batches of 500, 4 clients | 1.7 s (11,974 events per second) |
| Fetch one subject (8 events) | 0.8 ms |
| Fetch `/order` with descendants (24,220 events) | 191 ms |
| Query `COUNT()` over one event type | 11.6 ms |
| Query filter, `ORDER BY` and `TOP 100` | 273 ms |
| Query `GROUP BY` with `SUM` | 334 ms |
| Resident memory afterwards | 85 MiB |

### At 500,000 events

The same machine (Apple M1 Pro, 16 GB, APFS), a store seeded with `scripts/seed.py 500000`: 2 top level subjects, about 160,000 subjects below them, 13 event types, 288 MB of event logs, 780 MB of indexes, 1,043 files in the data directory. The file count does not grow with events or subjects, the index shard count is fixed. Median of 3 runs over HTTP, warm process:

| Operation | Time |
|---|---|
| Cold start until `/ping` answers | 100 ms |
| Fetch one customer (4 events, reference payloads resolved), first touch | 17 ms |
| Fetch one customer, cached | 0.6 ms |
| Commit 1 event | 7 ms |
| Query `COUNT()` over one indexed type (61,340 events) | 26 ms |
| Query `COUNT()` over everything | 0.26 s |
| Query `ORDER BY` a payload field `LIMIT 100` over one indexed type | 0.6 s |
| Query `DESCENDANTS '/' AND` one indexed type, `COUNT()` | 0.6 s |
| Fetch `/customer` with descendants (75,378 events, 38 MB) | 0.8 s |
| Query `GROUP BY` with `SUM` over about 150,000 events | 2.2 s |
| Query `UNDER '/order'` projecting one field (125,723 rows) | 2.6 s |
| Query `GROUP BY` type over everything | 3.0 s |
| Hash chain audit of all 500,000 events | 3.2 s |
| Fetch `/order` with descendants (424,622 events, 216 MB) | 3.7 s |
| Fetch `/` (all 500,000 events) | 4.4 s |
| Query a payload field without an index, full scan (12,767 hits) | 4.8 s |
| Backup (285 MB) | 4.9 s |
| Erase one customer (4 reference payloads) | 0.6 s |
| Rebuild all indexes | 15 s |

`COUNT()` without a payload filter is answered from the index without loading events. Everything that reads payloads loads and decodes each selected event once, which dominates at this size: about 9 microseconds per event. Resident memory: about 20 MB idle, about 1 GB after reading all 500,000 events. Selections and backups materialize their result before sending it, see Limitations.

## Validation

- Security, durability, corruption, concurrency, hash-tampering, schema, budget, permission and compatibility tests run with the race detector on Linux and macOS.
- Fuzz targets cover the query parser, the backup decoder and the commit round trip (`FuzzCommitRoundTrip`: an accepted event is found by ID and exact subject, carries the submitted payload and leaves the chain valid). `FuzzGroupCommitSerialEquivalence` checks that a queue of concurrent commits with subject preconditions has the outcome of executing them one after another. Crashing inputs are kept as regression fixtures.
- Disk-full tests run against a real small volume (`GENESISDB_FULL_VOLUME`, tmpfs in the CI, HFS+ locally): a commit that hits ENOSPC marks the store unhealthy, no acknowledged event is lost, at most the in-flight batch appears after recovery, and a restart on a still full volume either serves every acknowledged event or refuses with ENOSPC when a complete batch awaits replay.
- An opt-in crash test (`make crash`) runs the real server under concurrent HTTP load and stops it over and over with `SIGKILL` or `SIGTERM`. With an externally provided volume it can also cut the power, discarding every write that was not flushed. After each stop it checks every commit against the answer its client received. See CONTRIBUTING.md.
- Pinned audit tools: staticcheck, govulncheck, gitleaks over the full history, Trivy over the container image. The engines import the standard library only, which an architecture test enforces.
- The container runs as UID 10001 with CA certificates and configurable patched base images.

## Limitations

- Backups and selections are materialized in memory, shard and reference scans are linear, and there is no storage-level pagination or streaming aggregate API. Memory use during a full backup or an unindexable query is proportional to the store.
- Cancellation is cooperative, not every filesystem synchronization or restore phase is interruptible.
- Restore validates schema paths and JSON structurally, not every schema's semantics.
- Hash chains do not authenticate an attacker-controlled directory. Erasure does not remove immutable metadata, old backups, snapshots or disk remnants.
- This is a single-node database without replication or consensus.
- Extended workload and power-loss testing are outside the automated gates: the crash test is run by hand, and a power cut needs a volume set up outside the repository. Whether a physical drive honors a flush is not tested. Run the disk-full tests on the deployment filesystem.
