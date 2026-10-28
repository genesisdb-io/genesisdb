# Roadmap

What is planned for GenesisDB after 2.0, in rough order. Nothing here is a commitment to a date. Items move up when someone needs them, open an issue if that someone is you.

## 2.1

- **Event signatures.** Ed25519 signature on every event, opt-in through a key file. The hash chain proves the log is consistent, a signature proves who wrote it. Public keys are stored with the data, verified by the audit and by an offline `genesisdb verify` command, key rotation without re-signing history.
- **Reference payload index.** Reads of subjects with reference payloads currently scan the reference log. An index makes them constant time regardless of how many reference payloads the store holds.
- **Backups and audits without blocking writes.** Both only need a stable snapshot, which the index already provides. Commits will no longer wait for a backup to finish.
- **Read-only mode.** A flag that refuses every write, for maintenance windows and for inspecting a copy of a production store.
- **Migration guide.** Importing events from other sources in CloudEvents format: mapping subjects, committing in batches, verifying counts.

## 2.2

- **Streaming backups.** A backup is written to the response as it is read instead of being materialized in memory first, so a backup larger than available memory works.
- **Metrics token.** Optional bearer token for the metrics endpoints.
- **Pre-migration audit.** `genesisdb convert --verify` checks a 1.x directory before converting it.

## Later, on demand

- **Indexes on payload fields.** Declared secondary indexes for `WHERE e.data.field == value`, so reporting queries do not scan.
- **Binary index records.** 2.0 already halved the index records with a tab separated format. A binary posting format would cut index disk usage further, currently about 2.7 times the event log on the measured 500,000 event store.
- **Windows.** The directory lock needs a Windows implementation, everything else compiles.
- **Replication.** A follower that reads the leader's event stream and serves a read-only copy. A warm standby, not consensus.
- **Query language additions.** `NOT`, grouped conditions, `LIKE`, `DISTINCT`, `SKIP`. Each when a use case appears that a projection does not serve better.
- **Packaging.** Install script, Homebrew tap, Helm chart.

## Not yet planned

- **gRPC.** The HTTP API is small and NDJSON streams well.
- **Built-in UI.** The VS Code extension is the UI.
- **Encryption at rest.** The filesystem or volume does this better.
- **Rate limiting.** A reverse proxy does this better. `GENESISDB_MAX_CONCURRENT_REQUESTS` caps concurrency.
- **Multi-tenancy inside one store.** Run one process per tenant.
